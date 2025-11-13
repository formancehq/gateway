package plugins

import (
	"bytes"
	"context"
	"fmt"
	"github.com/formancehq/go-libs/v3/auth"
	"github.com/formancehq/go-libs/v3/oidc"
	"github.com/formancehq/go-libs/v3/oidc/client"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	wNats "github.com/ThreeDotsLabs/watermill-nats/v2/pkg/nats"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/xdg-go/scram"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/formancehq/go-libs/v3/logging"
	"github.com/formancehq/go-libs/v3/publish"

	"go.uber.org/zap"
)

const (
	EventVersion   = "v2"
	EventApp       = "gateway"
	EventTypeAudit = "AUDIT"
)

func init() {
	caddy.RegisterModule(Audit{})
	httpcaddyfile.RegisterHandlerDirective("audit", parseAuditCaddyfile)
}

type Audit struct {
	logger    *zap.Logger       `json:"-"`
	bufPool   *sync.Pool        `json:"-"`
	publisher message.Publisher `json:"-"`
	keySet    oidc.KeySet       `json:"-"`

	TopicName      string `json:"topic_name,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	StackID        string `json:"stack_id,omitempty"`
	AuthEnabled    bool   `json:"auth_enabled,omitempty"`
	AuthURL        string `json:"auth_url,omitempty"`
	AuthIssuer     string `json:"auth_issuer,omitempty"`
	AutoProvision  bool   `json:"auto_provision,omitempty"`

	PublisherKafkaBroker           string `json:"publisher_kafka_broker,omitempty"`
	PublisherKafkaEnabled          bool   `json:"publisher_kafka_enabled,omitempty"`
	PublisherKafkaTLSEnabled       bool   `json:"publisher_kafka_tls_enabled,omitempty"`
	PublisherKafkaSASLEnabled      bool   `json:"publisher_kafka_sasl_enabled,omitempty"`
	PublisherKafkaSASLUsername     string `json:"publisher_kafka_sasl_username,omitempty"`
	PublisherKafkaSASLPassword     string `json:"publisher_kafka_sasl_password,omitempty"`
	PublisherKafkaSASLMechanism    string `json:"publisher_kafka_sasl_mechanism,omitempty"`
	PublisherKafkaSASLScramSHASize int    `json:"publisher_kafka_sasl_scram_sha_size,omitempty"`

	PublisherNatsEnabled           bool          `json:"publisher_nats_enabled,omitempty"`
	PublisherNatsURL               string        `json:"publisher_nats_url,omitempty"`
	PublisherNatsClientId          string        `json:"publisher_nats_client_id,omitempty"`
	PublisherNatsMaxReconnects     int           `json:"publisher_nats_max_reconnects,omitempty"`
	PublisherNatsMaxReconnectsWait time.Duration `json:"publisher_nats_max_reconnects_wait,omitempty"`
}

// Implements the caddy.Module interface.
func (Audit) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.audit",
		New: func() caddy.Module { return new(Audit) },
	}
}

func parseAuditCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	a := new(Audit)
	// default value to keep compatibility
	a.AutoProvision = true

	for h.Next() {
		for h.NextBlock(0) {
			key := h.Val()
			switch key {
			case "publisher_kafka_enabled":
				var err error
				a.PublisherKafkaEnabled, err = parseBool(h.Dispenser)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_kafka_enabled: %v", err)
				}

			case "publisher_kafka_broker":
				if !h.AllArgs(&a.PublisherKafkaBroker) {
					return nil, h.Errf("expected one string value for kafka_broker")
				}

			case "publisher_kafka_tls_enabled":
				var err error
				a.PublisherKafkaTLSEnabled, err = parseBool(h.Dispenser)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_kafka_tls_enabled: %v", err)
				}

			case "publisher_kafka_sasl_enabled":
				var err error
				a.PublisherKafkaSASLEnabled, err = parseBool(h.Dispenser)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_kafka_sasl_enabled: %v", err)
				}

			case "publisher_kafka_sasl_username":
				if !h.AllArgs(&a.PublisherKafkaSASLUsername) {
					return nil, h.Errf("expected one string value for publisher kafka sasl username")
				}

			case "publisher_kafka_sasl_password":
				if !h.AllArgs(&a.PublisherKafkaSASLPassword) {
					return nil, h.Errf("expected one string value for publisher kafka sasl password")
				}

			case "publisher_kafka_sasl_mechanism":
				if !h.AllArgs(&a.PublisherKafkaSASLMechanism) {
					return nil, h.Errf("expected one string value for publisher kafka sasl mechanism")
				}

			case "publisher_kafka_sasl_scram_sha_size":
				var publisherKafkaSaslScramShaSize string
				if !h.AllArgs(&publisherKafkaSaslScramShaSize) {
					return nil, h.Errf("expected one boolean value")
				}

				res, err := strconv.ParseInt(publisherKafkaSaslScramShaSize, 10, 32)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_kafka_sasl_scram_sha_size: %v", err)
				}
				a.PublisherKafkaSASLScramSHASize = int(res)

			case "publisher_nats_enabled":
				var err error
				a.PublisherNatsEnabled, err = parseBool(h.Dispenser)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_nats_enabled: %v", err)
				}

			case "publisher_nats_url":
				if !h.AllArgs(&a.PublisherNatsURL) {
					return nil, h.Errf("expected one string value for publisher_nats_url")
				}

			case "publisher_nats_client_id":
				if !h.AllArgs(&a.PublisherNatsClientId) {
					return nil, h.Errf("expected one string value for publisher_nats_client_id")
				}
			case "publisher_nats_max_reconnects":
				var publisherNatsMaxReconnects string
				if !h.AllArgs(&publisherNatsMaxReconnects) {
					return nil, h.Errf("expected one boolean value")
				}

				res, err := strconv.ParseInt(publisherNatsMaxReconnects, 10, 32)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_nats_max_reconnects: %v", err)
				}
				a.PublisherNatsMaxReconnects = int(res)
			case "publisher_nats_max_reconnects_wait":
				var publisherNatsMaxReconnectsWait string
				if !h.AllArgs(&publisherNatsMaxReconnectsWait) {
					return nil, h.Errf("expected one boolean value")
				}
				res, err := time.ParseDuration(publisherNatsMaxReconnectsWait)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_nats_max_reconnects_wait: %v", err)
				}
				a.PublisherNatsMaxReconnectsWait = res
			case "topic_name":
				var topicName string
				if !h.AllArgs(&topicName) {
					return nil, h.Errf("expected one string value")
				}
				a.TopicName = topicName
			case "auto_provision":
				var autoProvision string
				if !h.AllArgs(&autoProvision) {
					return nil, h.Errf("expected one boolean value")
				}
				v, err := strconv.ParseBool(autoProvision)
				if err != nil {
					return nil, h.Errf("failed to parse auto_provision: %v", err)
				}
				a.AutoProvision = v
			case "auth_url":
				if !h.AllArgs(&a.AuthURL) {
					return nil, h.Errf("expected one string value for auth_internal_url")
				}
			case "auth_issuer":
				if !h.AllArgs(&a.AuthIssuer) {
					return nil, h.Errf("expected one string value for expected_issuer")
				}
			case "auth_enabled":
				var err error
				a.AuthEnabled, err = parseBool(h.Dispenser)
				if err != nil {
					return nil, h.Errf("failed to parse auth_enabled: %v", err)
				}
			case "organization_id":
				if !h.AllArgs(&a.OrganizationID) {
					return nil, h.Errf("expected one string value for organization_id")
				}
			case "stack_id":
				if !h.AllArgs(&a.StackID) {
					return nil, h.Errf("expected one string value for stack_id")
				}
			default:
				return nil, h.Errf("unrecognized option: %s", key)
			}
		}
	}

	if a.TopicName == "" {
		return nil, fmt.Errorf("topic_name parameter is required")
	}
	if a.AuthEnabled && a.AuthURL == "" {
		return nil, fmt.Errorf("auth_url parameter is required when auth_enabled is true")
	}

	return a, nil
}

func parseBool(d *caddyfile.Dispenser) (bool, error) {
	var b string
	if !d.AllArgs(&b) {
		return false, d.Errf("expected one boolean value")
	}

	res, err := strconv.ParseBool(b)
	if err != nil {
		return false, d.Errf("expected boolean value")
	}

	return res, nil
}

// Implements the caddy.Provisioner interface.
func (a *Audit) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger(a)
	a.bufPool = &sync.Pool{
		New: func() any {
			return new(bytes.Buffer)
		},
	}

	if a.PublisherKafkaEnabled {
		if err := a.provisionKafkaPublisher(); err != nil {
			return err
		}
	}

	if a.PublisherNatsEnabled {
		if err := a.provisionNatsPublisher(); err != nil {
			return err
		}
	}

	if a.AuthEnabled {
		httpClient := http.DefaultClient
		discovery, err := client.Discover[oidc.DiscoveryConfiguration](ctx, a.AuthURL, httpClient)
		if err != nil {
			a.logger.Error("failed to discover oidc configuration", zap.Error(err))
			return err
		}

		a.keySet = client.NewRemoteKeySet(httpClient, discovery.JwksURI)
	}

	return nil
}

func newNatsPublisherWithConn(conn *nats.Conn, logger watermill.LoggerAdapter, config wNats.PublisherConfig) (*wNats.Publisher, error) {
	return wNats.NewPublisherWithNatsConn(conn, config.GetPublisherPublishConfig(), logger)
}

func (a *Audit) provisionNatsPublisher() error {

	jetStreamConfig := wNats.JetStreamConfig{
		AutoProvision: a.AutoProvision,
		DurablePrefix: "gateway",
	}

	natsOptions := []nats.Option{
		nats.Name(a.PublisherNatsClientId),
		nats.MaxReconnects(a.PublisherNatsMaxReconnects),
		nats.ReconnectWait(a.PublisherNatsMaxReconnectsWait),
		nats.ClosedHandler(func(c *nats.Conn) {
			a.logger.Info("nats connection closed")
			err := caddy.Stop()
			if err != nil {
				a.logger.Error("failed to stop caddy", zap.Error(err))
				panic(err)
			}
			os.Exit(1)
		}),
	}

	publisherConfig := wNats.PublisherConfig{
		URL:               a.PublisherNatsURL,
		NatsOptions:       natsOptions,
		JetStream:         jetStreamConfig,
		Marshaler:         &wNats.NATSMarshaler{},
		SubjectCalculator: wNats.DefaultSubjectCalculator,
	}

	conn, err := publish.NewNatsConn(
		publisherConfig,
	)
	if err != nil {
		a.logger.Error("failed to create nats connection", zap.Error(err))
		return err
	}

	a.publisher, err = newNatsPublisherWithConn(
		conn,
		logging.NewZapLoggerAdapter(
			a.logger,
		),
		publisherConfig,
	)

	if err != nil {
		a.logger.Error("failed to create nats publisher", zap.Error(err))
		return err
	}

	return nil
}

func (a *Audit) provisionKafkaPublisher() error {

	options := []publish.SaramaOption{
		publish.WithSASLCredentials(
			a.PublisherKafkaSASLUsername,
			a.PublisherKafkaSASLPassword,
		),
	}

	if a.PublisherKafkaTLSEnabled {
		options = append(options, publish.WithTLS())
	}

	if a.PublisherKafkaSASLEnabled {
		options = append(options, publish.WithSASLMechanism(sarama.SASLMechanism(a.PublisherKafkaSASLMechanism)))
		options = append(options,
			publish.WithSASLScramClient(func() sarama.SCRAMClient {
				var fn scram.HashGeneratorFcn
				switch a.PublisherKafkaSASLScramSHASize {
				case 512:
					fn = publish.SHA512
				case 256:
					fn = publish.SHA256
				default:
					panic("sha size not handled")
				}
				return &publish.XDGSCRAMClient{
					HashGeneratorFcn: fn,
				}
			}),
		)
	}

	var err error
	a.publisher, err = publish.NewKafkaPublisher(
		logging.NewZapLoggerAdapter(
			a.logger,
		),
		publish.NewSaramaConfig(
			"gateway",
			sarama.V1_0_0_0,
			options...),
		kafka.DefaultMarshaler{},
		a.PublisherKafkaBroker,
	)

	if err != nil {
		a.logger.Error("failed to create kafka publisher", zap.Error(err))
		return err
	}

	return nil
}

func (a Audit) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return err
		}
	}

	if len(body) > 0 {
		_ = r.Body.Close()
		// Restore the io.ReadCloser to its original state
		r.Body = io.NopCloser(bytes.NewBuffer(body))
	}

	buf := a.bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer a.bufPool.Put(buf)

	rww := NewResponseWriterWrapper(w, buf)
	if err := next.ServeHTTP(rww, r.Clone(r.Context())); err != nil {
		return err
	}

	response := NewHttpResponse(
		*rww.statusCode,
		rww.Header(),
		rww.body.String(),
	)

	// Extract trace ID from OpenTelemetry context
	traceID := extractTraceID(r.Context())

	// Extract IP address from request
	ipAddress := extractIPAddress(r)

	// Remove response body for sensitive endpoints
	if r.URL.Path == "/api/auth/oauth/token" {
		response.Body = ""
	}

	var (
		claims               *oidc.AccessTokenClaims
		tokenValidationError error
	)

	if a.AuthEnabled {
		issuer := a.AuthIssuer
		if issuer == "" {
			issuer = a.AuthURL
		}
		claims, tokenValidationError = auth.ClaimsFromRequest(r, issuer, a.keySet)
	}

	requestHeaders := r.Header.Clone()
	// Remove Authorization header from request headers
	requestHeaders.Del("Authorization")

	payload := Payload{
		ID:      uuid.New().String(),
		TraceID: traceID,
		Actor: Actor{
			Claims: claims,
			TokenValidationError: func() string {
				if tokenValidationError != nil {
					if errors.Is(tokenValidationError, auth.ErrNoAuthorizationHeader) {
						return ""
					}
					return tokenValidationError.Error()
				}
				return ""
			}(),
			OrganizationID: a.OrganizationID,
			StackID:        a.StackID,
			IPAddress:      ipAddress,
		},
		HTTP: HTTP{
			Request: HttpRequest{
				Method: r.Method,
				Path:   r.URL.Path,
				Host:   r.Host,
				Header: requestHeaders,
				Body: func() string {
					if len(body) > 0 {
						return string(body)
					}
					return ""
				}(),
			},
			Response: response,
		},
	}

	if err := a.publisher.Publish(
		a.TopicName,
		publish.NewMessage(
			r.Context(),
			publish.EventMessage{
				Date:    time.Now().UTC(),
				App:     EventApp,
				Version: EventVersion,
				Type:    EventTypeAudit,
				Payload: payload,
			},
		),
	); err != nil {
		a.logger.Error(fmt.Errorf("failed to publish audit message: %v", err).Error())
	}

	return nil
}

// Interface Guards
var (
	_ caddy.Provisioner           = (*Audit)(nil)
	_ caddy.Module                = (*Audit)(nil)
	_ caddyhttp.MiddlewareHandler = (*Audit)(nil)
)

//------------------------------------------------------------------------------

// ResponseWriterWrapper is a wrapper for the http.ResponseWriter, it captures
// the response body and status code to be used in the audit log.
type ResponseWriterWrapper struct {
	http.ResponseWriter
	body       *bytes.Buffer
	statusCode *int
}

// NewResponseWriterWrapper static function creates a wrapper for the
// http.ResponseWriter
func NewResponseWriterWrapper(w http.ResponseWriter, buf *bytes.Buffer) ResponseWriterWrapper {
	statusCode := 200
	return ResponseWriterWrapper{
		ResponseWriter: w,
		body:           buf,
		statusCode:     &statusCode, // Default status code
	}
}

func (rww ResponseWriterWrapper) Write(buf []byte) (int, error) {
	rww.body.Write(buf)
	return rww.ResponseWriter.Write(buf)
}

// Header function overwrites the http.ResponseWriter Header() function
func (rww ResponseWriterWrapper) Header() http.Header {
	return rww.ResponseWriter.Header()

}

// WriteHeader function overwrites the http.ResponseWriter WriteHeader() function
func (rww ResponseWriterWrapper) WriteHeader(statusCode int) {
	(*rww.statusCode) = statusCode
	rww.ResponseWriter.WriteHeader(statusCode)
}

type HttpRequest struct {
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Host   string      `json:"host"`
	Header http.Header `json:"header"`
	Body   string      `json:"body,omitempty"`
}

type HttpResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       string      `json:"body,omitempty"`
}

func NewHttpResponse(
	statusCode int,
	headers http.Header,
	body string,
) HttpResponse {
	return HttpResponse{
		StatusCode: statusCode,
		Headers:    headers,
		Body:       body,
	}
}

type Actor struct {
	Claims               *oidc.AccessTokenClaims `json:"claims,omitempty"`
	TokenValidationError string                  `json:"token_validation_error,omitempty"`
	OrganizationID       string                  `json:"organization_id"`
	StackID              string                  `json:"stack_id"`
	IPAddress            string                  `json:"ip_address"`
}

type HTTP struct {
	Request  HttpRequest  `json:"request"`
	Response HttpResponse `json:"response"`
}

type Payload struct {
	ID      string `json:"id"`
	TraceID string `json:"trace_id"`
	Actor   Actor  `json:"actor"`
	HTTP    HTTP   `json:"http"`
}

// extractIPAddress extracts the client IP address from HTTP request
// Priority: X-Forwarded-For > X-Real-IP > RemoteAddr
func extractIPAddress(r *http.Request) string {
	// Try X-Forwarded-For first
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		return strings.TrimSpace(ips[0])
	}

	// Try X-Real-IP
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fallback to RemoteAddr (remove port)
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip != "" {
		return ip
	}
	return r.RemoteAddr
}

// extractTraceID extracts the OpenTelemetry trace ID from context
func extractTraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		return span.SpanContext().TraceID().String()
	}
	return ""
}

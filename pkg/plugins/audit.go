package plugins

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
	"github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	wNats "github.com/ThreeDotsLabs/watermill-nats/v2/pkg/nats"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/nats-io/nats.go"
	"github.com/xdg-go/scram"
	"go.uber.org/zap"

	"github.com/formancehq/go-libs/v5/pkg/audit"
	"github.com/formancehq/go-libs/v5/pkg/audit/httpaudit"
	v5oidc "github.com/formancehq/go-libs/v5/pkg/authn/oidc"
	v5client "github.com/formancehq/go-libs/v5/pkg/authn/oidc/client"
	"github.com/formancehq/go-libs/v5/pkg/messaging/publish"
	logging "github.com/formancehq/go-libs/v5/pkg/observe/log"
)

const EventApp = "gateway"

func init() {
	caddy.RegisterModule(Audit{})
	httpcaddyfile.RegisterHandlerDirective("audit", parseAuditCaddyfile)
}

type Audit struct {
	logger     *zap.Logger                     `json:"-"`
	publisher  message.Publisher               `json:"-"`
	natsConn   *nats.Conn                      `json:"-"`
	closing    *atomic.Bool                    `json:"-"`
	middleware func(http.Handler) http.Handler `json:"-"`

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

func (Audit) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.audit",
		New: func() caddy.Module { return new(Audit) },
	}
}

func parseAuditCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	a := new(Audit)
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
				var v string
				if !h.AllArgs(&v) {
					return nil, h.Errf("expected one integer value")
				}
				res, err := strconv.ParseInt(v, 10, 32)
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
				var v string
				if !h.AllArgs(&v) {
					return nil, h.Errf("expected one integer value")
				}
				res, err := strconv.ParseInt(v, 10, 32)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_nats_max_reconnects: %v", err)
				}
				a.PublisherNatsMaxReconnects = int(res)
			case "publisher_nats_max_reconnects_wait":
				var v string
				if !h.AllArgs(&v) {
					return nil, h.Errf("expected one duration value")
				}
				res, err := time.ParseDuration(v)
				if err != nil {
					return nil, h.Errf("failed to parse publisher_nats_max_reconnects_wait: %v", err)
				}
				a.PublisherNatsMaxReconnectsWait = res
			case "topic_name":
				if !h.AllArgs(&a.TopicName) {
					return nil, h.Errf("expected one string value")
				}
			case "auto_provision":
				var v string
				if !h.AllArgs(&v) {
					return nil, h.Errf("expected one boolean value")
				}
				b, err := strconv.ParseBool(v)
				if err != nil {
					return nil, h.Errf("failed to parse auto_provision: %v", err)
				}
				a.AutoProvision = b
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

func (a *Audit) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger(a)
	a.closing = &atomic.Bool{}

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

	opts := []audit.Option{
		audit.WithOrganizationID(a.OrganizationID),
		audit.WithStackID(a.StackID),
	}

	if a.AuthEnabled {
		httpClient := http.DefaultClient
		issuer := a.AuthIssuer
		if issuer == "" {
			issuer = a.AuthURL
		}

		discovery, err := v5client.Discover[v5oidc.DiscoveryConfiguration](ctx, a.AuthURL, httpClient)
		if err != nil {
			a.logger.Error("failed to discover oidc configuration", zap.Error(err))
			return err
		}

		keySet := v5client.NewRemoteKeySet(httpClient, discovery.JwksURI)
		opts = append(opts, audit.WithAuth(map[string]v5oidc.KeySet{issuer: keySet}))
	}

	a.middleware = httpaudit.Middleware(a.publisher, a.TopicName, EventApp, opts,
		httpaudit.WithEnabled(true),
		httpaudit.WithSensitivePaths("/api/auth/oauth/token"),
	)

	return nil
}

func (a Audit) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	var handlerErr error

	a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerErr = next.ServeHTTP(w, r)
	})).ServeHTTP(w, r)

	return handlerErr
}

func (a *Audit) Cleanup() error {
	a.closing.Store(true)
	if a.publisher != nil {
		_ = a.publisher.Close()
	}
	if a.natsConn != nil {
		a.natsConn.Close()
	}
	return nil
}

// Interface Guards
var (
	_ caddy.Provisioner           = (*Audit)(nil)
	_ caddy.Module                = (*Audit)(nil)
	_ caddy.CleanerUpper          = (*Audit)(nil)
	_ caddyhttp.MiddlewareHandler = (*Audit)(nil)
)

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
			if a.closing.Load() {
				return
			}
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

	var err error
	a.natsConn, err = publish.NewNatsConn(publisherConfig)
	if err != nil {
		a.logger.Error("failed to create nats connection", zap.Error(err))
		return err
	}

	a.publisher, err = publish.NewNatsPublisherWithConn(
		a.natsConn,
		logging.NewZapLoggerAdapter(a.logger),
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
		logging.NewZapLoggerAdapter(a.logger),
		publish.NewSaramaConfig("gateway", sarama.V1_0_0_0, options...),
		kafka.DefaultMarshaler{},
		a.PublisherKafkaBroker,
	)
	if err != nil {
		a.logger.Error("failed to create kafka publisher", zap.Error(err))
		return err
	}

	return nil
}

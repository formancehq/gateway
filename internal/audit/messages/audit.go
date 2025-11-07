package messages

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/trace"

	"github.com/formancehq/go-libs/publish"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	EventVersion   = "v2"
	EventApp       = "gateway"
	EventTypeAudit = "AUDIT"
)

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
	Identity       string `json:"identity"`
	OrganizationID string `json:"organization_id"`
	StackID        string `json:"stack_id"`
	IPAddress      string `json:"ip_address"`
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

// parseStackEnv parses the STACK environment variable format "ORGANIZATIONID-STACKID"
func parseStackEnv(stack string) (organizationID, stackID string) {
	if stack == "" {
		return "", ""
	}

	parts := strings.SplitN(stack, "-", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}

	// If no dash, consider as stack_id only
	return "", stack
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

func NewAuditMessagePayload(
	ctx context.Context,
	r *http.Request,
	logger *zap.Logger,
	request HttpRequest,
	response HttpResponse,
) publish.EventMessage {
	// Extract identity from JWT token
	identity := ""
	if request.Header != nil {
		if authorizationHeader := request.Header.Get("Authorization"); authorizationHeader != "" && strings.HasPrefix(strings.ToLower(authorizationHeader), "bearer ") {
			tokenString := strings.Replace(strings.Replace(authorizationHeader, "Bearer ", "", 1), "bearer ", "", 1)
			token, _, err := new(jwt.Parser).ParseUnverified(tokenString, jwt.MapClaims{})
			if err != nil {
				logger.Error(fmt.Sprintf("error for Parse %s", err))
			}
			if token != nil {
				if claims, ok := token.Claims.(jwt.MapClaims); ok {
					identity = fmt.Sprint(claims["sub"])
				} else {
					logger.Error(fmt.Sprintf("error get claims JWT token: %s", err))
				}
			}
		}

		request.Header.Del("Authorization")
	}

	// Parse STACK environment variable (format: "ORGANIZATIONID-STACKID")
	stackEnv := os.Getenv("STACK")
	organizationID, stackID := parseStackEnv(stackEnv)

	// Extract trace ID from OpenTelemetry context
	traceID := extractTraceID(ctx)

	// Extract IP address from request
	ipAddress := extractIPAddress(r)

	// Remove response body for sensitive endpoints
	if request.Path == "/api/auth/oauth/token" {
		response.Body = ""
	}

	payload := Payload{
		ID:      uuid.New().String(),
		TraceID: traceID,
		Actor: Actor{
			Identity:       identity,
			OrganizationID: organizationID,
			StackID:        stackID,
			IPAddress:      ipAddress,
		},
		HTTP: HTTP{
			Request:  request,
			Response: response,
		},
	}

	return publish.EventMessage{
		Date:    time.Now().UTC(),
		App:     EventApp,
		Version: EventVersion,
		Type:    EventTypeAudit,
		Payload: payload,
	}
}

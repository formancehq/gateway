package messages

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/formancehq/go-libs/v3/oidc"
	"github.com/formancehq/go-libs/v3/publish"
	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

const (
	EventVersion   = "v2"
	EventApp       = "gateway"
	EventTypeAudit = "AUDIT"
)

var (
	keySetCache     oidc.KeySet
	keySetCacheLock sync.RWMutex
	keySetOnce      sync.Once
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

type Authentication struct {
	Method   string `json:"method"`
	Verified bool   `json:"verified"`
	Reason   string `json:"reason,omitempty"`
}

type Actor struct {
	Identity       string          `json:"identity"`
	Authentication *Authentication `json:"authentication,omitempty"`
	OrganizationID string          `json:"organization_id"`
	StackID        string          `json:"stack_id"`
	IPAddress      string          `json:"ip_address"`
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

// initKeySet initializes the OIDC key set from the issuer if not already done
func initKeySet(ctx context.Context, issuerURL string, logger *zap.Logger) {
	keySetOnce.Do(func() {
		// Discover OIDC configuration
		config, err := oidc.Discover(ctx, issuerURL, oidc.DiscoveryEndpoint)
		if err != nil {
			logger.Error("failed to discover OIDC configuration", zap.Error(err))
			return
		}

		// Fetch the JWKS from the JWKS URI
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.JwksURI, nil)
		if err != nil {
			logger.Error("failed to create JWKS request", zap.Error(err))
			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			logger.Error("failed to fetch JWKS", zap.Error(err))
			return
		}
		defer func() {
			if closeErr := resp.Body.Close(); closeErr != nil {
				logger.Error("failed to close response body", zap.Error(closeErr))
			}
		}()

		if resp.StatusCode != http.StatusOK {
			logger.Error("failed to fetch JWKS", zap.Int("status_code", resp.StatusCode))
			return
		}

		// Decode the JWKS
		var jwks struct {
			Keys []map[string]interface{} `json:"keys"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
			logger.Error("failed to decode JWKS", zap.Error(err))
			return
		}

		// Convert to jose.JSONWebKey
		keys := make([]jose.JSONWebKey, 0, len(jwks.Keys))
		for _, keyData := range jwks.Keys {
			keyBytes, err := json.Marshal(keyData)
			if err != nil {
				continue
			}
			var key jose.JSONWebKey
			if err := key.UnmarshalJSON(keyBytes); err != nil {
				continue
			}
			keys = append(keys, key)
		}

		if len(keys) == 0 {
			logger.Error("no valid keys found in JWKS")
			return
		}

		// Create static key set
		ks := oidc.NewStaticKeySet(keys...)

		keySetCacheLock.Lock()
		keySetCache = ks
		keySetCacheLock.Unlock()
	})
}

// verifyJWT attempts to verify the JWT token signature and expiration
// Returns (verified bool, reason string) where reason explains why verification failed
// This function does not throw errors, it simply returns false with a reason on any failure
func verifyJWT(ctx context.Context, tokenString string, issuerURL string, logger *zap.Logger) (bool, string) {
	// If no issuer provided, cannot verify
	if issuerURL == "" {
		return false, "no_issuer_configured"
	}

	// Initialize key set if not already done
	initKeySet(ctx, issuerURL, logger)

	keySetCacheLock.RLock()
	ks := keySetCache
	keySetCacheLock.RUnlock()

	if ks == nil {
		// Key set not initialized, cannot verify
		return false, "keyset_unavailable"
	}

	// Parse the JWT as a JWS (support common algorithms)
	jws, err := jose.ParseSigned(tokenString, []jose.SignatureAlgorithm{
		jose.RS256, jose.RS384, jose.RS512,
		jose.PS256, jose.PS384, jose.PS512,
		jose.ES256, jose.ES384, jose.ES512,
		jose.EdDSA,
	})
	if err != nil {
		// Failed to parse JWT
		return false, "invalid_format"
	}

	// Verify the JWT signature using the key set
	_, err = ks.VerifySignature(ctx, jws)
	if err != nil {
		// Verification failed (could be invalid signature, expired token, etc.)
		// Common errors: signature mismatch, expired token, invalid claims
		if strings.Contains(err.Error(), "exp") {
			return false, "expired_token"
		}
		if strings.Contains(err.Error(), "signature") {
			return false, "invalid_signature"
		}
		return false, "verification_failed"
	}

	return true, ""
}

func NewAuditMessagePayload(
	ctx context.Context,
	r *http.Request,
	logger *zap.Logger,
	issuer string,
	authInternalURL string,
	organizationID string,
	stackID string,
	request HttpRequest,
	response HttpResponse,
) publish.EventMessage {
	// Extract identity from JWT token and verify it
	identity := ""
	var authentication *Authentication
	if request.Header != nil {
		if authorizationHeader := request.Header.Get("Authorization"); authorizationHeader != "" && strings.HasPrefix(strings.ToLower(authorizationHeader), "bearer ") {
			tokenString := strings.Replace(strings.Replace(authorizationHeader, "Bearer ", "", 1), "bearer ", "", 1)

			// Verify JWT signature and expiration using the internal auth URL
			verified, reason := verifyJWT(ctx, tokenString, authInternalURL, logger)
			authentication = &Authentication{
				Method:   "jwt",
				Verified: verified,
				Reason:   reason,
			}

			// Extract identity from JWT claims (whether verified or not)
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
			Authentication: authentication,
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

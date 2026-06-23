package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v5oidc "github.com/formancehq/go-libs/v5/pkg/authn/oidc"
)

func TestDiscoverAuthConfigurationUsesAuthIssuerWithAuthURLDiscovery(t *testing.T) {
	expectedIssuer := "https://stack.example.test/api/auth"
	var requestedPath string

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(v5oidc.DiscoveryConfiguration{
			Issuer:  expectedIssuer,
			JwksURI: expectedIssuer + "/keys",
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer authServer.Close()

	discovery, err := discoverAuthConfiguration(
		context.Background(),
		authServer.URL,
		expectedIssuer,
		authServer.Client(),
	)
	if err != nil {
		t.Fatalf("discover auth configuration: %v", err)
	}

	if requestedPath != v5oidc.DiscoveryEndpoint {
		t.Fatalf("expected discovery request path %q, got %q", v5oidc.DiscoveryEndpoint, requestedPath)
	}
	if discovery.Issuer != expectedIssuer {
		t.Fatalf("expected discovered issuer %q, got %q", expectedIssuer, discovery.Issuer)
	}
}

func TestDiscoverAuthConfigurationSupportsAuthURLAsIssuer(t *testing.T) {
	var authServer *httptest.Server
	authServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(v5oidc.DiscoveryConfiguration{
			Issuer:  authServer.URL,
			JwksURI: authServer.URL + "/keys",
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer authServer.Close()

	discovery, err := discoverAuthConfiguration(
		context.Background(),
		authServer.URL,
		authServer.URL,
		authServer.Client(),
	)
	if err != nil {
		t.Fatalf("discover auth configuration: %v", err)
	}

	if discovery.Issuer != authServer.URL {
		t.Fatalf("expected discovered issuer %q, got %q", authServer.URL, discovery.Issuer)
	}
}

func TestInternalAuthEndpointURLRewritesIssuerEndpointToAuthURL(t *testing.T) {
	got, err := internalAuthEndpointURL(
		"https://stack.example.test/api/auth/keys",
		"https://stack.example.test/api/auth",
		"http://auth:8080",
	)
	if err != nil {
		t.Fatalf("rewrite auth endpoint url: %v", err)
	}

	if got != "http://auth:8080/keys" {
		t.Fatalf("expected internal jwks url %q, got %q", "http://auth:8080/keys", got)
	}
}

func TestInternalAuthEndpointURLPreservesAuthURLPath(t *testing.T) {
	got, err := internalAuthEndpointURL(
		"https://stack.example.test/api/auth/keys",
		"https://stack.example.test/api/auth",
		"http://gateway:8080/api/auth",
	)
	if err != nil {
		t.Fatalf("rewrite auth endpoint url: %v", err)
	}

	if got != "http://gateway:8080/api/auth/keys" {
		t.Fatalf("expected internal jwks url %q, got %q", "http://gateway:8080/api/auth/keys", got)
	}
}

func TestInternalAuthEndpointURLKeepsExternalEndpointWhenNotUnderIssuer(t *testing.T) {
	endpoint := "https://keys.example.test/jwks"
	got, err := internalAuthEndpointURL(
		endpoint,
		"https://stack.example.test/api/auth",
		"http://auth:8080",
	)
	if err != nil {
		t.Fatalf("rewrite auth endpoint url: %v", err)
	}

	if got != endpoint {
		t.Fatalf("expected endpoint %q, got %q", endpoint, got)
	}
}

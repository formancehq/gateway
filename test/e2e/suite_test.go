//go:build it

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/formancehq/stack/components/gateway/pkg/plugins"

	logging "github.com/formancehq/go-libs/v5/pkg/observe/log"
	"github.com/formancehq/go-libs/v5/pkg/testing/deferred"
	"github.com/formancehq/go-libs/v5/pkg/testing/platform/natstesting"
	"github.com/formancehq/go-libs/v5/pkg/audit"
	"github.com/nats-io/nats.go"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gateway E2E Suite")
}

var (
	natsServer = deferred.New[*natstesting.NatsServer]()
	debug      = os.Getenv("DEBUG") == "true"
	logger     = logging.NewDefaultLogger(GinkgoWriter, debug, false, false)
)

var _ = SynchronizedBeforeSuite(func(specContext SpecContext) []byte {
	deferred.RegisterRecoverHandler(GinkgoRecover)

	natsServer.LoadAsync(func() (*natstesting.NatsServer, error) {
		By("Initializing NATS server")
		ret := natstesting.CreateServer(GinkgoT(), debug, logger)
		By("NATS address: " + ret.ClientURL())
		return ret, nil
	})

	By("Waiting for NATS")
	_, err := natsServer.Wait(specContext)
	Expect(err).To(BeNil())

	data, err := json.Marshal(natsServer.GetValue())
	Expect(err).To(BeNil())
	return data
}, func(data []byte) {
	select {
	case <-natsServer.Done():
		return
	default:
	}
	ns := &natstesting.NatsServer{}
	Expect(json.Unmarshal(data, ns)).To(Succeed())
	natsServer.SetValue(ns)
})

// freePort finds an available TCP port.
func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	ExpectWithOffset(1, err).To(BeNil())
	port := l.Addr().(*net.TCPAddr).Port
	ExpectWithOffset(1, l.Close()).To(Succeed())
	return port
}

// backendRecorder holds the mock backend server and the last request it received.
type backendRecorder struct {
	Server      *httptest.Server
	mu          sync.Mutex
	lastReq     *http.Request
}

func (b *backendRecorder) LastRequest() *http.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastReq
}

func (b *backendRecorder) recordRequest(r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastReq = r.Clone(r.Context())
}

// startBackend creates a mock HTTP backend that records incoming requests.
func startBackend() *backendRecorder {
	rec := &backendRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.recordRequest(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	DeferCleanup(rec.Server.Close)
	return rec
}

// startGateway starts a Caddy server with the audit plugin configured to
// publish to the given NATS URL, proxying to the given backend address.
// It returns the gateway base URL (e.g. "http://127.0.0.1:PORT").
func startGateway(natsURL, backendAddr, topicName string) string {
	port := freePort()

	caddyfileContent := fmt.Sprintf(`
{
	order audit after encode
	admin off
}

:%d {
	audit {
		topic_name %s
		organization_id test-org
		stack_id test-stack
		auto_provision true

		publisher_kafka_enabled false

		publisher_nats_enabled true
		publisher_nats_url %s
		publisher_nats_client_id gateway-test
	}

	handle /api/* {
		reverse_proxy %s
	}

	handle /health {
		respond "OK" 200
	}
}
`, port, topicName, natsURL, backendAddr)

	adapter := caddyconfig.GetAdapter("caddyfile")
	Expect(adapter).NotTo(BeNil(), "caddyfile adapter not registered")

	cfgJSON, _, err := adapter.Adapt([]byte(caddyfileContent), nil)
	Expect(err).To(BeNil())

	Expect(caddy.Load(cfgJSON, false)).To(Succeed())

	DeferCleanup(func() {
		Expect(caddy.Stop()).To(Succeed())
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Wait for the server to be ready.
	Eventually(func() error {
		resp, err := http.Get(baseURL + "/health")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}).WithTimeout(5 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())

	return baseURL
}

// subscribeToAuditTopic subscribes to the NATS topic and returns a channel of raw messages.
func subscribeToAuditTopic(natsURL, topicName string) chan *nats.Msg {
	conn, err := nats.Connect(natsURL)
	Expect(err).To(BeNil())
	DeferCleanup(conn.Close)

	ch := make(chan *nats.Msg, 64)
	sub, err := conn.Subscribe(topicName, func(msg *nats.Msg) {
		ch <- msg
	})
	Expect(err).To(BeNil())
	DeferCleanup(sub.Unsubscribe)

	return ch
}

// parseAuditPayload extracts the audit.Payload from a raw NATS message.
// The message payload is a Watermill EventMessage JSON containing a nested Payload.
func parseAuditPayload(msg *nats.Msg) audit.Payload {
	var envelope struct {
		Payload audit.Payload `json:"payload"`
	}
	ExpectWithOffset(1, json.Unmarshal(msg.Data, &envelope)).To(Succeed())
	return envelope.Payload
}

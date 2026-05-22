//go:build it

package e2e_test

import (
	"net/http"
	"strings"
	"time"

	"github.com/formancehq/go-libs/v5/pkg/testing/deferred"
	"github.com/formancehq/go-libs/v5/pkg/testing/platform/natstesting"
	"github.com/formancehq/go-libs/v5/pkg/audit"
	"github.com/nats-io/nats.go"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Audit", func() {
	var topicName = "audit-e2e"

	Context("basic audit flow", Ordered, func() {
		var (
			backend    *backendRecorder
			gatewayURL string
			messages   chan *nats.Msg
		)

		BeforeAll(func(specContext SpecContext) {
			natsURL := deferred.Map(natsServer, (*natstesting.NatsServer).ClientURL)
			url, err := natsURL.Wait(specContext)
			Expect(err).To(BeNil())

			backend = startBackend()
			gatewayURL = startGateway(url, backend.Server.Listener.Addr().String(), topicName)
			messages = subscribeToAuditTopic(url, topicName)
		})

		When("a request is proxied through the gateway", func() {
			var resp *http.Response

			BeforeEach(func() {
				req, err := http.NewRequest("POST", gatewayURL+"/api/test",
					strings.NewReader(`{"hello":"world"}`))
				Expect(err).To(BeNil())
				req.Header.Set("Content-Type", "application/json")

				resp, err = http.DefaultClient.Do(req)
				Expect(err).To(BeNil())
				defer func() { _ = resp.Body.Close() }()
			})

			It("should return a successful response", func() {
				Expect(resp.StatusCode).To(Equal(http.StatusOK))
			})

			It("should publish an audit event to NATS", func() {
				Eventually(messages).Should(Receive(Satisfy(func(msg *nats.Msg) bool {
					payload := parseAuditPayload(msg)
					return payload.HTTP.Request.Method == "POST" &&
						payload.HTTP.Request.Path == "/api/test" &&
						payload.HTTP.Response.StatusCode == http.StatusOK &&
						payload.Actor.OrganizationID == "test-org" &&
						payload.Actor.StackID == "test-stack"
				})))
			})

			It("should set the X-Formance-Audit header on the proxied request", func() {
				Expect(backend.LastRequest()).NotTo(BeNil())
				Expect(backend.LastRequest().Header.Get(audit.HandledHeader)).To(Equal("true"))
			})

			It("should strip the Authorization header from the audit payload", func() {
				req, err := http.NewRequest("GET", gatewayURL+"/api/auth-check", nil)
				Expect(err).To(BeNil())
				req.Header.Set("Authorization", "Bearer some-secret-token")

				resp, err := http.DefaultClient.Do(req)
				Expect(err).To(BeNil())
				_ = resp.Body.Close()

				Eventually(messages).Should(Receive(Satisfy(func(msg *nats.Msg) bool {
					payload := parseAuditPayload(msg)
					return payload.HTTP.Request.Path == "/api/auth-check" &&
						payload.HTTP.Request.Header.Get("Authorization") == ""
				})))
			})
		})

		When("a request already has the audit header", func() {
			It("should skip audit and not publish an event", func() {
				// Drain any pending messages
				for {
					select {
					case <-messages:
					default:
						goto drained
					}
				}
			drained:

				req, err := http.NewRequest("GET", gatewayURL+"/api/skip-me", nil)
				Expect(err).To(BeNil())
				req.Header.Set(audit.HandledHeader, "true")

				resp, err := http.DefaultClient.Do(req)
				Expect(err).To(BeNil())
				_ = resp.Body.Close()

				Expect(resp.StatusCode).To(Equal(http.StatusOK))

				Consistently(messages, 500*time.Millisecond).ShouldNot(Receive())
			})
		})

		When("the audit header is not in the captured payload", func() {
			It("should not leak the audit header into the audit event", func() {
				req, err := http.NewRequest("GET", gatewayURL+"/api/no-leak", nil)
				Expect(err).To(BeNil())

				resp, err := http.DefaultClient.Do(req)
				Expect(err).To(BeNil())
				_ = resp.Body.Close()

				Eventually(messages).Should(Receive(Satisfy(func(msg *nats.Msg) bool {
					payload := parseAuditPayload(msg)
					return payload.HTTP.Request.Path == "/api/no-leak" &&
						payload.HTTP.Request.Header.Get(audit.HandledHeader) == ""
				})))
			})
		})
	})
})

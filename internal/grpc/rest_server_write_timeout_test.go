package grpc

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The data plane runs synchronous long operations — apply / retry execute
// the whole change plan and write their response once at the end (hold
// steps alone run minutes). A connection-level WriteTimeout severs exactly
// those responses: the run settles server-side while the client gets a
// reset connection (observed in the cluster lab: retry answered
// "Remote end closed connection without response" with WriteTimeout=30s).
// Per-request bounds belong to the request context, not the socket
// deadline, so the write deadline stays off. The header-read slowloris
// guard and keep-alive cleanup remain — dropping them too would trade one
// exposure for another.
func TestGatewayServerHasNoWriteTimeout(t *testing.T) {
	gw := &Gateway{}
	srv := gw.newHTTPServer(http.NewServeMux())

	assert.Zero(t, srv.WriteTimeout,
		"WriteTimeout must stay 0: it is connection-level and severs synchronous long-running responses")
	assert.Equal(t, 10*time.Second, srv.ReadHeaderTimeout)
	assert.Equal(t, 120*time.Second, srv.IdleTimeout)
}

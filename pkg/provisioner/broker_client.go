package provisioner

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/agentry/agentry/pkg/tunnel"
)

// BrokerClient is the cluster-side agent that phones home to an
// xdp-broker. When configured, the provisioner runs one in addition
// to its local HTTP listener: the same http.Handler is served over
// both the local port and the yamux session to the broker.
//
// Lifecycle:
//
//	for {
//	  Dial broker → on success, http.Serve(session, handler) until it drops
//	  on disconnect, exponential backoff, retry
//	}
//
// The loop is identity-agnostic; auth (per-cluster cert against the
// XDP CA) plugs in via the TLSConfig field once we have it. Phase-9
// ships without TLS for in-process testing.
type BrokerClient struct {
	brokerURL string
	clusterID string
	handler   http.Handler

	// tlsConfig is nil for the dev-mode plain-HTTP path and non-nil
	// for production. When set, tunnel.Dial uses it to present a
	// client cert during the TLS handshake — the broker matches the
	// CN against the cluster ID in handleTunnel.
	tlsConfig *tls.Config

	// connected is observable from outside (tests, /healthz) without
	// holding any lock. atomic.Bool gives us race-free state.
	connected atomic.Bool
}

// NewBrokerClient returns an idle client. Call Run to start the loop.
// tlsConfig may be nil — when so, the dial is plain HTTP (dev only).
func NewBrokerClient(brokerURL, clusterID string, handler http.Handler, tlsConfig *tls.Config) *BrokerClient {
	return &BrokerClient{
		brokerURL: brokerURL,
		clusterID: clusterID,
		handler:   handler,
		tlsConfig: tlsConfig,
	}
}

// Connected reports whether the tunnel is currently up. Useful for
// the provisioner's /healthz and for tests waiting on the session to
// register at the broker.
func (bc *BrokerClient) Connected() bool {
	return bc.connected.Load()
}

// Run dials the broker and serves the handler over its yamux session,
// looping with exponential backoff on disconnect. Returns when ctx
// ends (either normal shutdown or cancellation).
func (bc *BrokerClient) Run(ctx context.Context) error {
	backoff := tunnel.NewBackoff(tunnel.DialerBackoff())

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		sess, err := tunnel.Dial(ctx, tunnel.DialConfig{
			BrokerURL: bc.brokerURL,
			Role:      tunnel.RoleCluster,
			Headers: http.Header{
				tunnel.HeaderClusterID: []string{bc.clusterID},
			},
			TLSConfig: bc.tlsConfig,
		})
		if err != nil {
			delay := backoff.Next()
			log.Printf("provisioner: broker dial failed (attempt %d): %v; retry in %s",
				backoff.Attempts(), err, delay)
			if delay == 0 {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				continue
			}
		}

		backoff.Reset()
		bc.connected.Store(true)
		log.Printf("provisioner: broker tunnel established (cluster=%s url=%s)",
			bc.clusterID, bc.brokerURL)

		// http.Serve treats sess as a net.Listener; each accepted
		// yamux stream is handed to bc.handler as one request. The
		// handler is the exact same one the local HTTP listener uses,
		// so broker-routed traffic and direct-port traffic go through
		// identical middleware (auth, telemetry, routes).
		srv := &http.Server{
			Handler:           bc.handler,
			ReadHeaderTimeout: 30 * time.Second,
		}
		err = srv.Serve(sess)
		bc.connected.Store(false)
		_ = sess.Close()

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isExpectedServeExit(err) {
			log.Printf("provisioner: broker tunnel dropped: %v", err)
		} else {
			log.Printf("provisioner: broker tunnel dropped (session closed); reconnecting")
		}
		// Loop reconnects. Backoff was reset on the previous success,
		// so the first retry kicks off at Base again.
	}
}

// isExpectedServeExit returns true for the "session closed cleanly"
// shapes of error that http.Serve emits when its underlying listener
// (the yamux session) goes away. We don't want to noise-log those.
func isExpectedServeExit(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, http.ErrServerClosed) {
		return true
	}
	// yamux session shutdown isn't an exported error type; match by
	// substring (same pattern we use in tunnel/copy.go).
	s := err.Error()
	return s == "session shutdown" ||
		s == "use of closed network connection"
}

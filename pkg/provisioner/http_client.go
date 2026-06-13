package provisioner

import (
	"net"
	"net/http"
	"time"
)

// sandboxHTTPClient is the shared client for the provisioner's SHORT
// control calls into a sandbox runtime over the local docker network
// (file reads, lockfile checks, quiesce, binding stamps). Two reasons it
// exists:
//
//   - http.DefaultClient has NO timeout. A wedged or half-dead runtime
//     would hang the calling goroutine forever; over many sandboxes that
//     leaks goroutines until the provisioner falls over. A 30s ceiling
//     turns "hung forever" into a clean error the caller can surface.
//   - A shared client means a shared Transport, so connections to a
//     sandbox the provisioner talks to repeatedly (status polls, binding
//     re-stamps) get pooled instead of dialed fresh every call.
//
// NOT for long operations — railpack builds, image pushes — which keep
// their own clients/DefaultClient so the 30s ceiling can't kill a slow
// build. Those are minutes-scale by design.
var sandboxHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

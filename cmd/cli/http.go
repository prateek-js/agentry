package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// appClient is the thin client we use for control-plane calls against
// app.agentry.run. Every request carries Authorization: Bearer <pat>
// from the local config.
//
// Org scoping happens server-side: the PAT carries (UserID, OrgID),
// and every dashboard handler filters by OrgID. The CLI just hands
// the token over and trusts the answer.
type appClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

// newAppClient pulls AppURL + APIToken from the config. Returns a
// friendly error when login hasn't been run yet — that's the most
// common new-user failure and we want the message to point straight
// at the fix.
func newAppClient(cfg *Config) (*appClient, error) {
	if cfg == nil || cfg.AppURL == "" {
		return nil, errors.New("config has no app_url — run `agentry login`")
	}
	if cfg.APIToken == "" {
		return nil, errors.New("not logged in — run `agentry login`")
	}
	return &appClient{
		baseURL: strings.TrimRight(cfg.AppURL, "/"),
		token:   cfg.APIToken,
		hc:      &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// get fetches a JSON response into out. Path is the API path under
// /api/v1/, e.g. "clusters" or "clusters/clu_xxx/sandboxes".
func (c *appClient) get(path string, out any) error {
	return c.do("GET", path, nil, out)
}

// delete issues DELETE /api/v1/<path>. We don't expect a body back so
// callers don't need to pass a sink.
func (c *appClient) delete(path string) error {
	return c.do("DELETE", path, nil, nil)
}

// do is the shared transport. Empty body for GET/DELETE; pass an
// io.Reader for future POST callers.
func (c *appClient) do(method, path string, body io.Reader, out any) error {
	req, _ := http.NewRequest(method, c.baseURL+"/api/v1/"+strings.TrimLeft(path, "/"), body)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAppErr(resp.StatusCode, raw, path)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// bridgeClient builds an http.Client using the device mTLS cert. Used
// by the legacy `share ls` and `forward` flows which still pin direct
// to the bridge (the control plane doesn't expose deploy-routes or
// the yamux tunnel). New code should prefer appClient instead — that
// path requires only a PAT and never leaves the user's org.
func bridgeClient(cfg *Config) *http.Client {
	tlsConf, err := buildClientTLS(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentry: build client TLS: %v\n", err)
		return http.DefaultClient
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}}
}

// decodeAppErr turns a non-2xx response into a useful CLI error.
// agentry-app writes errors as `{"error": "…"}` — surface that string
// directly so users don't have to grep through HTTP boilerplate.
func decodeAppErr(status int, body []byte, path string) error {
	var wrap struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &wrap) == nil && wrap.Error != "" {
		msg = wrap.Error
	}
	// 401 has its own dedicated message because it's the friction
	// point new users hit on a stale or revoked token.
	if status == http.StatusUnauthorized {
		return fmt.Errorf("auth rejected — run `agentry login` (path=%s)", path)
	}
	return fmt.Errorf("%d on %s: %s", status, path, msg)
}

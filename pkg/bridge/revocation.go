package bridge

import (
	"encoding/json"
	"net/http"
	"sync"
)

// revocation.go — the bridge's certificate denylist.
//
// Issued leaf certs live for a year. Deleting a cluster/device in the
// control plane is a soft-delete (status=revoked); without this the
// bridge would keep trusting that cert's tunnel until expiry, so a
// stolen or offboarded key stays a valid identity for up to a year.
//
// The control plane pushes the set of revoked cert CNs (cluster-<name>
// / device-<name>) to PUT /api/revoked-cns on each resync, and mtlsGate
// rejects any handshake whose peer-cert CN is in the set. It's a
// denylist (fail-open): an unknown CN is allowed, so a bridge restart
// that empties the set before the next push briefly re-trusts a revoked
// cert — acceptable for best-effort revocation, and the set repopulates
// within one resync tick.

type revocationSet struct {
	mu  sync.RWMutex
	cns map[string]struct{}
}

func newRevocationSet() *revocationSet {
	return &revocationSet{cns: map[string]struct{}{}}
}

// Replace swaps in a fresh denylist (last-writer-wins, mirroring the
// deploy-route push model).
func (rs *revocationSet) Replace(cns []string) {
	m := make(map[string]struct{}, len(cns))
	for _, c := range cns {
		if c != "" {
			m[c] = struct{}{}
		}
	}
	rs.mu.Lock()
	rs.cns = m
	rs.mu.Unlock()
}

func (rs *revocationSet) has(cn string) bool {
	rs.mu.RLock()
	_, ok := rs.cns[cn]
	rs.mu.RUnlock()
	return ok
}

// IsRevoked reports whether a verified peer cert's CN has been revoked.
// Safe on a nil/empty set (returns false) so the dev path is unaffected.
func (b *Broker) IsRevoked(cn string) bool {
	if b.revocation == nil || cn == "" {
		return false
	}
	return b.revocation.has(cn)
}

type revokedCNsEnvelope struct {
	CNs []string `json:"cns"`
}

// handleRevokedCNsPut replaces the denylist. Admin-cert-gated, same as
// the deploy-route push — a regular device cert can't poison it.
func (b *Broker) handleRevokedCNsPut(w http.ResponseWriter, r *http.Request) {
	if !b.requireAdmin(w, r) {
		return
	}
	var body revokedCNsEnvelope
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	b.revocation.Replace(body.CNs)
	w.WriteHeader(http.StatusNoContent)
}

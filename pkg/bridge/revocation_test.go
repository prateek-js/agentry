package bridge

import "testing"

func TestRevocationSet_ReplaceAndHas(t *testing.T) {
	rs := newRevocationSet()
	if rs.has("cluster-a") {
		t.Fatal("empty set should revoke nothing")
	}
	rs.Replace([]string{"cluster-a", "device-b", ""})
	if !rs.has("cluster-a") || !rs.has("device-b") {
		t.Error("revoked CNs should match")
	}
	if rs.has("") || rs.has("cluster-c") {
		t.Error("empty / unknown CN must not match")
	}
	// Replace is last-writer-wins: old entries drop.
	rs.Replace([]string{"cluster-c"})
	if rs.has("cluster-a") {
		t.Error("Replace should have dropped cluster-a")
	}
	if !rs.has("cluster-c") {
		t.Error("Replace should have added cluster-c")
	}
}

func TestBrokerIsRevoked(t *testing.T) {
	b := NewWithConfig(Config{})
	if b.IsRevoked("cluster-a") {
		t.Fatal("fresh broker revokes nothing")
	}
	if b.IsRevoked("") {
		t.Error("empty CN is never revoked")
	}
	b.revocation.Replace([]string{"cluster-a"})
	if !b.IsRevoked("cluster-a") {
		t.Error("cluster-a should be revoked after Replace")
	}
}

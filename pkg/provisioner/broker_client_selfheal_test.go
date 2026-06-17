package provisioner

import (
	"errors"
	"testing"
)

func TestIsCertRejection(t *testing.T) {
	reject := []string{
		"remote error: tls: bad certificate",
		`tls: failed to verify certificate: x509: certificate signed by unknown authority`,
		"remote error: tls: certificate required",
		"x509: certificate has expired or is not yet valid",
	}
	for _, m := range reject {
		if !isCertRejection(errors.New(m)) {
			t.Errorf("isCertRejection(%q) = false, want true", m)
		}
	}

	notReject := []string{
		"dial tcp 1.2.3.4:443: connect: connection refused",
		"context deadline exceeded",
		"EOF",
		"no such host",
	}
	for _, m := range notReject {
		if isCertRejection(errors.New(m)) {
			t.Errorf("isCertRejection(%q) = true, want false", m)
		}
	}
	if isCertRejection(nil) {
		t.Error("isCertRejection(nil) = true, want false")
	}
}

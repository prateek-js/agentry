package main

import (
	"os"
	"strings"
	"testing"
)

func TestLoadEmailConfig(t *testing.T) {
	// Clear then set; restore after.
	for _, k := range []string{"SMTP_HOST", "SMTP_PORT", "SMTP_USER", "SMTP_PASSWORD", "SMTP_FROM"} {
		t.Setenv(k, "")
	}
	if _, ok := loadEmailConfig(); ok {
		t.Fatal("no SMTP_HOST/FROM → email capability should be off")
	}
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_FROM", "no-reply@example.com")
	cfg, ok := loadEmailConfig()
	if !ok {
		t.Fatal("host+from present → should be on")
	}
	if cfg.Port != "587" {
		t.Errorf("default port = %q; want 587", cfg.Port)
	}
	t.Setenv("SMTP_PORT", "465")
	cfg, _ = loadEmailConfig()
	if cfg.Port != "465" {
		t.Errorf("explicit port not honored: %q", cfg.Port)
	}
	// Missing FROM disables even with a host.
	_ = os.Unsetenv("SMTP_FROM")
	t.Setenv("SMTP_FROM", "")
	if _, ok := loadEmailConfig(); ok {
		t.Error("missing SMTP_FROM should disable email")
	}
}

func TestBuildMIME(t *testing.T) {
	msg := buildMIME("from@x.com", "to@y.com", "Reset your password", "<p>html</p>", "plain text")
	for _, want := range []string{
		"From: from@x.com", "To: to@y.com", "Subject: Reset your password",
		"multipart/alternative", "text/plain", "text/html", "plain text", "<p>html</p>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("MIME missing %q", want)
		}
	}
}

func TestSanitizeHeader_StripsCRLF(t *testing.T) {
	// Header-injection guard: a subject can't smuggle extra headers.
	got := sanitizeHeader("Hi\r\nBcc: victim@x.com")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("CRLF survived sanitize: %q", got)
	}
}

func TestEnvelopeFrom(t *testing.T) {
	cases := map[string]string{
		"no-reply@acme.com":           "no-reply@acme.com",
		"Acme <no-reply@acme.com>":    "no-reply@acme.com",
		"  spaced@acme.com  ":         "spaced@acme.com",
		`"Acme Support" <s@acme.com>`: "s@acme.com",
	}
	for in, want := range cases {
		if got := envelopeFrom(in); got != want {
			t.Errorf("envelopeFrom(%q) = %q; want %q", in, got, want)
		}
	}
}

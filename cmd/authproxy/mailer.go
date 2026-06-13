package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// mailer.go — transactional email for password reset + verification.
//
// Capability model: email is "lit up" purely by the SMTP_* env vars the
// `smtp` service binding stamps (SMTP_HOST/PORT/USER/PASSWORD/FROM). No
// separate flag — if the host is set, the authproxy can send mail, the
// forgot-password route activates, and the login page grows a "Forgot
// password?" link. Bind smtp → reset works. Don't → it stays hidden.
//
// We speak SMTP directly (net/smtp) rather than pulling a dep: the
// surface is one Send() and the providers our users bind (SES, Postmark,
// Mailgun, Gmail relay) all speak vanilla SMTP+STARTTLS or implicit TLS.

// EmailConfig is the parsed SMTP binding. Nil on the Config when no SMTP
// service is bound — that's the "email capability off" state.
type EmailConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	From     string
}

// loadEmailConfig reads the SMTP_* env the `smtp` binding injects.
// Returns (cfg, true) only when the minimum to actually send is present:
// a host and a from-address. User/password are optional (some relays
// authenticate by IP / are open on a private network), so we don't gate
// on them. Port defaults to 587 (submission + STARTTLS), the modern
// default.
func loadEmailConfig() (*EmailConfig, bool) {
	host := strings.TrimSpace(os.Getenv("SMTP_HOST"))
	from := strings.TrimSpace(os.Getenv("SMTP_FROM"))
	if host == "" || from == "" {
		return nil, false
	}
	port := strings.TrimSpace(os.Getenv("SMTP_PORT"))
	if port == "" {
		port = "587"
	}
	return &EmailConfig{
		Host:     host,
		Port:     port,
		User:     strings.TrimSpace(os.Getenv("SMTP_USER")),
		Password: os.Getenv("SMTP_PASSWORD"),
		From:     from,
	}, true
}

// Mailer sends mail through the configured SMTP relay. Behind an
// interface so handlers can be tested against a recording fake without a
// real SMTP server.
type Mailer interface {
	Send(to, subject, htmlBody, textBody string) error
}

// smtpMailer is the real implementation.
type smtpMailer struct {
	cfg *EmailConfig
}

func newSMTPMailer(cfg *EmailConfig) *smtpMailer { return &smtpMailer{cfg: cfg} }

// Send delivers one multipart (text + HTML) message. Transport is
// chosen by port: 465 → implicit TLS from the first byte; everything
// else → plaintext dial then STARTTLS upgrade when the server offers it.
// AUTH (PLAIN) is only ever sent over a TLS-protected connection — we
// refuse to hand credentials to a cleartext socket.
func (m *smtpMailer) Send(to, subject, htmlBody, textBody string) error {
	addr := net.JoinHostPort(m.cfg.Host, m.cfg.Port)
	msg := buildMIME(m.cfg.From, to, subject, htmlBody, textBody)

	dialTimeout := 10 * time.Second
	tlsConf := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}

	var client *smtp.Client
	var err error
	if m.cfg.Port == "465" {
		// Implicit TLS (SMTPS).
		conn, derr := tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", addr, tlsConf)
		if derr != nil {
			return fmt.Errorf("smtp dial(tls) %s: %w", addr, derr)
		}
		client, err = smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("smtp client: %w", err)
		}
	} else {
		conn, derr := net.DialTimeout("tcp", addr, dialTimeout)
		if derr != nil {
			return fmt.Errorf("smtp dial %s: %w", addr, derr)
		}
		client, err = smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("smtp client: %w", err)
		}
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConf); err != nil {
				_ = client.Close()
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}
	defer client.Close()

	// AUTH only over a secured connection.
	if m.cfg.User != "" {
		if ok, _ := client.Extension("STARTTLS"); ok || m.cfg.Port == "465" {
			if tlsOK, _ := client.TLSConnectionState(); tlsOK.HandshakeComplete {
				auth := smtp.PlainAuth("", m.cfg.User, m.cfg.Password, m.cfg.Host)
				if err := client.Auth(auth); err != nil {
					return fmt.Errorf("smtp auth: %w", err)
				}
			}
		}
	}

	if err := client.Mail(envelopeFrom(m.cfg.From)); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := wc.Write([]byte(msg)); err != nil {
		_ = wc.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return client.Quit()
}

// buildMIME assembles a minimal multipart/alternative message (plain +
// HTML) so clients that block HTML still show the link. We keep the
// boundary fixed-but-unique-per-process via a constant; collisions with
// body content are vanishingly unlikely for our short transactional
// bodies.
func buildMIME(from, to, subject, htmlBody, textBody string) string {
	const boundary = "agentry-authproxy-boundary-7f3a9c"
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(textBody + "\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(htmlBody + "\r\n")
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

// sanitizeHeader strips CR/LF so a subject can't inject extra headers
// (header-injection / SMTP smuggling). Subjects here are fixed strings,
// but defense-in-depth is cheap.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

// envelopeFrom extracts the bare address from a possibly display-named
// From ("Acme <no-reply@acme.com>" → "no-reply@acme.com") for the SMTP
// MAIL FROM command, which wants the address only.
func envelopeFrom(from string) string {
	if i := strings.LastIndex(from, "<"); i >= 0 {
		if j := strings.Index(from[i:], ">"); j >= 0 {
			return strings.TrimSpace(from[i+1 : i+j])
		}
	}
	return strings.TrimSpace(from)
}

package main

import (
	"bytes"
	"fmt"
	"html/template"
	"sort"
	"strings"
)

// templates.go — the HTML the sidecar serves.
//
// Two surfaces: /auth/login and /auth/signup. Both inline-styled, no
// JS, no external CSS — the page renders in one round trip even when
// the user's app is broken. Apple-ish polish: zinc/neutral palette,
// generous padding, system fonts. We are NOT trying to win a design
// award; we are trying to look credible enough that a real user types
// their password into it.
//
// Templates are parsed once at boot so handlers stay branch-free.

type pageData struct {
	Title     string
	CSRFToken string
	Error     string  // flash text rendered above the form on a retry
	Email     string  // sticky email so the user doesn't retype
	Providers []providerButton
	ShowName  bool    // signup page wants the Name field
	OtherURL  string  // link to the OTHER page ("don't have an account?")
	OtherText string
	PostTarget string // form action — /auth/login or /auth/signup
}

type providerButton struct {
	Name  string // "google", "github" …
	Label string // "Continue with Google"
}

var (
	loginTmpl  = template.Must(template.New("login").Parse(authHTML))
	signupTmpl = template.Must(template.New("signup").Parse(authHTML))
)

func renderLogin(d pageData) ([]byte, error) {
	d.Title = "Sign in"
	d.PostTarget = "/auth/login"
	d.OtherURL = "/auth/signup"
	d.OtherText = "Create an account"
	d.ShowName = false
	return renderPage(loginTmpl, d)
}

func renderSignup(d pageData) ([]byte, error) {
	d.Title = "Create your account"
	d.PostTarget = "/auth/signup"
	d.OtherURL = "/auth/login"
	d.OtherText = "Already have an account? Sign in"
	d.ShowName = true
	return renderPage(signupTmpl, d)
}

func renderPage(t *template.Template, d pageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return buf.Bytes(), nil
}

// providerButtons converts the config map into a stable, alphabetised
// list of buttons. Stable order so the page doesn't flicker between
// renders (Go's map iteration is randomised).
func providerButtons(providers map[string]ProviderConfig) []providerButton {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]providerButton, 0, len(names))
	for _, n := range names {
		out = append(out, providerButton{
			Name:  n,
			Label: "Continue with " + providerDisplayName(n),
		})
	}
	return out
}

func providerDisplayName(n string) string {
	switch n {
	case "google":
		return "Google"
	case "github":
		return "GitHub"
	case "microsoft":
		return "Microsoft"
	case "apple":
		return "Apple"
	case "generic-oidc":
		return "SSO"
	default:
		return strings.Title(n)
	}
}

// authHTML — single template shared by login + signup. Conditionals
// on .ShowName / .OtherURL keep them on the same page skeleton so
// styling stays consistent.
const authHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>{{.Title}}</title>
<style>
*, *::before, *::after { box-sizing: border-box; }
html, body { margin: 0; padding: 0; height: 100%; }
body {
  font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", system-ui, sans-serif;
  background: #fafafa;
  color: #111827;
  -webkit-font-smoothing: antialiased;
  display: flex;
  align-items: center;
  justify-content: center;
}
.card {
  width: 100%;
  max-width: 380px;
  margin: 24px;
  padding: 32px;
  background: #ffffff;
  border: 1px solid #e5e7eb;
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(0,0,0,0.04);
}
h1 {
  margin: 0 0 24px 0;
  font-size: 20px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
label {
  display: block;
  font-size: 13px;
  font-weight: 500;
  color: #374151;
  margin-bottom: 6px;
}
input[type="email"], input[type="password"], input[type="text"] {
  width: 100%;
  padding: 9px 12px;
  font-size: 14px;
  border: 1px solid #d1d5db;
  border-radius: 8px;
  background: #ffffff;
  color: #111827;
  outline: none;
  transition: border-color 120ms ease, box-shadow 120ms ease;
}
input:focus {
  border-color: #6366f1;
  box-shadow: 0 0 0 3px rgba(99,102,241,0.12);
}
.field { margin-bottom: 16px; }
button.primary {
  width: 100%;
  padding: 10px 12px;
  font-size: 14px;
  font-weight: 600;
  color: #ffffff;
  background: #111827;
  border: 1px solid #111827;
  border-radius: 8px;
  cursor: pointer;
  transition: background 120ms ease;
}
button.primary:hover { background: #1f2937; }
.providers { margin-top: 20px; }
.provider-btn {
  display: block;
  width: 100%;
  padding: 9px 12px;
  margin-bottom: 8px;
  font-size: 14px;
  font-weight: 500;
  color: #111827;
  background: #ffffff;
  border: 1px solid #d1d5db;
  border-radius: 8px;
  text-decoration: none;
  text-align: center;
  cursor: pointer;
  transition: background 120ms ease, border-color 120ms ease;
}
.provider-btn:hover { background: #f9fafb; border-color: #9ca3af; }
.divider {
  display: flex;
  align-items: center;
  margin: 20px 0;
  color: #6b7280;
  font-size: 12px;
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.divider::before, .divider::after {
  content: "";
  flex: 1;
  height: 1px;
  background: #e5e7eb;
}
.divider::before { margin-right: 12px; }
.divider::after { margin-left: 12px; }
.other {
  margin-top: 20px;
  text-align: center;
  font-size: 13px;
  color: #6b7280;
}
.other a { color: #4f46e5; text-decoration: none; font-weight: 500; }
.other a:hover { text-decoration: underline; }
.error {
  margin-bottom: 16px;
  padding: 10px 12px;
  background: #fef2f2;
  color: #991b1b;
  border: 1px solid #fecaca;
  border-radius: 8px;
  font-size: 13px;
}
</style>
</head>
<body>
<div class="card">
<h1>{{.Title}}</h1>
{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{if .Providers}}
<div class="providers">
{{range .Providers}}<a class="provider-btn" href="/auth/oauth/{{.Name}}/start">{{.Label}}</a>{{end}}
</div>
<div class="divider">or</div>
{{end}}
<form method="POST" action="{{.PostTarget}}" autocomplete="on">
<input type="hidden" name="csrf_token" value="{{.CSRFToken}}" />
{{if .ShowName}}
<div class="field">
<label for="name">Name</label>
<input id="name" name="name" type="text" autocomplete="name" />
</div>
{{end}}
<div class="field">
<label for="email">Email</label>
<input id="email" name="email" type="email" autocomplete="email" required value="{{.Email}}" />
</div>
<div class="field">
<label for="password">Password</label>
<input id="password" name="password" type="password" autocomplete="{{if .ShowName}}new-password{{else}}current-password{{end}}" required />
</div>
<button class="primary" type="submit">{{if .ShowName}}Create account{{else}}Sign in{{end}}</button>
</form>
<div class="other"><a href="{{.OtherURL}}">{{.OtherText}}</a></div>
</div>
</body>
</html>`

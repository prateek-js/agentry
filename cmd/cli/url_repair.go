package main

import (
	"net/url"
	"strings"
)

// url_repair.go — fix DB connection URLs whose userinfo (user:password)
// contains characters that look like malformed percent-encoding.
//
// The bug we're solving: an operator pastes
//
//	postgresql://postgres:4g.uW%azWbL*mZ9@host:5432/db
//
// into the bind prompt. Go's net/url.Parse, pgx's parser, and every
// other URL parser correctly read `%` as the start of a percent-
// encoded triplet, see `az` (which are not hex digits), and fail with
// "invalid URL escape %az". The user typed a password with special
// characters — they're not wrong; the URL just needs the userinfo
// percent-encoded before any parser touches it.
//
// repairConnectionURL detects this case and re-encodes the password
// (and optionally the username) in place. URLs that already parse
// cleanly are passed through unchanged. We DON'T mutate URLs that
// look bad in some other way (no scheme, no host) — those are real
// bugs the user needs to see.

// repairConnectionURL returns a re-encoded URL when the input has
// userinfo with raw special characters that break url.Parse. Empty
// input → empty output. URLs that already parse → returned as-is.
// Anything we can't make sense of → returned as-is (the caller's
// parser will surface its own error message).
func repairConnectionURL(raw string) string {
	if raw == "" {
		return raw
	}
	// If url.Parse is happy, the URL is already well-formed — don't
	// touch it. Idempotency matters: repeated bindings of the same
	// URL must not double-encode (`%25az` after one pass, `%2525az`
	// after two, …).
	if _, err := url.Parse(raw); err == nil {
		return raw
	}

	// Find the scheme separator. URLs without "://" aren't connection
	// strings we know how to repair.
	schemeIdx := strings.Index(raw, "://")
	if schemeIdx < 0 {
		return raw
	}
	scheme := raw[:schemeIdx+3]
	rest := raw[schemeIdx+3:]

	// The host portion ends at "/", "?", or "#". Userinfo (if any)
	// lives between the scheme and the LAST "@" before the host.
	hostStart := indexOfAny(rest, "/?#")
	var hostAndQuery string
	if hostStart < 0 {
		hostAndQuery = ""
	} else {
		hostAndQuery = rest[hostStart:]
		rest = rest[:hostStart]
	}

	// "Last @" is right — passwords can legally contain @ (rare but
	// possible). The host can't, so the rightmost @ is always the
	// userinfo/host boundary.
	atIdx := strings.LastIndexByte(rest, '@')
	if atIdx < 0 {
		// No userinfo. Whatever broke url.Parse isn't a password
		// escape — leave the original alone.
		return raw
	}
	userinfo := rest[:atIdx]
	host := rest[atIdx:] // includes the leading '@'

	// userinfo = user[:password]. Split on the FIRST ':' — passwords
	// can contain colons (especially when they're auto-generated), but
	// usernames cannot.
	var user, pass string
	if c := strings.IndexByte(userinfo, ':'); c >= 0 {
		user = userinfo[:c]
		pass = userinfo[c+1:]
	} else {
		user = userinfo
	}

	repaired := scheme +
		url.QueryEscape(user) // covers special chars in usernames too
	if pass != "" || strings.Contains(userinfo, ":") {
		repaired += ":" + url.QueryEscape(pass)
	}
	repaired += host + hostAndQuery

	// Only return the repaired version if it actually parses now —
	// otherwise the user has a different problem and the original
	// error message is more useful than a confusing repaired one.
	if _, err := url.Parse(repaired); err != nil {
		return raw
	}
	return repaired
}

// indexOfAny returns the position of the FIRST occurrence of any
// byte in `chars` within s, or -1 if none are present. strings has
// IndexAny but it's for unicode runes — we want raw bytes here since
// '/' '?' '#' are ASCII.
func indexOfAny(s, chars string) int {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(chars, s[i]) >= 0 {
			return i
		}
	}
	return -1
}

// looksLikeDBURL returns true when value looks like one of the
// connection URLs we want to repair. Used at bind time to avoid
// trying to repair, say, an OpenAI API key.
func looksLikeDBURL(value string) bool {
	for _, p := range []string{
		"postgres://", "postgresql://",
		"mysql://", "mariadb://",
		"mongodb://", "mongodb+srv://",
	} {
		if strings.HasPrefix(value, p) {
			return true
		}
	}
	return false
}

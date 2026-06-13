package main

import (
	"errors"
	"strings"
	"unicode"
)

// password.go — the password policy, shared by signup + reset.
//
// Deliberately modest: a length floor + a reject-list of the passwords
// that actually show up in credential-stuffing dumps, plus a "not all
// one character" guard. We don't impose character-class rules (mixed
// case / symbols) — NIST 800-63B advises against them; they push users
// toward predictable patterns (Password1!) without adding real entropy.
// Length + a common-password screen is the better-evidenced bar.

const minPasswordLen = 8

// commonPasswords is a small screen of the most abused passwords. Not
// exhaustive — a true HaveIBeenPwned check needs a network call we don't
// want in the hot path — but it stops the lowest-effort guesses.
var commonPasswords = map[string]struct{}{
	"password": {}, "password1": {}, "password123": {}, "12345678": {},
	"123456789": {}, "1234567890": {}, "qwerty123": {}, "qwertyuiop": {},
	"letmein": {}, "welcome1": {}, "admin123": {}, "iloveyou": {},
	"changeme": {}, "passw0rd": {}, "1q2w3e4r": {}, "abc12345": {},
	"football": {}, "baseball": {}, "trustno1": {}, "sunshine": {},
}

// validatePassword returns nil when the password clears the policy, or a
// user-facing error explaining the single most relevant failure.
func validatePassword(pw string) error {
	if len(pw) < minPasswordLen {
		return errors.New("Password must be at least 8 characters.")
	}
	if _, bad := commonPasswords[strings.ToLower(pw)]; bad {
		return errors.New("That password is too common. Choose something harder to guess.")
	}
	if allSameRune(pw) {
		return errors.New("Password can't be a single repeated character.")
	}
	return nil
}

// allSameRune reports whether every rune in s is identical (e.g.
// "aaaaaaaa"), a common way to scrape past a bare length check.
func allSameRune(s string) bool {
	if s == "" {
		return false
	}
	var first rune
	for i, r := range s {
		if i == 0 {
			first = r
			continue
		}
		if r != first {
			return false
		}
	}
	// Ignore whitespace-only as "same" too — caller already trims.
	return !unicode.IsSpace(first)
}

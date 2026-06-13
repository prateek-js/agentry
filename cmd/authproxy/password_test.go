package main

import "testing"

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		ok   bool
	}{
		{"good", "correct-horse-battery", true},
		{"min length exactly 8", "abcd1234", true},
		{"too short", "abc123", false},
		{"common password", "password", false},
		{"common mixed case", "Password123", false}, // lowercased match
		{"all same char", "aaaaaaaa", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePassword(tc.pw)
			if tc.ok && err != nil {
				t.Errorf("%q should pass; got %v", tc.pw, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("%q should be rejected", tc.pw)
			}
		})
	}
}

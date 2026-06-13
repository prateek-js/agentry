package bridge

import "testing"

func TestFilterClerkCookies(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "only __session", in: "__session=abc", want: ""},
		{name: "only __client_uat", in: "__client_uat=ts", want: ""},
		{name: "only __client_state", in: "__client_state=foo", want: ""},
		{name: "agentry_csrf passes through",
			in: "agentry_csrf=tok", want: "agentry_csrf=tok"},
		{name: "agentry_session passes through",
			in: "agentry_session=val", want: "agentry_session=val"},
		{name: "Clerk + agentry — strip Clerk only",
			in:   "__session=clerk; agentry_csrf=a; __client_uat=ts; agentry_session=b",
			want: "agentry_csrf=a; agentry_session=b"},
		{name: "third-party cookie passes through",
			in: "ga=ga-value; __session=clerk", want: "ga=ga-value"},
		{name: "malformed entry (no =) preserved if not Clerk",
			in: "weirdname; agentry_csrf=x", want: "weirdname; agentry_csrf=x"},
		{name: "case-sensitive — _Session is not Clerk",
			in: "_Session=fake; agentry_csrf=x", want: "_Session=fake; agentry_csrf=x"},
		{name: "value with = inside is preserved",
			in: "auth=key=val; agentry_csrf=x", want: "auth=key=val; agentry_csrf=x"},
		{name: "trailing semicolon",
			in: "agentry_csrf=x;", want: "agentry_csrf=x"},
		{name: "leading/trailing space",
			in: " agentry_csrf=x ; __session=y ", want: "agentry_csrf=x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterClerkCookies(tc.in)
			if got != tc.want {
				t.Errorf("filterClerkCookies(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsClerkCookieName(t *testing.T) {
	clerk := []string{
		"__session", "__client_uat",
		"__client_state", "__client_anything",
	}
	notClerk := []string{
		"agentry_csrf", "agentry_session", "ga", "session",
		"_client_uat",         // single underscore — not Clerk
		"__Session",           // wrong case
		"client_uat",          // missing prefix
		"",
	}
	for _, n := range clerk {
		if !isClerkCookieName(n) {
			t.Errorf("%q should be classed as Clerk", n)
		}
	}
	for _, n := range notClerk {
		if isClerkCookieName(n) {
			t.Errorf("%q should NOT be classed as Clerk", n)
		}
	}
}

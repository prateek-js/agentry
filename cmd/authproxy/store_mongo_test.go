package main

import "testing"

func TestDatabaseFromURIDefaults(t *testing.T) {
	cases := map[string]string{
		"mongodb://h:27017":                  mongoDefaultDB,
		"mongodb://h:27017/":                 mongoDefaultDB,
		"mongodb://h:27017/myapp":            "myapp",
		"mongodb://h:27017/myapp?retry=1":    "myapp",
		"mongodb+srv://cluster/auth":         "auth",
		"":                                   mongoDefaultDB,
	}
	for in, want := range cases {
		if got := databaseFromURI(in); got != want {
			t.Errorf("databaseFromURI(%q) = %q, want %q", in, got, want)
		}
	}
}

// userFromDoc exercises the bson-doc → User mapping without needing a
// live mongo. We don't synthesize a bson.DateTime here because that
// type's constructor is awkward; the CreatedAt-zero branch is the
// path the SQL adapter also hits when a row's column comes back NULL.
func TestUserFromDocBasic(t *testing.T) {
	doc := map[string]any{
		"_id":           "id1",
		"email":         "x@y.com",
		"password_hash": "hash",
		"name":          "Alice",
		"provider":      "password",
		"provider_id":   "",
	}
	u := userFromDoc(doc)
	if u.ID != "id1" || u.Email != "x@y.com" || u.Provider != "password" {
		t.Fatalf("decoded wrong: %+v", u)
	}
}

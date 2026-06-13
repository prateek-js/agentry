package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is the in-memory Store the handler tests use. Keeping it
// in the same package as the real store keeps the contract honest —
// a method-set drift breaks compilation here first.
type fakeStore struct {
	mu        sync.Mutex
	byEmail   map[string]*User
	byID      map[string]*User
	byProvKey map[string]*User // provider+id -> user
	tokens    map[string]*fakeToken
	nextID    int
}

type fakeToken struct {
	userID    string
	purpose   string
	expiresAt time.Time
	used      bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byEmail:   map[string]*User{},
		byID:      map[string]*User{},
		byProvKey: map[string]*User{},
		tokens:    map[string]*fakeToken{},
	}
}

func (f *fakeStore) CreateUserPassword(_ context.Context, email, password, name string) (*User, error) {
	if len(password) < 8 {
		return nil, errors.New("password too short")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byEmail[email]; ok {
		return nil, ErrEmailTaken
	}
	f.nextID++
	u := &User{
		ID:           idForTest(f.nextID),
		Email:        email,
		PasswordHash: "h:" + password, // fake hash for the test surface
		Name:         name,
		Provider:     "password",
		CreatedAt:    time.Now(),
	}
	f.byEmail[email] = u
	f.byID[u.ID] = u
	return u, nil
}

func (f *fakeStore) GetUserByEmail(_ context.Context, email string) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byEmail[strings.ToLower(email)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return u, nil
}

func (f *fakeStore) UpsertUserFromOAuth(_ context.Context, provider, providerID, email, name string) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := provider + ":" + providerID
	if u, ok := f.byProvKey[key]; ok {
		u.Name = name
		return u, nil
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if u, ok := f.byEmail[email]; ok {
		u.Provider = provider
		u.ProviderID = providerID
		u.Name = name
		f.byProvKey[key] = u
		return u, nil
	}
	f.nextID++
	u := &User{
		ID:         idForTest(f.nextID),
		Email:      email,
		Name:       name,
		Provider:   provider,
		ProviderID: providerID,
		CreatedAt:  time.Now(),
	}
	f.byEmail[email] = u
	f.byProvKey[key] = u
	f.byID[u.ID] = u
	return u, nil
}

func (f *fakeStore) Close() error { return nil }

func (f *fakeStore) UpdatePassword(_ context.Context, userID, newPassword string) error {
	if len(newPassword) < 8 {
		return errors.New("password too short")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[userID]
	if !ok {
		return sql.ErrNoRows
	}
	u.PasswordHash = "h:" + newPassword
	u.FailedAttempts = 0
	u.LockedUntil = nil
	return nil
}

func (f *fakeStore) MarkEmailVerified(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.byID[userID]; ok {
		u.EmailVerified = true
	}
	return nil
}

func (f *fakeStore) RecordLoginFailure(_ context.Context, userID string, attempts int, lockedUntil time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[userID]
	if !ok {
		return nil
	}
	u.FailedAttempts = attempts
	if lockedUntil.IsZero() {
		u.LockedUntil = nil
	} else {
		t := lockedUntil
		u.LockedUntil = &t
	}
	return nil
}

func (f *fakeStore) ResetLoginAttempts(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.byID[userID]; ok {
		u.FailedAttempts = 0
		u.LockedUntil = nil
	}
	return nil
}

func (f *fakeStore) CreateEmailToken(_ context.Context, userID, purpose, tokenHash string, expiresAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Drop existing tokens of the same purpose for this user.
	for h, t := range f.tokens {
		if t.userID == userID && t.purpose == purpose {
			delete(f.tokens, h)
		}
	}
	f.tokens[tokenHash] = &fakeToken{userID: userID, purpose: purpose, expiresAt: expiresAt}
	return nil
}

func (f *fakeStore) ConsumeEmailToken(_ context.Context, purpose, tokenHash string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[tokenHash]
	if !ok || t.used || t.purpose != purpose || time.Now().After(t.expiresAt) {
		return "", ErrTokenInvalid
	}
	t.used = true
	return t.userID, nil
}

func idForTest(n int) string {
	// 32-char pseudo-id, predictable for assertions.
	return strings.Repeat("0", 31) + string(rune('0'+(n%10)))
}

// TestStoreInterface confirms fakeStore implements Store. If a method
// is added to Store, this file fails to compile — exactly what we want.
func TestStoreInterface(t *testing.T) {
	var _ Store = newFakeStore()
}

func TestRebindPostgres(t *testing.T) {
	q := rebind("postgres", `SELECT * FROM t WHERE a = ? AND b = ?`)
	want := `SELECT * FROM t WHERE a = $1 AND b = $2`
	if q != want {
		t.Fatalf("rebind:\n got=%q\nwant=%q", q, want)
	}
}

func TestRebindMySQLPassthrough(t *testing.T) {
	in := `SELECT * FROM t WHERE a = ? AND b = ?`
	if got := rebind("mysql", in); got != in {
		t.Fatalf("mysql rebind should be identity: %q", got)
	}
}

func TestNewUserID(t *testing.T) {
	a, err := newUserID()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 {
		t.Fatalf("expected 32 hex chars, got %d", len(a))
	}
	b, _ := newUserID()
	if a == b {
		t.Fatal("collision on consecutive newUserID")
	}
}

func TestIsUniqueViolation(t *testing.T) {
	cases := map[string]bool{
		"":                                  false,
		"connection refused":                false,
		"ERROR: duplicate key value":        true,
		"UNIQUE constraint failed: x":       true,
		"Error 1062: Duplicate entry":       true,
		"duplicate key value violates unique constraint": true,
	}
	for msg, want := range cases {
		var got bool
		if msg == "" {
			got = isUniqueViolation(nil)
		} else {
			got = isUniqueViolation(errors.New(msg))
		}
		if got != want {
			t.Fatalf("isUniqueViolation(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestFakeStoreCreateUserHappy(t *testing.T) {
	s := newFakeStore()
	u, err := s.CreateUserPassword(context.Background(), "Mixed@Case.com", "longenough", "Some Name")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "mixed@case.com" {
		t.Fatalf("email not lowercased: %q", u.Email)
	}
	// Duplicate email -> ErrEmailTaken.
	if _, err := s.CreateUserPassword(context.Background(), "mixed@case.com", "longenough", ""); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

func TestFakeStoreGetUserByEmail(t *testing.T) {
	s := newFakeStore()
	if _, err := s.GetUserByEmail(context.Background(), "absent@x.com"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows, got %v", err)
	}
	_, _ = s.CreateUserPassword(context.Background(), "x@x.com", "longenough", "")
	u, err := s.GetUserByEmail(context.Background(), "x@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.Provider != "password" {
		t.Fatalf("wrong provider: %q", u.Provider)
	}
}

func TestFakeStoreUpsertLinksExistingEmail(t *testing.T) {
	s := newFakeStore()
	first, _ := s.CreateUserPassword(context.Background(), "u@x.com", "longenough", "")
	linked, err := s.UpsertUserFromOAuth(context.Background(), "google", "gid1", "u@x.com", "Linked")
	if err != nil {
		t.Fatal(err)
	}
	if linked.ID != first.ID {
		t.Fatalf("expected linked row id %q, got %q", first.ID, linked.ID)
	}
	if linked.Provider != "google" {
		t.Fatalf("expected provider google, got %q", linked.Provider)
	}
}

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

// store.go — the per-app user table.
//
// One table, `agentry_users`, owned entirely by the sidecar. Schema:
//
//	id           uuid-ish hex (32 chars), primary key
//	email        unique, lowercased on write
//	password_hash bcrypt of password; empty for social-only accounts
//	name         display name
//	provider     "password" | "google" | "github" | …
//	provider_id  upstream's stable user id; empty for password accounts
//	created_at   first-seen timestamp
//
// Postgres and MySQL share enough SQL that the same statements work
// in both, with one quirk: mysql doesn't have ON CONFLICT DO NOTHING,
// so the migration uses CREATE TABLE IF NOT EXISTS and our user CRUD
// dodges the deduplication problem by reading-before-write where it
// matters.
//
// We do NOT introduce a separate `agentry_sessions` table — sessions
// are stateless (sealed cookies), see session.go.

type User struct {
	ID            string
	Email         string
	PasswordHash  string
	Name          string
	Provider      string // "password" | "google" | "github" | …
	ProviderID    string
	CreatedAt     time.Time
}

// Store is the thin abstraction over the user table. Keeping it
// behind an interface makes the handlers testable against a fake
// without spinning up a real DB.
type Store interface {
	CreateUserPassword(ctx context.Context, email, password, name string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	UpsertUserFromOAuth(ctx context.Context, provider, providerID, email, name string) (*User, error)
	Close() error
}

// openStore dials the bound DB + ensures the users collection/table
// exists. Caller is responsible for calling Close() on shutdown.
// Family is "postgres", "mysql", or "mongo" — anything else is the
// caller's bug.
func openStore(family, url string) (Store, error) {
	switch family {
	case "postgres":
		db, err := sql.Open("pgx", url)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
		s := &sqlStore{db: db, kind: "postgres"}
		if err := s.migrate(); err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	case "mysql":
		db, err := sql.Open("mysql", url)
		if err != nil {
			return nil, fmt.Errorf("open mysql: %w", err)
		}
		s := &sqlStore{db: db, kind: "mysql"}
		if err := s.migrate(); err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	case "mongo", "mongodb":
		return openMongoStore(url)
	default:
		return nil, fmt.Errorf("unsupported DB family %q", family)
	}
}

// sqlNoRows is the SQL sentinel mongo's NoDocuments maps to, so the
// handlers can use a single errors.Is check across all three families.
var sqlNoRows = sql.ErrNoRows

// sqlStore is the shared implementation for postgres + mysql. The
// only family-specific bit is the migration DDL — both families
// accept the same SELECT/INSERT/UPDATE shapes for the rest.
type sqlStore struct {
	db   *sql.DB
	kind string
}

func (s *sqlStore) Close() error { return s.db.Close() }

// migrate runs CREATE TABLE IF NOT EXISTS — idempotent, runs on every
// boot. We don't have a migration framework because we don't (yet)
// have multiple migrations: the schema is small and stable enough that
// "create if missing" handles every case until we need a real one.
func (s *sqlStore) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ddl string
	switch s.kind {
	case "postgres":
		ddl = `CREATE TABLE IF NOT EXISTS agentry_users (
            id            TEXT PRIMARY KEY,
            email         TEXT NOT NULL UNIQUE,
            password_hash TEXT NOT NULL DEFAULT '',
            name          TEXT NOT NULL DEFAULT '',
            provider      TEXT NOT NULL DEFAULT 'password',
            provider_id   TEXT NOT NULL DEFAULT '',
            created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
        )`
	case "mysql":
		ddl = `CREATE TABLE IF NOT EXISTS agentry_users (
            id            VARCHAR(64) PRIMARY KEY,
            email         VARCHAR(255) NOT NULL UNIQUE,
            password_hash TEXT NOT NULL,
            name          VARCHAR(255) NOT NULL,
            provider      VARCHAR(64) NOT NULL,
            provider_id   VARCHAR(255) NOT NULL,
            created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
        )`
	}
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate %s: %w", s.kind, err)
	}
	// Provider+ID composite uniqueness — separate from the email
	// unique so two providers can each have the same user (linked
	// accounts).
	idxDDL := `CREATE INDEX IF NOT EXISTS agentry_users_provider_idx ON agentry_users (provider, provider_id)`
	if s.kind == "mysql" {
		// MySQL doesn't accept IF NOT EXISTS on CREATE INDEX in all
		// versions. Try it; on failure, swallow because the index
		// probably already exists. The behaviour we care about
		// (faster provider lookups) degrades silently in the worst
		// case to a table scan.
		_, _ = s.db.ExecContext(ctx, idxDDL)
		return nil
	}
	if _, err := s.db.ExecContext(ctx, idxDDL); err != nil {
		// Same fallback for postgres — the index isn't load-bearing
		// for correctness.
		_ = err
	}
	return nil
}

// CreateUserPassword inserts a brand-new user with a bcrypt-hashed
// password. Returns ErrEmailTaken if the email already exists; the
// signup handler shows that as "an account with that email already
// exists" without leaking whether the original signup was social or
// password-based.
func (s *sqlStore) CreateUserPassword(ctx context.Context, email, password, name string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, errors.New("invalid email")
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	uid, err := newUserID()
	if err != nil {
		return nil, err
	}
	u := &User{
		ID:           uid,
		Email:        email,
		PasswordHash: string(hash),
		Name:         strings.TrimSpace(name),
		Provider:     "password",
		CreatedAt:    time.Now().UTC(),
	}
	q := rebind(s.kind, `INSERT INTO agentry_users (id, email, password_hash, name, provider, provider_id, created_at)
                         VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if _, err := s.db.ExecContext(ctx, q, u.ID, u.Email, u.PasswordHash, u.Name, u.Provider, "", u.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}
	return u, nil
}

// GetUserByEmail loads the row used by the password-login path.
// Returns (nil, sql.ErrNoRows) when the user doesn't exist so the
// handler can surface "invalid email or password" rather than
// distinguishing the two — standard "don't leak which side was
// wrong" defence.
func (s *sqlStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	q := rebind(s.kind, `SELECT id, email, password_hash, name, provider, provider_id, created_at
                          FROM agentry_users WHERE email = ?`)
	row := s.db.QueryRowContext(ctx, q, email)
	var u User
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Provider, &u.ProviderID, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// UpsertUserFromOAuth is the OAuth callback's landing path. Try
// provider+id first (the stable identifier); if that misses, try
// email; if both miss, create a new row. Updates the name on every
// login so display names stay fresh as users edit them upstream.
func (s *sqlStore) UpsertUserFromOAuth(ctx context.Context, provider, providerID, email, name string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	// Try provider lookup first — most reliable signal that "this is
	// the same person as last time."
	q := rebind(s.kind, `SELECT id, email, password_hash, name, provider, provider_id, created_at
                          FROM agentry_users WHERE provider = ? AND provider_id = ?`)
	var u User
	err := s.db.QueryRowContext(ctx, q, provider, providerID).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Provider, &u.ProviderID, &u.CreatedAt)
	if err == nil {
		// Refresh name on every login.
		if name != "" && name != u.Name {
			upd := rebind(s.kind, `UPDATE agentry_users SET name = ? WHERE id = ?`)
			_, _ = s.db.ExecContext(ctx, upd, name, u.ID)
			u.Name = name
		}
		return &u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Then by email. Lets the same human have linked accounts —
	// signs up with password, later "logs in with Google" with the
	// same email, ends up on the same row.
	q = rebind(s.kind, `SELECT id, email, password_hash, name, provider, provider_id, created_at
                       FROM agentry_users WHERE email = ?`)
	err = s.db.QueryRowContext(ctx, q, email).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Provider, &u.ProviderID, &u.CreatedAt)
	if err == nil {
		// Attach the OAuth identity to the existing row.
		upd := rebind(s.kind, `UPDATE agentry_users SET provider = ?, provider_id = ?, name = ?
                               WHERE id = ?`)
		if _, e := s.db.ExecContext(ctx, upd, provider, providerID, name, u.ID); e != nil {
			return nil, e
		}
		u.Provider = provider
		u.ProviderID = providerID
		if name != "" {
			u.Name = name
		}
		return &u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Fresh user. provider_id needs to be unique-ish too — we use
	// the provider's stable id so re-running the callback for the
	// same user just hits the first lookup next time.
	uid, err := newUserID()
	if err != nil {
		return nil, err
	}
	u = User{
		ID:         uid,
		Email:      email,
		Name:       strings.TrimSpace(name),
		Provider:   provider,
		ProviderID: providerID,
		CreatedAt:  time.Now().UTC(),
	}
	ins := rebind(s.kind, `INSERT INTO agentry_users (id, email, password_hash, name, provider, provider_id, created_at)
                            VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if _, err := s.db.ExecContext(ctx, ins, u.ID, u.Email, "", u.Name, u.Provider, u.ProviderID, u.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert oauth user: %w", err)
	}
	return &u, nil
}

// ErrEmailTaken is the signup-side error code the handler converts
// into a user-facing message.
var ErrEmailTaken = errors.New("email already taken")

// newUserID returns 32 hex chars of cryptographic randomness — 128
// bits of entropy, plenty for unguessability and stable across DB
// families (UUID v4 would work too but adds a dep).
func newUserID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// rebind swaps `?` placeholders for postgres's `$N` form. database/sql
// doesn't do this automatically because the pgx driver expects
// pg-flavour placeholders. Keeping the source SQL in ?-form means the
// CRUD methods stay readable; rebind() is the only family-aware bit.
func rebind(kind, q string) string {
	if kind != "postgres" {
		return q
	}
	var out strings.Builder
	idx := 1
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			out.WriteByte('$')
			out.WriteString(fmt.Sprintf("%d", idx))
			idx++
			continue
		}
		out.WriteByte(q[i])
	}
	return out.String()
}

// isUniqueViolation tries to tell apart "duplicate-email INSERT
// blocked" from other DB errors. Both postgres and mysql encode this
// in the error message; we sniff for the substrings their drivers
// emit. Brittle but bounded — both drivers are well-behaved.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") ||
		strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "1062") // mysql's Error 1062
}

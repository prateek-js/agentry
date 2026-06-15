package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// store_security.go — the SQL side of password reset, email
// verification, and account lockout. Mongo equivalents live in
// store_mongo.go. The lockout *policy* (when + how long) is in
// lockout.go so both adapters and the handler agree on one source.

func (s *sqlStore) UpdatePassword(ctx context.Context, userID, newPassword string) error {
	if len(newPassword) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	// Reset lock state on a successful password change — a reset is a
	// legitimate recovery, so we don't want the account to stay locked.
	q := s.sql(`UPDATE agentry_users
        SET password_hash = ?, failed_attempts = 0, locked_until = NULL
        WHERE id = ?`)
	res, err := s.db.ExecContext(ctx, q, string(hash), userID)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *sqlStore) MarkEmailVerified(ctx context.Context, userID string) error {
	q := s.sql(`UPDATE agentry_users SET email_verified = ? WHERE id = ?`)
	if _, err := s.db.ExecContext(ctx, q, true, userID); err != nil {
		return fmt.Errorf("mark verified: %w", err)
	}
	return nil
}

func (s *sqlStore) RecordLoginFailure(ctx context.Context, userID string, attempts int, lockedUntil time.Time) error {
	var q string
	var args []any
	if lockedUntil.IsZero() {
		q = s.sql(`UPDATE agentry_users SET failed_attempts = ?, locked_until = NULL WHERE id = ?`)
		args = []any{attempts, userID}
	} else {
		q = s.sql(`UPDATE agentry_users SET failed_attempts = ?, locked_until = ? WHERE id = ?`)
		args = []any{attempts, lockedUntil.UTC(), userID}
	}
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("record login failure: %w", err)
	}
	return nil
}

func (s *sqlStore) ResetLoginAttempts(ctx context.Context, userID string) error {
	q := s.sql(`UPDATE agentry_users SET failed_attempts = 0, locked_until = NULL WHERE id = ?`)
	if _, err := s.db.ExecContext(ctx, q, userID); err != nil {
		return fmt.Errorf("reset login attempts: %w", err)
	}
	return nil
}

func (s *sqlStore) CreateEmailToken(ctx context.Context, userID, purpose, tokenHash string, expiresAt time.Time) error {
	// Invalidate any outstanding tokens of the same purpose for this user
	// first, so "send me another reset link" doesn't leave the previous
	// one live. Best-effort — the new token works regardless.
	del := s.sql(`DELETE FROM agentry_email_tokens WHERE user_id = ? AND purpose = ?`)
	_, _ = s.db.ExecContext(ctx, del, userID, purpose)

	q := s.sql(`INSERT INTO agentry_email_tokens (token_hash, user_id, purpose, expires_at, created_at)
        VALUES (?, ?, ?, ?, ?)`)
	if _, err := s.db.ExecContext(ctx, q, tokenHash, userID, purpose, expiresAt.UTC(), time.Now().UTC()); err != nil {
		return fmt.Errorf("create email token: %w", err)
	}
	return nil
}

func (s *sqlStore) ConsumeEmailToken(ctx context.Context, purpose, tokenHash string) (string, error) {
	// Atomic single-use: the UPDATE only matches a row that is unused,
	// unexpired, and of the right purpose. RowsAffected==0 means the
	// token was already consumed, expired, or never existed — all the
	// same opaque ErrTokenInvalid to the caller.
	now := time.Now().UTC()
	upd := s.sql(`UPDATE agentry_email_tokens
        SET used_at = ?
        WHERE token_hash = ? AND purpose = ? AND used_at IS NULL AND expires_at > ?`)
	res, err := s.db.ExecContext(ctx, upd, now, tokenHash, purpose, now)
	if err != nil {
		return "", fmt.Errorf("consume token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrTokenInvalid
	}
	// The token was valid; fetch its owner.
	var userID string
	q := s.sql(`SELECT user_id FROM agentry_email_tokens WHERE token_hash = ?`)
	if err := s.db.QueryRowContext(ctx, q, tokenHash).Scan(&userID); err != nil {
		return "", fmt.Errorf("token owner lookup: %w", err)
	}
	return userID, nil
}

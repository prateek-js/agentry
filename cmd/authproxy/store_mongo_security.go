package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"golang.org/x/crypto/bcrypt"
)

// store_mongo_security.go — mongo side of password reset, verification,
// and lockout. Mirrors store_security.go (the SQL adapter); the shared
// lockout policy lives in lockout.go.

func (s *mongoStore) UpdatePassword(ctx context.Context, userID, newPassword string) error {
	if len(newPassword) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	res, err := s.coll.UpdateByID(ctx, userID, bson.M{
		"$set":   bson.M{"password_hash": string(hash), "failed_attempts": 0},
		"$unset": bson.M{"locked_until": ""},
	})
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if res.MatchedCount == 0 {
		return sqlNoRows
	}
	return nil
}

func (s *mongoStore) MarkEmailVerified(ctx context.Context, userID string) error {
	if _, err := s.coll.UpdateByID(ctx, userID, bson.M{"$set": bson.M{"email_verified": true}}); err != nil {
		return fmt.Errorf("mark verified: %w", err)
	}
	return nil
}

func (s *mongoStore) RecordLoginFailure(ctx context.Context, userID string, attempts int, lockedUntil time.Time) error {
	update := bson.M{"$set": bson.M{"failed_attempts": attempts}}
	if lockedUntil.IsZero() {
		update["$unset"] = bson.M{"locked_until": ""}
	} else {
		update["$set"].(bson.M)["locked_until"] = lockedUntil.UTC()
	}
	if _, err := s.coll.UpdateByID(ctx, userID, update); err != nil {
		return fmt.Errorf("record login failure: %w", err)
	}
	return nil
}

func (s *mongoStore) ResetLoginAttempts(ctx context.Context, userID string) error {
	if _, err := s.coll.UpdateByID(ctx, userID, bson.M{
		"$set":   bson.M{"failed_attempts": 0},
		"$unset": bson.M{"locked_until": ""},
	}); err != nil {
		return fmt.Errorf("reset login attempts: %w", err)
	}
	return nil
}

func (s *mongoStore) CreateEmailToken(ctx context.Context, userID, purpose, tokenHash string, expiresAt time.Time) error {
	// Drop any outstanding tokens of this purpose for the user first.
	_, _ = s.tokens.DeleteMany(ctx, bson.M{"user_id": userID, "purpose": purpose})
	_, err := s.tokens.InsertOne(ctx, bson.M{
		"_id":        tokenHash,
		"user_id":    userID,
		"purpose":    purpose,
		"expires_at": expiresAt.UTC(),
		"created_at": time.Now().UTC(),
		// used_at omitted → absent means "unused"; presence means consumed.
	})
	if err != nil {
		return fmt.Errorf("create email token: %w", err)
	}
	return nil
}

func (s *mongoStore) ConsumeEmailToken(ctx context.Context, purpose, tokenHash string) (string, error) {
	now := time.Now().UTC()
	// Atomic find-and-mark: only a row that's unused, unexpired, and of
	// the right purpose matches. used_at is set so it can't be replayed.
	filter := bson.M{
		"_id":        tokenHash,
		"purpose":    purpose,
		"used_at":    bson.M{"$exists": false},
		"expires_at": bson.M{"$gt": now},
	}
	var doc bson.M
	err := s.tokens.FindOneAndUpdate(ctx, filter, bson.M{"$set": bson.M{"used_at": now}}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return "", ErrTokenInvalid
		}
		return "", fmt.Errorf("consume token: %w", err)
	}
	userID, _ := doc["user_id"].(string)
	if userID == "" {
		return "", ErrTokenInvalid
	}
	return userID, nil
}

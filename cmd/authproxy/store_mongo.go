package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/crypto/bcrypt"
)

// store_mongo.go — mongo Store adapter. Parallel to sqlStore but
// against a single collection `agentry_users` in whichever database
// the connection URL's path component selects (e.g.
// mongodb://host:27017/myapp → db=myapp). When the URL has no path,
// we default to `agentry`.
//
// Document shape (same fields as the SQL row):
//
//	_id           string (32 hex) — primary key, doubles as user id
//	email         string — unique
//	password_hash string — bcrypt; empty for OAuth-only accounts
//	name          string
//	provider      string — "password" | "google" | …
//	provider_id   string — upstream stable id; empty for password
//	created_at    time.Time
//
// Two unique indexes get created at first connect:
//
//	{ email: 1 }                  unique
//	{ provider: 1, provider_id: 1 } unique, sparse
//
// Mongo's natural deduplication semantics let us write straight
// upserts; we don't need the read-before-write dance the SQL adapter
// uses for OAuth linking.

const mongoCollection = "agentry_users"
const mongoTokenCollection = "agentry_email_tokens"
const mongoDefaultDB = "agentry"

type mongoStore struct {
	client *mongo.Client
	coll   *mongo.Collection
	tokens *mongo.Collection
}

// openMongoStore dials mongo, picks the database, ensures indexes,
// returns the Store. Caller owns Close().
func openMongoStore(url, suffix string) (Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := options.Client().ApplyURI(url)
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("connect mongo: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("ping mongo: %w", err)
	}

	dbName := databaseFromURI(url)
	db := client.Database(dbName)
	// Per-app collections — see appSuffix. Two apps sharing the same
	// Mongo binding never share a users collection.
	collName, tokenName := mongoCollection, mongoTokenCollection
	if suffix != "" {
		collName = mongoCollection + "_" + suffix
		tokenName = mongoTokenCollection + "_" + suffix
	}
	coll := db.Collection(collName)
	tokens := db.Collection(tokenName)

	s := &mongoStore{client: client, coll: coll, tokens: tokens}
	if err := s.ensureIndexes(ctx); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return s, nil
}

// databaseFromURI honors the path component of the URI when present
// (mongodb://host/dbname), otherwise falls back to "agentry". Same
// rule the official driver applies internally — we mirror it so the
// fallback is explicit + testable.
func databaseFromURI(uri string) string {
	idx := strings.Index(uri, "://")
	if idx == -1 {
		return mongoDefaultDB
	}
	rest := uri[idx+3:]
	// Strip query string.
	if q := strings.Index(rest, "?"); q != -1 {
		rest = rest[:q]
	}
	slash := strings.Index(rest, "/")
	if slash == -1 || slash == len(rest)-1 {
		return mongoDefaultDB
	}
	name := rest[slash+1:]
	if name == "" {
		return mongoDefaultDB
	}
	return name
}

// ensureIndexes runs the two unique-index creates idempotently. The
// driver silently no-ops on existing indexes with the same spec, so
// every boot is safe.
func (s *mongoStore) ensureIndexes(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "email", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("email_unique"),
		},
		{
			// Provider + provider_id uniqueness. Sparse so password-only
			// accounts (provider="password", provider_id="") don't all
			// collide on the empty string.
			Keys: bson.D{
				{Key: "provider", Value: 1},
				{Key: "provider_id", Value: 1},
			},
			Options: options.Index().
				SetUnique(true).
				SetName("provider_id_unique").
				SetSparse(true),
		},
	})
	if err != nil {
		return fmt.Errorf("create indexes: %w", err)
	}
	// Email-token collection: index the owner+purpose lookup, and a TTL
	// on expires_at so consumed/expired tokens are reaped automatically
	// (mongo's TTL monitor deletes them ~once a minute after expiry).
	_, err = s.tokens.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "purpose", Value: 1}}, Options: options.Index().SetName("user_purpose")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("ttl_expires").SetExpireAfterSeconds(0)},
	})
	if err != nil {
		return fmt.Errorf("create token indexes: %w", err)
	}
	return nil
}

func (s *mongoStore) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.client.Disconnect(ctx)
}

// CreateUserPassword inserts a fresh password-auth user. Uses
// InsertOne so a duplicate email naturally surfaces as the driver's
// duplicate-key error, which we translate into ErrEmailTaken.
func (s *mongoStore) CreateUserPassword(ctx context.Context, email, password, name string) (*User, error) {
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
	doc := bson.M{
		"_id":             u.ID,
		"email":           u.Email,
		"password_hash":   u.PasswordHash,
		"name":            u.Name,
		"provider":        u.Provider,
		"provider_id":     "",
		"created_at":      u.CreatedAt,
		"email_verified":  false,
		"failed_attempts": 0,
	}
	if _, err := s.coll.InsertOne(ctx, doc); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}
	return u, nil
}

// GetUserByEmail loads the password-login candidate.
func (s *mongoStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var doc bson.M
	err := s.coll.FindOne(ctx, bson.M{"email": email}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			// Map to the same error shape as the SQL adapter so the
			// handler's "invalid email or password" branch fires.
			return nil, sqlNoRows
		}
		return nil, err
	}
	return userFromDoc(doc), nil
}

// UpsertUserFromOAuth handles three cases identically to sqlStore:
// provider+id hit → return + refresh name; email hit → attach
// provider; nothing → create.
//
// Mongo's findOneAndUpdate-with-upsert *would* be one round trip, but
// we want different behavior in each branch (refresh name only, attach
// provider, create from scratch). Two reads + one write is fine.
func (s *mongoStore) UpsertUserFromOAuth(ctx context.Context, provider, providerID, email, name string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)

	// 1. Try provider + provider_id.
	var doc bson.M
	err := s.coll.FindOne(ctx, bson.M{
		"provider":    provider,
		"provider_id": providerID,
	}).Decode(&doc)
	if err == nil {
		u := userFromDoc(doc)
		if name != "" && name != u.Name {
			_, _ = s.coll.UpdateByID(ctx, u.ID, bson.M{"$set": bson.M{"name": name}})
			u.Name = name
		}
		return u, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return nil, err
	}

	// 2. Try by email — link OAuth identity onto an existing account.
	err = s.coll.FindOne(ctx, bson.M{"email": email}).Decode(&doc)
	if err == nil {
		u := userFromDoc(doc)
		update := bson.M{"$set": bson.M{
			"provider":    provider,
			"provider_id": providerID,
		}}
		if name != "" {
			update["$set"].(bson.M)["name"] = name
		}
		if _, e := s.coll.UpdateByID(ctx, u.ID, update); e != nil {
			return nil, e
		}
		u.Provider = provider
		u.ProviderID = providerID
		if name != "" {
			u.Name = name
		}
		return u, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return nil, err
	}

	// 3. Brand-new user.
	uid, err := newUserID()
	if err != nil {
		return nil, err
	}
	u := &User{
		ID:         uid,
		Email:      email,
		Name:       name,
		Provider:   provider,
		ProviderID: providerID,
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := s.coll.InsertOne(ctx, bson.M{
		"_id":           u.ID,
		"email":         u.Email,
		"password_hash": "",
		"name":          u.Name,
		"provider":      u.Provider,
		"provider_id":   u.ProviderID,
		"created_at":    u.CreatedAt,
	}); err != nil {
		return nil, fmt.Errorf("insert oauth user: %w", err)
	}
	return u, nil
}

// userFromDoc decodes a bson document into the shared User struct.
// Keeps the field-name mapping in one place — if we ever rename a
// column, it changes here once.
func userFromDoc(doc bson.M) *User {
	u := &User{}
	if v, ok := doc["_id"].(string); ok {
		u.ID = v
	}
	if v, ok := doc["email"].(string); ok {
		u.Email = v
	}
	if v, ok := doc["password_hash"].(string); ok {
		u.PasswordHash = v
	}
	if v, ok := doc["name"].(string); ok {
		u.Name = v
	}
	if v, ok := doc["provider"].(string); ok {
		u.Provider = v
	}
	if v, ok := doc["provider_id"].(string); ok {
		u.ProviderID = v
	}
	if v, ok := doc["created_at"].(bson.DateTime); ok {
		u.CreatedAt = v.Time()
	}
	if v, ok := doc["email_verified"].(bool); ok {
		u.EmailVerified = v
	}
	switch v := doc["failed_attempts"].(type) {
	case int32:
		u.FailedAttempts = int(v)
	case int64:
		u.FailedAttempts = int(v)
	case int:
		u.FailedAttempts = v
	}
	if v, ok := doc["locked_until"].(bson.DateTime); ok {
		t := v.Time().UTC()
		u.LockedUntil = &t
	}
	return u
}

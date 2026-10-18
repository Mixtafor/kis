package auth

import (
	"crypto/rand"
	"errors"
	"time"

	"edition/internal/news"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "edition-server"
const audience = "edition-api"

type Tokens struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

func New(secret string, ttl time.Duration) (*Tokens, error) {
	if len(secret) < 32 || len(secret) > 256 {
		return nil, errors.New("JWT_SECRET must have 32..256 characters")
	}
	if ttl < time.Minute || ttl > 30*24*time.Hour {
		return nil, errors.New("token lifetime must be between 1m and 720h")
	}
	return &Tokens{key: []byte(secret), ttl: ttl, now: time.Now}, nil
}

func (t *Tokens) Issue(clientID string) (string, time.Time, error) {
	now := t.now().UTC()
	expires := jwt.NewNumericDate(now.Add(t.ttl))
	claims := jwt.RegisteredClaims{Issuer: issuer, Audience: jwt.ClaimStrings{audience}, Subject: clientID, IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: expires, ID: rand.Text()}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.key)
	return token, expires.Time, err
}

func (t *Tokens) Subject(raw string) (string, error) {
	if len(raw) > 4096 {
		return "", news.ErrUnauthorized
	}
	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return t.key, nil }, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(t.now))
	if err != nil || !token.Valid || claims.Subject == "" || claims.IssuedAt == nil || claims.NotBefore == nil || claims.ID == "" {
		return "", news.ErrUnauthorized
	}
	return claims.Subject, nil
}

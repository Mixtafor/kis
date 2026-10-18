package auth

import (
	"errors"
	"testing"
	"time"

	"edition/internal/news"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "a-test-secret-of-at-least-thirty-two-characters"

var fixedNow = time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)

func tokensAt(t *testing.T) *Tokens {
	t.Helper()
	tokens, err := New(testSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tokens.now = func() time.Time { return fixedNow }
	return tokens
}

func sign(t *testing.T, method jwt.SigningMethod, claims jwt.Claims, key any) string {
	t.Helper()
	raw, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": "edition-server", "aud": []string{"edition-api"}, "sub": "reporter",
		"iat": fixedNow.Unix(), "nbf": fixedNow.Unix(), "exp": fixedNow.Add(time.Hour).Unix(), "jti": "token-id",
	}
}

func TestIssueAndExpiration(t *testing.T) {
	tokens := tokensAt(t)
	raw, expires, err := tokens.Issue("reporter")
	if err != nil {
		t.Fatal(err)
	}
	subject, err := tokens.Subject(raw)
	if err != nil || subject != "reporter" || !expires.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("subject=%q expires=%v err=%v", subject, expires, err)
	}
	tokens.now = func() time.Time { return expires }
	if subject, err := tokens.Subject(raw); subject != "" || !errors.Is(err, news.ErrUnauthorized) {
		t.Fatalf("expired token: subject=%q err=%v", subject, err)
	}
}

// Проверяем подпись, алгоритм, издателя, получателя и обязательные поля JWT.
func TestInvalidTokens(t *testing.T) {
	tokens := tokensAt(t)
	wrongIssuer, wrongAudience := validClaims(), validClaims()
	wrongIssuer["iss"], wrongAudience["aud"] = "another-server", []string{"another-api"}
	type tokenCase struct{ name, raw string }
	cases := []tokenCase{
		{"empty", ""},
		{"malformed", "not-a-jwt"},
		{"wrong_key", sign(t, jwt.SigningMethodHS256, validClaims(), []byte("another-signing-key"))},
		{"wrong_algorithm", sign(t, jwt.SigningMethodHS384, validClaims(), []byte(testSecret))},
		{"unsigned", sign(t, jwt.SigningMethodNone, validClaims(), jwt.UnsafeAllowNoneSignatureType)},
		{"wrong_issuer", sign(t, jwt.SigningMethodHS256, wrongIssuer, []byte(testSecret))},
		{"wrong_audience", sign(t, jwt.SigningMethodHS256, wrongAudience, []byte(testSecret))},
	}
	for _, field := range []string{"iss", "aud", "sub", "exp", "iat", "nbf", "jti"} {
		claims := validClaims()
		delete(claims, field)
		cases = append(cases, tokenCase{"missing_" + field, sign(t, jwt.SigningMethodHS256, claims, []byte(testSecret))})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if subject, err := tokens.Subject(tc.raw); subject != "" || !errors.Is(err, news.ErrUnauthorized) {
				t.Fatalf("subject=%q err=%v", subject, err)
			}
		})
	}
}

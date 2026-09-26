// Package session verifies the signed session tokens issued by the frontend.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The platform verifies the SAME signed session token the frontend issues
// (see frontend/src/lib/session.ts): base64url(payload) + "." +
// base64url(HMAC-SHA256(body, SESSION_SECRET)). Sharing SESSION_SECRET lets the
// platform authenticate users without its own login flow.

// User is the identity carried in a session.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatarUrl"`
}

type payload struct {
	User User  `json:"user"`
	Exp  int64 `json:"exp"` // unix seconds
}

// ErrInvalid is returned for a malformed, forged or expired session token.
var ErrInvalid = errors.New("invalid session")

// Verify checks a session token's signature and expiry and returns the
// user. secret is the shared SESSION_SECRET.
func Verify(secret, token string) (*User, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || body == "" || sig == "" {
		return nil, ErrInvalid
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return nil, ErrInvalid
	}

	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrInvalid
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrInvalid
	}
	if p.Exp < time.Now().Unix() {
		return nil, ErrInvalid
	}
	if p.User.Login == "" {
		return nil, ErrInvalid
	}
	return &p.User, nil
}

package main

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

type sessionUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatarUrl"`
}

type sessionPayload struct {
	User sessionUser `json:"user"`
	Exp  int64       `json:"exp"` // unix seconds
}

var errBadSession = errors.New("invalid session")

// verifySession checks a session token's signature and expiry and returns the
// user. secret is the shared SESSION_SECRET.
func verifySession(secret, token string) (*sessionUser, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || body == "" || sig == "" {
		return nil, errBadSession
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return nil, errBadSession
	}

	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errBadSession
	}
	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errBadSession
	}
	if p.Exp < time.Now().Unix() {
		return nil, errBadSession
	}
	if p.User.Login == "" {
		return nil, errBadSession
	}
	return &p.User, nil
}

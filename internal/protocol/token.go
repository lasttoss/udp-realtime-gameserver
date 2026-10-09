package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claims is what the auth endpoint hands to a client and the relay verifies.
type Claims struct {
	PlayerID string `json:"player_id"`
	Region   string `json:"region"`
	GameID   string `json:"game_id"`
	Expires  int64  `json:"exp"` // unix seconds
}

// TokenError distinguishes the reasons a token was refused, so the relay can tell a client
// "your token expired" instead of dropping the packet.
type TokenError struct {
	Reason string
}

func (e *TokenError) Error() string { return "token: " + e.Reason }

var ErrMalformedToken = &TokenError{Reason: "malformed"}

// SignToken produces "<base64url(payload)>.<hex(hmac)>".
//
// The relay has to trust the caller is who they say they are, but it must not become the
// authority on identity: it verifies a short lived signed ticket minted by the game's auth
// service instead of holding accounts, passwords or sessions of its own.
func SignToken(secret []byte, claims Claims) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("token: the signing secret cannot be empty")
	}
	if claims.PlayerID == "" {
		return "", errors.New("token: player_id is required")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + signature(secret, encoded), nil
}

// VerifyToken checks the signature and the expiry, and returns the claims it carries.
func VerifyToken(secret []byte, token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Claims{}, ErrMalformedToken
	}
	if !hmac.Equal([]byte(signature(secret, parts[0])), []byte(parts[1])) {
		return Claims{}, &TokenError{Reason: "bad signature"}
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformedToken
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, ErrMalformedToken
	}
	if claims.PlayerID == "" {
		return Claims{}, &TokenError{Reason: "no player_id"}
	}
	if claims.Expires == 0 {
		return Claims{}, &TokenError{Reason: "no expiry"}
	}
	if now.Unix() > claims.Expires {
		return Claims{}, &TokenError{Reason: fmt.Sprintf("expired %s ago", now.Sub(time.Unix(claims.Expires, 0)).Truncate(time.Second))}
	}
	return claims, nil
}

func signature(secret []byte, encodedPayload string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encodedPayload))
	return hex.EncodeToString(mac.Sum(nil))
}

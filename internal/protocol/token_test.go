package protocol

import (
	"strings"
	"testing"
	"time"
)

func TestSignAndVerifyToken(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	token, err := SignToken(secret, Claims{
		PlayerID: "player-1", Region: "sea", GameID: "tag-arena",
		Expires: now.Add(10 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("SignToken() error = %v", err)
	}

	claims, err := VerifyToken(secret, token, now)
	if err != nil {
		t.Fatalf("VerifyToken() error = %v", err)
	}
	if claims.PlayerID != "player-1" || claims.Region != "sea" || claims.GameID != "tag-arena" {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestVerifyTokenRejectsAnExpiredTicketAndSaysSo(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	token, err := SignToken(secret, Claims{PlayerID: "p", Expires: now.Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatalf("SignToken() error = %v", err)
	}

	_, err = VerifyToken(secret, token, now)
	if err == nil {
		t.Fatal("an expired ticket was accepted")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %q, want it to mention the expiry", err.Error())
	}
}

func TestVerifyTokenRejectsATamperedPayload(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Now()

	token, err := SignToken(secret, Claims{PlayerID: "player-1", Expires: now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("SignToken() error = %v", err)
	}

	parts := strings.Split(token, ".")
	// swap the payload for one that claims to be somebody else, keep the old signature
	forged, err := SignToken(secret, Claims{PlayerID: "player-2", Expires: now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("SignToken() error = %v", err)
	}
	tampered := strings.Split(forged, ".")[0] + "." + parts[1]

	if _, err := VerifyToken(secret, tampered, now); err == nil {
		t.Fatal("a token with a mismatched signature was accepted")
	}
}

func TestVerifyTokenRejectsAnotherSecret(t *testing.T) {
	now := time.Now()
	token, err := SignToken([]byte("secret-a"), Claims{PlayerID: "p", Expires: now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("SignToken() error = %v", err)
	}
	if _, err := VerifyToken([]byte("secret-b"), token, now); err == nil {
		t.Fatal("a token signed with a different secret was accepted")
	}
}

func TestVerifyTokenRejectsMalformedInput(t *testing.T) {
	now := time.Now()
	secret := []byte("test-secret")

	for _, token := range []string{"", ".", "onlyonepart", "a.b.c", "!!!.###"} {
		if _, err := VerifyToken(secret, token, now); err == nil {
			t.Fatalf("VerifyToken(%q) was accepted", token)
		}
	}
}

func TestSignTokenRequiresASecretAndAPlayer(t *testing.T) {
	if _, err := SignToken(nil, Claims{PlayerID: "p"}); err == nil {
		t.Fatal("an empty secret was accepted")
	}
	if _, err := SignToken([]byte("s"), Claims{}); err == nil {
		t.Fatal("a ticket without a player_id was signed")
	}
}

func TestVerifyTokenRejectsATicketWithoutExpiry(t *testing.T) {
	secret := []byte("test-secret")
	token, err := SignToken(secret, Claims{PlayerID: "p"})
	if err != nil {
		t.Fatalf("SignToken() error = %v", err)
	}
	if _, err := VerifyToken(secret, token, time.Now()); err == nil {
		t.Fatal("a ticket with no expiry was accepted")
	}
}

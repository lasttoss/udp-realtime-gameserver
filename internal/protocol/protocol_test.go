package protocol

import (
	"bytes"
	"testing"
	"time"
)

func TestHelloRoundTrip(t *testing.T) {
	frame, err := EncodeHello("player.ticket")
	if err != nil {
		t.Fatalf("EncodeHello() error = %v", err)
	}
	got, err := DecodeHello(frame)
	if err != nil {
		t.Fatalf("DecodeHello() error = %v", err)
	}
	if got.Token != "player.ticket" {
		t.Fatalf("Token = %q, want %q", got.Token, "player.ticket")
	}
}

func TestHelloRejectsAnEmptyOrOversizedToken(t *testing.T) {
	if _, err := EncodeHello(""); err == nil {
		t.Fatal("EncodeHello(\"\") was accepted")
	}
	long := make([]byte, MaxTokenBytes+1)
	if _, err := EncodeHello(string(long)); err == nil {
		t.Fatal("an oversized token was accepted")
	}
}

func TestInputRoundTripKeepsTheSignedValues(t *testing.T) {
	in := Input{Seq: 65535, X: -12345, Y: 32767}
	got, err := DecodeInput(EncodeInput(in))
	if err != nil {
		t.Fatalf("DecodeInput() error = %v", err)
	}
	if got != in {
		t.Fatalf("decoded %+v, want %+v", got, in)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	players := []PlayerState{
		{ID: 1, Flags: FlagGhost, X: -100, Y: 200},
		{ID: 2, Flags: 0, X: 3000, Y: -4000},
	}
	frame := EncodeSnapshot(42, 7, players)

	if len(frame) != 8+len(players)*7 {
		t.Fatalf("frame is %d bytes, want %d: one player costs 7 bytes on the wire", len(frame), 8+len(players)*7)
	}

	got, err := DecodeSnapshot(frame)
	if err != nil {
		t.Fatalf("DecodeSnapshot() error = %v", err)
	}
	if got.Tick != 42 || got.AckSeq != 7 {
		t.Fatalf("header = tick %d ack %d, want 42/7", got.Tick, got.AckSeq)
	}
	if len(got.Players) != 2 || got.Players[0] != players[0] || got.Players[1] != players[1] {
		t.Fatalf("players = %+v, want %+v", got.Players, players)
	}
}

func TestPongAndErrorRoundTrip(t *testing.T) {
	sent := uint64(time.Now().UnixNano())
	clientTime, tick, err := DecodePong(EncodePong(sent, 99))
	if err != nil {
		t.Fatalf("DecodePong() error = %v", err)
	}
	if clientTime != sent || tick != 99 {
		t.Fatalf("pong = %d/%d, want %d/99", clientTime, tick, sent)
	}

	code, message, err := ErrorMessage(EncodeError(ErrRoomFull, "no room"))
	if err != nil {
		t.Fatalf("ErrorMessage() error = %v", err)
	}
	if code != ErrRoomFull || message != "no room" {
		t.Fatalf("error frame = %d %q, want %d %q", code, message, ErrRoomFull, "no room")
	}
}

func TestDecodersRejectShortFrames(t *testing.T) {
	cases := map[string]func([]byte) error{
		"hello":    func(b []byte) error { _, err := DecodeHello(b); return err },
		"input":    func(b []byte) error { _, err := DecodeInput(b); return err },
		"snapshot": func(b []byte) error { _, err := DecodeSnapshot(b); return err },
		"pong":     func(b []byte) error { _, _, err := DecodePong(b); return err },
	}

	for name, decode := range cases {
		t.Run(name, func(t *testing.T) {
			if err := decode(nil); err == nil {
				t.Fatal("an empty frame was accepted")
			}
			truncated := []byte{KindSnapshot, 0, 0, 0, 1, 0, 0, 5}
			if name == "snapshot" {
				if err := decode(truncated); err == nil {
					t.Fatal("a snapshot claiming 5 players in 8 bytes was accepted")
				}
			}
		})
	}
}

func TestClampKeepsServerAuthorityOverMovement(t *testing.T) {
	maxStep := int16(MaxSpeed / TickRate)

	// a legal step is passed through untouched
	x, y, clamped := Clamp(0, 0, maxStep-1, 0)
	if clamped {
		t.Fatal("a step within the speed limit was clamped")
	}
	if x != maxStep-1 || y != 0 {
		t.Fatalf("legal step changed to %d,%d", x, y)
	}

	// teleporting is cut back to the maximum distance for one tick
	x, y, clamped = Clamp(0, 0, 30000, 30000)
	if !clamped {
		t.Fatal("a teleport was not clamped")
	}
	dist := sqrt(float64(x)*float64(x) + float64(y)*float64(y))
	if dist > float64(maxStep)+1 {
		t.Fatalf("clamped distance %.1f exceeds the %.1f cm budget", dist, float64(maxStep))
	}

	// the direction is preserved, only the magnitude is cut
	if x <= 0 || y <= 0 {
		t.Fatalf("the clamped direction was lost: %d,%d", x, y)
	}
}

func TestClampAllowsMaxSpeedExactly(t *testing.T) {
	maxStep := MaxSpeed / TickRate
	if _, _, clamped := Clamp(0, 0, int16(maxStep), 0); clamped {
		t.Fatalf("a step of exactly %d cm was clamped", maxStep)
	}
	if _, _, clamped := Clamp(0, 0, int16(maxStep+1), 0); !clamped {
		t.Fatalf("a step of %d cm was allowed", maxStep+1)
	}
}

func TestQuantizeSaturatesInsteadOfWrapping(t *testing.T) {
	if got := Quantize(1e9); got != 32767 {
		t.Fatalf("Quantize(1e9) = %d, want 32767 (saturating, not wrapping)", got)
	}
	if got := Quantize(-1e9); got != -32768 {
		t.Fatalf("Quantize(-1e9) = %d, want -32768", got)
	}
}

func TestKindOfAnEmptyPacketIsAnError(t *testing.T) {
	if _, err := Kind(nil); err == nil {
		t.Fatal("Kind(nil) was accepted")
	}
	if kind, _ := Kind(EncodeInput(Input{})); kind != KindInput {
		t.Fatalf("Kind() = %d, want %d", kind, KindInput)
	}
	if !bytes.Equal(EncodeInput(Input{}), EncodeInput(Input{})) {
		t.Fatal("EncodeInput is not deterministic")
	}
}

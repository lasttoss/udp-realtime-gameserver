package protocol

import (
	"math"
	"testing"
)

// TestWelcomeRoundTrips is the frame the load test blamed for 34k clamped inputs: a client that
// does not learn where the relay put it will be corrected on every single input, so the fields
// have to survive the wire exactly.
func TestWelcomeRoundTrips(t *testing.T) {
	frame := EncodeWelcome(7, 3, -1234, 4321, 1_700_000_000_123)

	w, err := DecodeWelcome(frame)
	if err != nil {
		t.Fatalf("DecodeWelcome() error = %v", err)
	}
	if w.PlayerID != 7 || w.RoomID != 3 {
		t.Errorf("player/room = %d/%d, want 7/3", w.PlayerID, w.RoomID)
	}
	if w.TickRate != TickRate {
		t.Errorf("tick rate = %d, want %d", w.TickRate, TickRate)
	}
	if w.SpawnX != -1234 || w.SpawnY != 4321 {
		t.Errorf("spawn = (%d,%d), want (-1234,4321)", w.SpawnX, w.SpawnY)
	}
	if w.ServerTime != 1_700_000_000_123 {
		t.Errorf("server time = %d, want 1700000000123", w.ServerTime)
	}
}

func TestDecodeWelcomeRefusesATruncatedFrame(t *testing.T) {
	frame := EncodeWelcome(1, 1, 0, 0, 0)
	for _, cut := range []int{0, 1, len(frame) - 1} {
		if _, err := DecodeWelcome(frame[:cut]); err == nil {
			t.Errorf("DecodeWelcome() accepted a frame truncated to %d bytes", cut)
		}
	}
	if _, err := DecodeWelcome(append([]byte{}, frame...)[:len(frame)-1]); err == nil {
		t.Error("DecodeWelcome() accepted a frame one byte short")
	}
}

// TestPingPongEchoesTheClientClock: the relay does not keep the client's clock, it hands it back,
// which is what the bot swarm turns into its RTT percentiles.
func TestPingPongEchoesTheClientClock(t *testing.T) {
	const sent = uint64(1_700_000_000_999_123)
	pong := EncodePong(sent, 4242)

	got, tick, err := DecodePong(pong)
	if err != nil {
		t.Fatalf("DecodePong() error = %v", err)
	}
	if got != sent {
		t.Errorf("echoed clock = %d, want %d", got, sent)
	}
	if tick != 4242 {
		t.Errorf("server tick = %d, want 4242", tick)
	}
	if len(EncodePing(sent)) != 9 {
		t.Errorf("PING frame = %d bytes, want 9", len(EncodePing(sent)))
	}
}

// TestInputFrameStaysSevenBytes is the whole point of the quantisation: 30 inputs a second per
// player has to be cheap, and one byte more here is one more megabit per second at 1000 players.
func TestInputFrameStaysSevenBytes(t *testing.T) {
	frame := EncodeInput(Input{Seq: 65535, X: -32768, Y: 32767})
	if len(frame) != 7 {
		t.Fatalf("INPUT frame = %d bytes, want 7", len(frame))
	}

	in, err := DecodeInput(frame)
	if err != nil {
		t.Fatalf("DecodeInput() error = %v", err)
	}
	if in.Seq != 65535 || in.X != -32768 || in.Y != 32767 {
		t.Errorf("decoded = %+v, want the extremes to survive", in)
	}
	if _, err := DecodeInput(frame[:6]); err == nil {
		t.Error("DecodeInput() accepted a six byte frame")
	}
}

// TestSnapshotCarriesEveryPlayerAndTheAck: AckSeq is what lets a client reconcile instead of
// walking while the server stands still, and a snapshot that silently drops a player would make
// that impossible to detect.
func TestSnapshotCarriesEveryPlayerAndTheAck(t *testing.T) {
	players := []PlayerState{
		{ID: 1, X: 100, Y: -200},
		{ID: 2, Flags: FlagGhost, X: 300, Y: 400},
		{ID: 3, Flags: FlagIdle | FlagGhost, X: 32767, Y: -32768},
	}
	frame := EncodeSnapshot(9_000_000, 1234, players)

	// header + 7 bytes per player: the cost of a tick is fixed and known
	if want := 8 + 7*len(players); len(frame) != want {
		t.Fatalf("SNAPSHOT frame = %d bytes, want %d", len(frame), want)
	}

	snap, err := DecodeSnapshot(frame)
	if err != nil {
		t.Fatalf("DecodeSnapshot() error = %v", err)
	}
	if snap.Tick != 9_000_000 {
		t.Errorf("tick = %d, want 9000000", snap.Tick)
	}
	if snap.AckSeq != 1234 {
		t.Errorf("ack seq = %d, want 1234", snap.AckSeq)
	}
	if len(snap.Players) != len(players) {
		t.Fatalf("players = %d, want %d", len(snap.Players), len(players))
	}
	for i, want := range players {
		if got := snap.Players[i]; got != want {
			t.Errorf("player %d = %+v, want %+v", i, got, want)
		}
	}

	// A frame that claims more players than it carries must be refused, not trusted: this is
	// where an out of bounds read would come from.
	lying := append(append([]byte{}, frame[:7]...), 200) // header + a count of 200 players
	if _, err := DecodeSnapshot(lying); err == nil {
		t.Error("DecodeSnapshot() trusted a count larger than the frame")
	}
}

// TestClampKeepsTheDirectionItWasGiven: clamping is a speed limit, not a stop, and a naive
// implementation that truncates the longer axis would quietly make diagonal movement impossible.
func TestClampKeepsTheDirectionItWasGiven(t *testing.T) {
	maxStep := int16(MaxSpeed / TickRate) // cm allowed in one tick

	// a legal step is passed through untouched
	if x, y, clamped := Clamp(0, 0, maxStep, 0); clamped || x != maxStep || y != 0 {
		t.Errorf("Clamp(a legal step) = (%d,%d,%v), want (%d,0,false)", x, y, clamped, maxStep)
	}

	// a diagonal step beyond the budget comes back at the budget, same heading
	for _, tc := range []struct{ dx, dy int16 }{{1000, 1000}, {-5000, 300}, {200, -9000}} {
		x, y, clamped := Clamp(0, 0, tc.dx, tc.dy)
		if !clamped {
			t.Errorf("Clamp(%d,%d) was not clamped", tc.dx, tc.dy)
			continue
		}
		dist := math.Hypot(float64(x), float64(y))
		if dist > float64(maxStep)+1 { // +1 for the centimetre rounding
			t.Errorf("Clamp(%d,%d) landed %0.1f cm away, budget is %d", tc.dx, tc.dy, dist, maxStep)
		}
		// the heading must survive within the centimetre rounding the wire format forces:
		// the sine of the angle between what was asked and what was granted
		asked, granted := math.Hypot(float64(tc.dx), float64(tc.dy)), math.Hypot(float64(x), float64(y))
		sin := math.Abs(float64(x)*float64(tc.dy)-float64(y)*float64(tc.dx)) / (asked * granted)
		if sin > 0.05 { // about three degrees
			t.Errorf("Clamp(%d,%d) = (%d,%d), which turned the heading by %0.1f degrees",
				tc.dx, tc.dy, x, y, math.Asin(sin)*180/math.Pi)
		}
	}
}

// TestSpawnPointsAreSpreadAroundTheArena: every client computes its spawn from the slot it was
// handed, so the function has to be deterministic and has to keep a full room from starting
// stacked on one point.
func TestSpawnPointsAreSpreadAroundTheArena(t *testing.T) {
	const radius = 500

	seen := map[[2]int16]bool{}
	for slot := 0; slot < MaxPlayersPerRoom; slot++ {
		x, y := SpawnPoint(slot)
		if x < -radius || x > radius || y < -radius || y > radius {
			t.Fatalf("SpawnPoint(%d) = (%d,%d), outside the %d cm arena", slot, x, y, radius)
		}
		seen[[2]int16{x, y}] = true

		if x2, y2 := SpawnPoint(slot); x2 != x || y2 != y {
			t.Errorf("SpawnPoint(%d) is not deterministic: (%d,%d) then (%d,%d)", slot, x, y, x2, y2)
		}
	}
	if len(seen) != MaxPlayersPerRoom {
		t.Errorf("%d distinct spawn points for a %d player room, want one each", len(seen), MaxPlayersPerRoom)
	}

	// the ring repeats once the room is full, which is what makes the slot the only input
	firstX, firstY := SpawnPoint(0)
	wrappedX, wrappedY := SpawnPoint(MaxPlayersPerRoom)
	if firstX != wrappedX || firstY != wrappedY {
		t.Errorf("SpawnPoint is not periodic with the room size: (%d,%d) then (%d,%d)", firstX, firstY, wrappedX, wrappedY)
	}
	// and the hand rolled trigonometry must not blow up at the ends of the ring
	for slot := 0; slot < 4*MaxPlayersPerRoom; slot++ {
		x, y := SpawnPoint(slot)
		if x == 0 && y == 0 {
			t.Errorf("SpawnPoint(%d) landed exactly on the centre", slot)
		}
	}
}

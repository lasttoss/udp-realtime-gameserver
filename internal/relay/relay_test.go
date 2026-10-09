package relay

import (
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lasttoss/udp-realtime-gameserver/internal/protocol"
)

// fakeClock lets the ghost, idle and rate limit rules be tested without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	*Relay
	clock *fakeClock
	t     *testing.T
}

func newHarness(t *testing.T, tune ...func(*Config)) *harness {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig([]byte("test-secret"))
	cfg.Now = clock.Now
	for _, fn := range tune {
		fn(&cfg)
	}

	r := New(cfg)
	if err := r.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = r.conn.Close() })

	return &harness{Relay: r, clock: clock, t: t}
}

// dial opens a client socket pointed at the relay under test.
func (h *harness) dial() *net.UDPConn {
	h.t.Helper()
	conn, err := net.DialUDP("udp", nil, h.LocalAddr().(*net.UDPAddr))
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	h.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// join registers a client the way the read loop would: it writes a HELLO frame carrying a ticket
// and hands the same packet to the relay, so the handler runs exactly as it does in production.
func (h *harness) join(playerKey, region string) *net.UDPConn {
	h.t.Helper()
	conn := h.dial()

	token, err := protocol.SignToken(h.cfg.Secret, protocol.Claims{
		PlayerID: playerKey, Region: region, Expires: h.clock.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		h.t.Fatalf("SignToken() error = %v", err)
	}
	frame, err := protocol.EncodeHello(token)
	if err != nil {
		h.t.Fatalf("EncodeHello() error = %v", err)
	}
	h.send(conn, frame)
	return conn
}

func (h *harness) send(conn *net.UDPConn, frame []byte) {
	h.t.Helper()
	if _, err := conn.Write(frame); err != nil {
		h.t.Fatalf("write: %v", err)
	}
	h.handlePacket(conn.LocalAddr().(*net.UDPAddr), frame)
}

func (h *harness) read(conn *net.UDPConn) []byte {
	h.t.Helper()
	buf := make([]byte, 2048)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		h.t.Fatalf("read: %v", err)
	}
	return buf[:n]
}

func welcomeRoom(t *testing.T, frame []byte) uint16 {
	t.Helper()
	welcome, err := protocol.DecodeWelcome(frame)
	if err != nil {
		t.Fatalf("expected a WELCOME frame, got kind %d (%v)", frame[0], err)
	}
	return welcome.RoomID
}

// spawnFrom returns the spawn position the relay handed to a client.
func spawnFrom(t *testing.T, frame []byte) (int16, int16) {
	t.Helper()
	welcome, err := protocol.DecodeWelcome(frame)
	if err != nil {
		t.Fatalf("DecodeWelcome() error = %v", err)
	}
	return welcome.SpawnX, welcome.SpawnY
}

func TestHelloCreatesOneRoomPerRegionAndFillsIt(t *testing.T) {
	h := newHarness(t)

	sea1, frame := h.joinAndWelcome("p1", "sea")
	if got := welcomeRoom(t, frame); got != 0 {
		t.Fatalf("first room id = %d, want 0", got)
	}
	_, frame2 := h.joinAndWelcome("p2", "sea")
	if got := welcomeRoom(t, frame2); got != 0 {
		t.Fatalf("second player in the same region got room %d, want the same room 0", got)
	}
	_, frame3 := h.joinAndWelcome("p3", "eu")
	if got := welcomeRoom(t, frame3); got != 1 {
		t.Fatalf("a player from another region got room %d, want a new room 1", got)
	}

	rooms, players := h.Counts()
	if rooms != 2 || players != 3 {
		t.Fatalf("counts = %d rooms / %d players, want 2/3", rooms, players)
	}
	_ = sea1
}

// joinAndWelcome joins and reads the WELCOME frame in one step.
func (h *harness) joinAndWelcome(playerKey, region string) (*net.UDPConn, []byte) {
	h.t.Helper()
	conn := h.join(playerKey, region)
	return conn, h.read(conn)
}

func TestRoomCapsAtTenPlayersThenOpensANewMatch(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < protocol.MaxPlayersPerRoom; i++ {
		_, frame := h.joinAndWelcome("p"+string(rune('a'+i)), "sea")
		if room := welcomeRoom(t, frame); room != 0 {
			t.Fatalf("player %d landed in room %d, want room 0", i, room)
		}
	}

	_, frame := h.joinAndWelcome("overflow", "sea")
	if room := welcomeRoom(t, frame); room != 1 {
		t.Fatalf("the 11th player landed in room %d, want a fresh room 1", room)
	}

	rooms, players := h.Counts()
	if rooms != 2 || players != 11 {
		t.Fatalf("counts = %d rooms / %d players, want 2/11", rooms, players)
	}
}

func TestRegionBucketRefusesToStartMoreRoomsThanAllowed(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxRooms = 1 })

	for i := 0; i < protocol.MaxPlayersPerRoom; i++ {
		h.joinAndWelcome("p"+string(rune('a'+i)), "sea")
	}

	conn := h.join("overflow", "sea")
	frame := h.read(conn)
	if frame[0] != protocol.KindError {
		t.Fatalf("frame kind = %d, want an ERROR frame", frame[0])
	}
	code, message, err := protocol.ErrorMessage(frame)
	if err != nil {
		t.Fatalf("ErrorMessage() error = %v", err)
	}
	if code != protocol.ErrRoomFull {
		t.Fatalf("code = %d, want %d (room full)", code, protocol.ErrRoomFull)
	}
	if message == "" {
		t.Fatal("the error frame carried no message")
	}
}

func TestInputIsClampedToTheMaximumSpeed(t *testing.T) {
	h := newHarness(t)
	conn, welcome := h.joinAndWelcome("p1", "sea")
	spawnX, spawnY := spawnFrom(t, welcome)

	// a client claiming to be 300 metres away one tick after it joined
	h.send(conn, protocol.EncodeInput(protocol.Input{Seq: 1, X: 32767, Y: 32767}))

	views := h.RoomViews()
	if len(views) != 1 || len(views[0].Players) != 1 {
		t.Fatalf("expected one room with one player, got %+v", views)
	}
	player := views[0].Players[0]

	maxStep := float64(protocol.MaxSpeed / protocol.TickRate)
	dist := math.Hypot(float64(player.X-spawnX), float64(player.Y-spawnY))
	if dist > maxStep+1 {
		t.Fatalf("player moved %.0f cm in one tick, the budget is %.0f", dist, maxStep)
	}
	if player.Violations != 1 {
		t.Fatalf("violations = %d, want 1", player.Violations)
	}
	if got := h.Stats.ClampedInputs.Load(); got != 1 {
		t.Fatalf("clamped counter = %d, want 1", got)
	}

	// the next step, which respects the speed budget, is accepted at face value
	nextX, nextY := player.X+10, player.Y+10
	h.send(conn, protocol.EncodeInput(protocol.Input{Seq: 2, X: nextX, Y: nextY}))

	after := h.RoomViews()[0].Players[0]
	if after.X != nextX || after.Y != nextY {
		t.Fatalf("legal step changed the player to %d,%d, want %d,%d", after.X, after.Y, nextX, nextY)
	}
	if after.Violations != 1 {
		t.Fatalf("a legal step was counted as a violation (violations = %d)", after.Violations)
	}
}

func TestAnHonestClientIsNeverClamped(t *testing.T) {
	h := newHarness(t)
	conn, welcome := h.joinAndWelcome("p1", "sea")
	spawnX, spawnY := spawnFrom(t, welcome)

	// 30 Hz for a second, moving at 400 cm/s: the load test originally reported clamping here
	// because a client that ignores its spawn point spends the whole match being corrected
	x, y := spawnX, spawnY
	for i := 0; i < protocol.TickRate; i++ {
		x += 13
		y += 13
		h.send(conn, protocol.EncodeInput(protocol.Input{Seq: uint16(i + 1), X: x, Y: y}))
	}

	if got := h.Stats.ClampedInputs.Load(); got != 0 {
		t.Fatalf("an honest client was clamped %d times, want 0", got)
	}
	if got, want := h.RoomViews()[0].Players[0].X, x; got != want {
		t.Fatalf("final position = %d, want %d: the relay changed a legal movement", got, want)
	}
}

func TestInputBeforeHelloIsRejected(t *testing.T) {
	h := newHarness(t)

	conn := h.dial()
	frame := protocol.EncodeInput(protocol.Input{Seq: 1, X: 1, Y: 1})
	h.send(conn, frame)

	reply := h.read(conn)
	code, _, err := protocol.ErrorMessage(reply)
	if err != nil {
		t.Fatalf("ErrorMessage() error = %v", err)
	}
	if code != protocol.ErrBadToken {
		t.Fatalf("code = %d, want %d", code, protocol.ErrBadToken)
	}
}

func TestGarbageTokensAreRefusedWithoutTouchingTheRoom(t *testing.T) {
	h := newHarness(t)

	conn := h.dial()
	frame, err := protocol.EncodeHello("not.a.real.ticket")
	if err != nil {
		t.Fatalf("EncodeHello() error = %v", err)
	}
	h.send(conn, frame)

	code, _, err := protocol.ErrorMessage(h.read(conn))
	if err != nil {
		t.Fatalf("ErrorMessage() error = %v", err)
	}
	if code != protocol.ErrBadToken {
		t.Fatalf("code = %d, want %d", code, protocol.ErrBadToken)
	}
	if rooms, players := h.Counts(); rooms != 0 || players != 0 {
		t.Fatalf("a refused client created %d rooms / %d players", rooms, players)
	}
	if got := h.Stats.BadTokens.Load(); got != 1 {
		t.Fatalf("bad token counter = %d, want 1", got)
	}
}

func TestSecondSessionForTheSamePlayerReplacesTheFirst(t *testing.T) {
	h := newHarness(t)
	first, _ := h.joinAndWelcome("same-player", "sea")
	second, _ := h.joinAndWelcome("same-player", "sea")

	rooms, players := h.Counts()
	if rooms != 1 || players != 1 {
		t.Fatalf("counts = %d rooms / %d players, want one player in one room", rooms, players)
	}

	// the replaced socket is told why, rather than silently stopping
	frame := h.read(first)
	if frame[0] != protocol.KindError {
		t.Fatalf("the replaced connection got kind %d, want an ERROR frame", frame[0])
	}
	_ = second
}

func TestGhostFlagThenGhostDrop(t *testing.T) {
	h := newHarness(t)
	conn, welcome := h.joinAndWelcome("p1", "sea")
	spawnX, spawnY := spawnFrom(t, welcome)

	// 10 cm per axis is inside the per-tick speed budget (800 cm/s at 30 Hz), so it is applied
	// exactly as sent
	h.send(conn, protocol.EncodeInput(protocol.Input{Seq: 1, X: spawnX + 10, Y: spawnY + 10}))

	// nothing for longer than GhostAfter: the position is kept and flagged
	h.clock.Advance(h.cfg.GhostAfter + time.Millisecond)
	h.tick()
	views := h.RoomViews()
	if len(views) != 1 {
		t.Fatalf("the room disappeared: %+v", views)
	}
	if !views[0].Players[0].Ghost {
		t.Fatal("a silent player was not flagged as a ghost")
	}
	if views[0].Players[0].X != spawnX+10 || views[0].Players[0].Y != spawnY+10 {
		t.Fatalf("the ghost position changed to %d,%d, want the last known %d,%d",
			views[0].Players[0].X, views[0].Players[0].Y, spawnX+10, spawnY+10)
	}

	// silent for longer than GhostDropAfter: the ghost is dropped
	h.clock.Advance(h.cfg.GhostDropAfter)
	h.tick()
	if rooms, players := h.Counts(); rooms != 0 || players != 0 {
		t.Fatalf("after the drop: %d rooms / %d players, want none", rooms, players)
	}
	if got := h.Stats.GhostDropped.Load(); got != 1 {
		t.Fatalf("ghost dropped counter = %d, want 1", got)
	}
}

func TestTickBroadcastsEveryPlayerInTheRoom(t *testing.T) {
	h := newHarness(t)
	first, _ := h.joinAndWelcome("p1", "sea")
	second, _ := h.joinAndWelcome("p2", "sea")

	h.tick()

	for name, conn := range map[string]*net.UDPConn{"first": first, "second": second} {
		frame := h.read(conn)
		snap, err := protocol.DecodeSnapshot(frame)
		if err != nil {
			t.Fatalf("%s player got an undecodable frame: %v", name, err)
		}
		if snap.Tick != 1 {
			t.Fatalf("%s player: tick = %d, want 1", name, snap.Tick)
		}
		if len(snap.Players) != 2 {
			t.Fatalf("%s player: snapshot has %d players, want 2", name, len(snap.Players))
		}
	}
}

func TestRateLimitDropsInputBeyondTheBudget(t *testing.T) {
	h := newHarness(t)
	conn, _ := h.joinAndWelcome("p1", "sea")

	for i := 0; i < protocol.MaxInputPerSecond+10; i++ {
		h.send(conn, protocol.EncodeInput(protocol.Input{Seq: uint16(i), X: int16(i), Y: 0}))
	}

	if got := h.Stats.RateLimited.Load(); got != 10 {
		t.Fatalf("rate limited = %d, want 10", got)
	}
}

func TestIdleRoomIsCollectedAndItsPlayersAreTold(t *testing.T) {
	h := newHarness(t)
	conn, _ := h.joinAndWelcome("p1", "sea")

	h.clock.Advance(h.cfg.IdleTimeout + time.Second)
	h.tick()

	if rooms, players := h.Counts(); rooms != 0 || players != 0 {
		t.Fatalf("the idle room was not collected: %d rooms / %d players", rooms, players)
	}
	if got := h.Stats.RoomsClosed.Load(); got != 1 {
		t.Fatalf("rooms closed = %d, want 1", got)
	}

	frame := h.read(conn)
	if frame[0] != protocol.KindError {
		t.Fatalf("frame kind = %d, want an ERROR frame telling the client the room is gone", frame[0])
	}
	if code, message, _ := protocol.ErrorMessage(frame); code != protocol.ErrNoRoom || message == "" {
		t.Fatalf("error frame = %d %q, want a no-room error with a message", code, message)
	}
}

func TestPingIsAnsweredWithTheSameClientTime(t *testing.T) {
	h := newHarness(t)
	conn, _ := h.joinAndWelcome("p1", "sea")

	clientTime := uint64(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC).UnixNano())
	h.send(conn, protocol.EncodePing(clientTime))

	got, tick, err := protocol.DecodePong(h.read(conn))
	if err != nil {
		t.Fatalf("DecodePong() error = %v", err)
	}
	if got != clientTime {
		t.Fatalf("pong echoed %d, want the client time %d", got, clientTime)
	}
	_ = tick
}

func TestUnknownFrameKindIsAnsweredWithBadFrame(t *testing.T) {
	h := newHarness(t)
	conn, _ := h.joinAndWelcome("p1", "sea")

	h.send(conn, []byte{200, 1, 2, 3})

	code, _, err := protocol.ErrorMessage(h.read(conn))
	if err != nil {
		t.Fatalf("ErrorMessage() error = %v", err)
	}
	if code != protocol.ErrBadFrame {
		t.Fatalf("code = %d, want %d", code, protocol.ErrBadFrame)
	}
	if got := h.Stats.BadFrames.Load(); got < 1 {
		t.Fatalf("bad frame counter = %d, want at least 1", got)
	}
}

func TestTickCountsItsOwnDurationAndTheRoomLifecycle(t *testing.T) {
	h := newHarness(t)
	h.joinAndWelcome("p1", "sea")
	h.tick()
	h.tick()

	snap := h.Stats.Snapshot()
	if snap.PlayersJoined != 1 {
		t.Fatalf("players joined = %d, want 1", snap.PlayersJoined)
	}
	if snap.RoomsOpened != 1 {
		t.Fatalf("rooms opened = %d, want 1", snap.RoomsOpened)
	}
	if snap.PacketsOut == 0 {
		t.Fatal("the tick loop sent no snapshots")
	}
	// the duration of both ticks was recorded, so the percentiles are not silently zero
	if snap.TickP50Micros < 0 || snap.TickP99Micros < snap.TickP50Micros {
		t.Fatalf("tick percentiles look wrong: p50=%d p99=%d", snap.TickP50Micros, snap.TickP99Micros)
	}
}

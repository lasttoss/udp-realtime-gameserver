package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lasttoss/udp-realtime-gameserver/internal/protocol"
	"github.com/lasttoss/udp-realtime-gameserver/internal/relay"
)

// clock is a hand wound clock: the tests need a ticket that is definitely expired, and sleeping
// for the token TTL is not a test. The relay reads it from its own goroutines, so it is guarded.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// env is the gateway, the relay behind it and a client socket, which is everything the HTTP API
// needs to be exercised for real. The relay gets its own UDP socket rather than a mock so these
// tests cover the seam where a ticket minted over HTTP is presented over UDP.
type env struct {
	t       *testing.T
	gw      *httptest.Server
	relay   *relay.Relay
	clock   *clock
	secret  []byte
	client  *net.UDPConn
	relayAt string
}

func newEnv(t *testing.T) *env {
	t.Helper()

	c := &clock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	secret := []byte("gateway-test-secret")

	cfg := relay.DefaultConfig(secret)
	cfg.Now = c.Now
	r := relay.New(cfg)
	if err := r.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("relay.Listen() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = r.LocalAddr()
	})

	gwCfg := DefaultConfig(secret, r.LocalAddr().String())
	gwCfg.Now = c.Now
	srv := httptest.NewServer(New(gwCfg, r).Handler())
	t.Cleanup(srv.Close)

	return &env{t: t, gw: srv, relay: r, clock: c, secret: secret, relayAt: r.LocalAddr().String()}
}

// auth calls the endpoint under test and returns the decoded response.
func (e *env) auth(body string) (int, map[string]any) {
	e.t.Helper()

	resp, err := http.Post(e.gw.URL+"/v1/auth", "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("POST /v1/auth error = %v", err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// get fetches a path and returns the status and the raw body.
func (e *env) get(path string) (int, string) {
	e.t.Helper()

	resp, err := http.Get(e.gw.URL + path)
	if err != nil {
		e.t.Fatalf("GET %s error = %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// dial opens a client socket pointed at the relay.
func (e *env) dial() *net.UDPConn {
	e.t.Helper()

	conn, err := net.DialUDP("udp", nil, e.relay.LocalAddr().(*net.UDPAddr))
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// hello sends a HELLO carrying the ticket and returns the frame the relay answered with.
func (e *env) hello(conn *net.UDPConn, token string) []byte {
	e.t.Helper()

	frame, err := protocol.EncodeHello(token)
	if err != nil {
		e.t.Fatalf("EncodeHello() error = %v", err)
	}
	if _, err := conn.Write(frame); err != nil {
		e.t.Fatalf("write hello: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		e.t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		e.t.Fatalf("read the relay's answer: %v", err)
	}
	return buf[:n]
}

// TestATicketMintedOverHTTPIsAcceptedOverUDP walks the whole path an honest client takes: ask for
// a ticket, present it on the UDP socket, be placed in a room, and then be visible over HTTP.
func TestATicketMintedOverHTTPIsAcceptedOverUDP(t *testing.T) {
	e := newEnv(t)

	status, auth := e.auth(`{"player_id":"ada","region":"eu"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/auth status = %d, want 200 (%v)", status, auth)
	}
	if auth["tick_rate"].(float64) != float64(protocol.TickRate) {
		t.Errorf("tick_rate = %v, want %d", auth["tick_rate"], protocol.TickRate)
	}
	if auth["relay_addr"] != e.relayAt {
		t.Errorf("relay_addr = %v, want %s", auth["relay_addr"], e.relayAt)
	}
	if auth["region"] != "eu" {
		t.Errorf("region = %v, want eu", auth["region"])
	}

	token, _ := auth["token"].(string)
	if token == "" {
		t.Fatal("the auth endpoint returned an empty token")
	}
	// The gateway is not the only verifier: the relay must accept what this token claims.
	claims, err := protocol.VerifyToken(e.secret, token, e.clock.Now())
	if err != nil {
		t.Fatalf("the minted token does not verify: %v", err)
	}
	if claims.PlayerID != "ada" || claims.Region != "eu" {
		t.Errorf("claims = %+v, want player_id=ada region=eu", claims)
	}

	conn := e.dial()
	frame := e.hello(conn, token)
	if frame[0] != protocol.KindWelcome {
		t.Fatalf("first byte = %d, want KindWelcome (%d): %x", frame[0], protocol.KindWelcome, frame)
	}
	welcome, err := protocol.DecodeWelcome(frame)
	if err != nil {
		t.Fatalf("DecodeWelcome() error = %v", err)
	}

	// and now the same session read back over HTTP
	status, body := e.get("/v1/rooms")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/rooms status = %d", status)
	}
	var rooms struct {
		Rooms []relay.RoomView `json:"rooms"`
	}
	if err := json.Unmarshal([]byte(body), &rooms); err != nil {
		t.Fatalf("decode /v1/rooms: %v (%s)", err, body)
	}
	if len(rooms.Rooms) != 1 {
		t.Fatalf("rooms = %d, want 1", len(rooms.Rooms))
	}
	if rooms.Rooms[0].ID != welcome.RoomID {
		t.Errorf("room id = %d, want the one in WELCOME (%d)", rooms.Rooms[0].ID, welcome.RoomID)
	}
	if len(rooms.Rooms[0].Players) != 1 || rooms.Rooms[0].Players[0].PlayerKey != "ada" {
		t.Fatalf("room players = %+v, want ada alone", rooms.Rooms[0].Players)
	}
	if rooms.Rooms[0].Region != "eu" {
		t.Errorf("room region = %q, want eu", rooms.Rooms[0].Region)
	}
	// the player is standing on the spawn point the relay chose, not where the client wanted to be
	if got := rooms.Rooms[0].Players[0]; got.X != welcome.SpawnX || got.Y != welcome.SpawnY {
		t.Errorf("player at (%d,%d), want the spawn point (%d,%d)", got.X, got.Y, welcome.SpawnX, welcome.SpawnY)
	}

	status, body = e.get("/v1/rooms/" + strconv.Itoa(int(welcome.RoomID)) + "/state")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/rooms/{id}/state status = %d (%s)", status, body)
	}
	status, body = e.get("/v1/stats")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/stats status = %d", status)
	}
	var stats map[string]any
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatalf("decode /v1/stats: %v", err)
	}
	if stats["rooms"].(float64) != 1 || stats["players"].(float64) != 1 {
		t.Errorf("/v1/stats rooms/players = %v/%v, want 1/1", stats["rooms"], stats["players"])
	}
	counters, _ := stats["counters"].(map[string]any)
	if counters["players_joined"].(float64) != 1 {
		t.Errorf("players_joined = %v, want 1", counters["players_joined"])
	}
}

// TestAnExpiredTicketIsRefusedOnTheUDPPath is the reason the auth endpoint returns an expiry at
// all: the relay has to enforce it, and it has to say why instead of dropping the packet.
func TestAnExpiredTicketIsRefusedOnTheUDPPath(t *testing.T) {
	e := newEnv(t)

	_, auth := e.auth(`{"player_id":"ada"}`)
	token := auth["token"].(string)

	// wind the clock past the ticket's lifetime
	e.clock.Advance(11 * time.Minute)

	conn := e.dial()
	frame := e.hello(conn, token)
	if frame[0] != protocol.KindError {
		t.Fatalf("first byte = %d, want KindError (%d): %x", frame[0], protocol.KindError, frame)
	}
	if frame[1] != protocol.ErrBadToken {
		t.Errorf("error code = %d, want ErrBadToken (%d)", frame[1], protocol.ErrBadToken)
	}
	if _, body := e.get("/v1/rooms"); !strings.Contains(body, `"rooms":[]`) {
		t.Errorf("/v1/rooms = %s, want no room to have been created by a refused ticket", body)
	}
}

func TestAuthValidatesItsInput(t *testing.T) {
	e := newEnv(t)

	tests := []struct {
		name string
		body string
	}{
		{"not json at all", `{player_id: ada}`},
		{"an empty body", ``},
		{"no player_id", `{"region":"sea"}`},
		{"a blank player_id", `{"player_id":"   "}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := e.auth(tc.body); status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%v)", status, body)
			}
		})
	}
}

func TestAuthFillsInTheDefaults(t *testing.T) {
	e := newEnv(t)

	status, auth := e.auth(`{"player_id":"ada"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if auth["region"] != "sea" {
		t.Errorf("region = %v, want the default sea", auth["region"])
	}
	if auth["expires_in_s"].(float64) != (10 * time.Minute).Seconds() {
		t.Errorf("expires_in_s = %v, want 600", auth["expires_in_s"])
	}
	claims, err := protocol.VerifyToken(e.secret, auth["token"].(string), e.clock.Now())
	if err != nil {
		t.Fatalf("VerifyToken() error = %v", err)
	}
	if claims.GameID != "tag-arena" {
		t.Errorf("game_id = %q, want the default tag-arena", claims.GameID)
	}
}

func TestTheHTTPRoutesAnswerAndRefuseCorrectly(t *testing.T) {
	e := newEnv(t)

	tests := []struct {
		method string
		path   string
		want   int
		expect string
	}{
		{"GET", "/healthz", http.StatusOK, "ok"},
		{"GET", "/", http.StatusOK, "<html"},
		{"GET", "/metrics", http.StatusOK, "udp_relay_packets_in_total"},
		{"GET", "/metrics", http.StatusOK, `udp_relay_tick_duration_micros{quantile="0.95"}`},
		{"GET", "/v1/rooms/999/state", http.StatusNotFound, "no such room"},
		{"GET", "/v1/rooms/not-a-number/state", http.StatusBadRequest, "room id must be a number"},
		{"GET", "/v1/auth", http.StatusMethodNotAllowed, ""},
		{"GET", "/v1/stat", http.StatusNotFound, "404 page not found"},
		{"GET", "/index.html", http.StatusNotFound, "404 page not found"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, e.gw.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.expect != "" && !strings.Contains(string(body), tc.expect) {
				t.Errorf("body = %q, want it to contain %q", body, tc.expect)
			}
		})
	}
}

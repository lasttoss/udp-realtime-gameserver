// Package gateway is the HTTP side of the realtime tier: it mints the short lived tickets the UDP
// relay verifies, exposes the relay's state for dashboards and serves the canvas viewer.
//
// In a real deployment the auth endpoint would be the game's existing backend; here it exists so
// the relay has something to trust that is not the client itself.
package gateway

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lasttoss/udp-realtime-gameserver/internal/protocol"
	"github.com/lasttoss/udp-realtime-gameserver/internal/relay"
)

//go:embed web/index.html
var webFS embed.FS

// Config is the gateway's tuning.
type Config struct {
	Secret    []byte
	TokenTTL  time.Duration
	RelayAddr string
	Logger    *slog.Logger
	Now       func() time.Time
}

func DefaultConfig(secret []byte, relayAddr string) Config {
	return Config{
		Secret:    secret,
		TokenTTL:  10 * time.Minute,
		RelayAddr: relayAddr,
		Logger:    slog.Default(),
		Now:       time.Now,
	}
}

// Gateway serves the HTTP API.
type Gateway struct {
	cfg   Config
	relay *relay.Relay
}

func New(cfg Config, r *relay.Relay) *Gateway {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Gateway{cfg: cfg, relay: r}
}

// Handler wires the routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth", g.handleAuth)
	mux.HandleFunc("GET /v1/stats", g.handleStats)
	mux.HandleFunc("GET /v1/rooms", g.handleRooms)
	mux.HandleFunc("GET /v1/rooms/{id}/state", g.handleRoomState)
	mux.HandleFunc("GET /healthz", g.handleHealth)
	mux.HandleFunc("GET /metrics", g.handleMetrics)
	// "{$}" is an exact match on "/": without it this catch-all answers every unknown path with
	// the viewer and a 200, which hides typos in the API paths from the client.
	mux.HandleFunc("GET /{$}", g.handleViewer)
	return mux
}

type authRequest struct {
	PlayerID string `json:"player_id"`
	Region   string `json:"region"`
	GameID   string `json:"game_id"`
}

type authResponse struct {
	Token      string `json:"token"`
	PlayerID   string `json:"player_id"`
	Region     string `json:"region"`
	RelayAddr  string `json:"relay_addr"`
	TickRate   int    `json:"tick_rate"`
	ExpiresAt  string `json:"expires_at"`
	ExpiresInS int64  `json:"expires_in_s"`
}

// handleAuth mints a ticket. It deliberately does not create accounts: identity belongs to the
// game backend, the relay only needs to know who is allowed in and for how long.
func (g *Gateway) handleAuth(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}

	req.PlayerID = strings.TrimSpace(req.PlayerID)
	req.Region = strings.TrimSpace(req.Region)
	req.GameID = strings.TrimSpace(req.GameID)
	if req.PlayerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "player_id is required"})
		return
	}
	if req.Region == "" {
		req.Region = "sea"
	}
	if req.GameID == "" {
		req.GameID = "tag-arena"
	}

	now := g.cfg.Now()
	expires := now.Add(g.cfg.TokenTTL)
	token, err := protocol.SignToken(g.cfg.Secret, protocol.Claims{
		PlayerID: req.PlayerID,
		Region:   req.Region,
		GameID:   req.GameID,
		Expires:  expires.Unix(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, authResponse{
		Token:      token,
		PlayerID:   req.PlayerID,
		Region:     req.Region,
		RelayAddr:  g.cfg.RelayAddr,
		TickRate:   protocol.TickRate,
		ExpiresAt:  expires.UTC().Format(time.RFC3339),
		ExpiresInS: int64(g.cfg.TokenTTL.Seconds()),
	})
}

func (g *Gateway) handleStats(w http.ResponseWriter, r *http.Request) {
	rooms, players := g.relay.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"rooms":      rooms,
		"players":    players,
		"tick_rate":  protocol.TickRate,
		"relay_addr": g.cfg.RelayAddr,
		"counters":   g.relay.Stats.Snapshot(),
	})
}

func (g *Gateway) handleRooms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"rooms": g.relay.RoomViews()})
}

func (g *Gateway) handleRoomState(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 16)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "room id must be a number"})
		return
	}
	for _, room := range g.relay.RoomViews() {
		if room.ID == uint16(id) {
			writeJSON(w, http.StatusOK, room)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such room"})
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

func (g *Gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	g.relay.Stats.WritePrometheus(w)
}

func (g *Gateway) handleViewer(w http.ResponseWriter, r *http.Request) {
	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "viewer not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

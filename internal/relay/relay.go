// Package relay is the UDP realtime tier: it holds one authoritative room per group of players,
// applies a 30 Hz simulation tick, and broadcasts snapshots of the world to everyone in the room.
//
// What makes it a game server rather than a message bus:
//
//   - a client cannot move itself. It sends an INPUT and the relay decides the new position,
//     clamping anything faster than MaxSpeed so a modified client cannot teleport.
//   - a player that stops sending input does not vanish: it is flagged as a ghost (the last known
//     position, so peers can keep interpolating) and only dropped after GhostDropAfter.
//   - a room with nothing happening is collected, which is what keeps a fleet of pods from
//     carrying thousands of empty rooms after the players went home.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/lasttoss/udp-realtime-gameserver/internal/protocol"
)

// socketReadBuffer is the UDP receive buffer the relay asks the kernel for.
const socketReadBuffer = 8 << 20 // 8 MiB

// Config is the relay's tuning, all of it with a usable default.
type Config struct {
	// Secret verifies the tickets minted by the auth endpoint.
	Secret []byte
	// MaxRooms caps how many rooms one process will run.
	MaxRooms int
	// GhostAfter is how long without input before a player is flagged as a ghost.
	GhostAfter time.Duration
	// GhostDropAfter is how long without input before a ghost is dropped from the room.
	GhostDropAfter time.Duration
	// IdleTimeout closes a room that has had no input for this long.
	IdleTimeout time.Duration
	// Now is injectable so the rules above can be tested without sleeping.
	Now func() time.Time
	// Logger receives the relay's own events.
	Logger *slog.Logger
}

// DefaultConfig returns the tuned defaults used in the demo.
func DefaultConfig(secret []byte) Config {
	return Config{
		Secret:         secret,
		MaxRooms:       1000,
		GhostAfter:     200 * time.Millisecond,
		GhostDropAfter: 5 * time.Second,
		IdleTimeout:    30 * time.Second,
		Now:            time.Now,
		Logger:         slog.Default(),
	}
}

// Player is one connected client inside a room.
type Player struct {
	ID        uint16
	PlayerKey string // from the ticket: one session per player key
	Region    string
	Addr      *net.UDPAddr

	X, Y       int16
	LastSeq    uint16
	LastInput  time.Time
	LastSeen   time.Time
	Violations int
	Inputs     int

	windowStart time.Time
	windowCount int
}

// Room is one authoritative simulation, capped at MaxPlayersPerRoom players.
type Room struct {
	ID           uint16
	Region       string
	Tick         uint32
	Players      map[uint16]*Player
	byAddr       map[string]uint16
	LastActivity time.Time
}

// PlayerView is the read-only shape used by the HTTP API and the viewer.
type PlayerView struct {
	ID         uint16 `json:"id"`
	PlayerKey  string `json:"player_key"`
	X          int16  `json:"x"`
	Y          int16  `json:"y"`
	Ghost      bool   `json:"ghost"`
	IdleMs     int64  `json:"idle_ms"`
	Violations int    `json:"violations"`
}

// RoomView is the read-only shape of a room.
type RoomView struct {
	ID           uint16       `json:"id"`
	Region       string       `json:"region"`
	Tick         uint32       `json:"tick"`
	Players      []PlayerView `json:"players"`
	LastActivity time.Time    `json:"last_activity"`
}

// Relay owns the socket, the rooms and the tick loop.
type Relay struct {
	cfg   Config
	conn  *net.UDPConn
	Stats *Metrics

	mu           sync.RWMutex
	rooms        map[uint16]*Room
	byPlayer     map[string]*Player
	nextRoomID   uint16
	nextPlayerID uint16

	wg   sync.WaitGroup
	done chan struct{}
}

// New builds a relay. Call Listen before Serve.
func New(cfg Config) *Relay {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Relay{
		cfg:      cfg,
		Stats:    NewMetrics(),
		rooms:    make(map[uint16]*Room),
		byPlayer: make(map[string]*Player),
		done:     make(chan struct{}),
	}
}

// Listen binds the UDP socket.
func (r *Relay) Listen(addr string) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	// The default socket receive buffer is a few hundred kilobytes, which is roughly a tenth of a
	// second of input traffic from a thousand players: one scheduling hiccup and the kernel drops
	// datagrams, which the clients then experience as being corrected by the server. Asking for a
	// bigger buffer is the cheapest half of the fix (the other half is what the client does when it
	// notices: see AckSeq in the snapshot and reconcile).
	if err := conn.SetReadBuffer(socketReadBuffer); err != nil {
		r.cfg.Logger.Warn("could not enlarge the udp receive buffer", "err", err)
	}
	r.conn = conn
	r.cfg.Logger.Info("udp relay listening", "addr", conn.LocalAddr().String(), "read_buffer_bytes", socketReadBuffer)
	return nil
}

// LocalAddr reports the bound address (useful when the port was chosen by the kernel).
func (r *Relay) LocalAddr() net.Addr {
	if r.conn == nil {
		return nil
	}
	return r.conn.LocalAddr()
}

// Serve reads packets and drives the tick loop until the context is cancelled.
func (r *Relay) Serve(ctx context.Context) error {
	if r.conn == nil {
		return errors.New("relay: Listen must be called before Serve")
	}

	readDone := make(chan struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(readDone)
		r.readLoop(ctx)
	}()

	ticker := time.NewTicker(protocol.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = r.conn.Close()
			<-readDone
			r.wg.Wait()
			return nil
		case <-ticker.C:
			started := r.cfg.Now()
			r.tick()
			r.Stats.ObserveTick(r.cfg.Now().Sub(started))
		}
	}
}

func (r *Relay) readLoop(ctx context.Context) {
	buf := make([]byte, 2048)
	for {
		n, addr, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				// A single malformed datagram must never take the relay down.
				r.cfg.Logger.Debug("udp read failed", "err", err)
				continue
			}
		}
		r.Stats.PacketsIn.Add(1)
		r.Stats.BytesIn.Add(int64(n))
		r.handlePacket(addr, buf[:n])
	}
}

func (r *Relay) handlePacket(addr *net.UDPAddr, packet []byte) {
	kind, err := protocol.Kind(packet)
	if err != nil {
		r.Stats.BadFrames.Add(1)
		return
	}

	switch kind {
	case protocol.KindHello:
		r.handleHello(addr, packet)
	case protocol.KindInput:
		r.handleInput(addr, packet)
	case protocol.KindPing:
		if len(packet) < 9 {
			r.Stats.BadFrames.Add(1)
			return
		}
		clientTime := uint64(packet[1])<<56 | uint64(packet[2])<<48 | uint64(packet[3])<<40 | uint64(packet[4])<<32 |
			uint64(packet[5])<<24 | uint64(packet[6])<<16 | uint64(packet[7])<<8 | uint64(packet[8])
		r.send(addr, protocol.EncodePong(clientTime, r.currentTick()))
	default:
		r.Stats.BadFrames.Add(1)
		_ = r.send(addr, protocol.EncodeError(protocol.ErrBadFrame, "unknown frame kind"))
	}
}

func (r *Relay) handleHello(addr *net.UDPAddr, packet []byte) {
	hello, err := protocol.DecodeHello(packet)
	if err != nil {
		r.Stats.BadFrames.Add(1)
		return
	}

	claims, err := protocol.VerifyToken(r.cfg.Secret, hello.Token, r.cfg.Now())
	if err != nil {
		r.Stats.BadTokens.Add(1)
		_ = r.send(addr, protocol.EncodeError(protocol.ErrBadToken, err.Error()))
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// One session per player: a second HELLO for the same player key replaces the first socket
	// instead of letting the account be in two rooms at once. The replaced client is told why,
	// so a game can react ("you signed in on another device") instead of just going silent.
	if existing, ok := r.byPlayer[claims.PlayerID]; ok {
		_ = r.send(existing.Addr, protocol.EncodeError(protocol.ErrBadToken, "session replaced by a newer connection"))
		r.removePlayerLocked(existing, "replaced by a newer session")
	}

	room := r.assignRoomLocked(claims.Region)
	if room == nil {
		_ = r.send(addr, protocol.EncodeError(protocol.ErrRoomFull, "all rooms in this region are full"))
		return
	}

	slot := len(room.Players)
	spawnX, spawnY := protocol.SpawnPoint(slot)

	player := &Player{
		ID:          r.nextPlayerID,
		PlayerKey:   claims.PlayerID,
		Region:      claims.Region,
		Addr:        addr,
		X:           spawnX,
		Y:           spawnY,
		LastInput:   r.cfg.Now(),
		LastSeen:    r.cfg.Now(),
		windowStart: r.cfg.Now(),
	}
	r.nextPlayerID++

	room.Players[player.ID] = player
	room.byAddr[addr.String()] = player.ID
	room.LastActivity = r.cfg.Now()
	r.byPlayer[claims.PlayerID] = player
	r.Stats.PlayersJoined.Add(1)

	r.send(addr, protocol.EncodeWelcome(player.ID, room.ID, player.X, player.Y, uint64(r.cfg.Now().UnixMilli())))
	r.cfg.Logger.Debug("player joined", "player", claims.PlayerID, "room", room.ID, "region", room.Region)
}

func (r *Relay) handleInput(addr *net.UDPAddr, packet []byte) {
	input, err := protocol.DecodeInput(packet)
	if err != nil {
		r.Stats.BadFrames.Add(1)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	player := r.playerByAddrLocked(addr)
	if player == nil {
		r.Stats.BadFrames.Add(1)
		_ = r.send(addr, protocol.EncodeError(protocol.ErrBadToken, "send HELLO before INPUT"))
		return
	}

	now := r.cfg.Now()
	player.LastSeen = now

	// Rate limit: a client that sends faster than the simulation can consume only wastes
	// bandwidth, so extra inputs inside the window are dropped rather than queued.
	if now.Sub(player.windowStart) > time.Second {
		player.windowStart = now
		player.windowCount = 0
	}
	player.windowCount++
	if player.windowCount > protocol.MaxInputPerSecond {
		r.Stats.RateLimited.Add(1)
		return
	}

	// Server authority: the client proposes, the relay decides.
	x, y, clamped := protocol.Clamp(player.X, player.Y, input.X, input.Y)
	if clamped {
		player.Violations++
		r.Stats.ClampedInputs.Add(1)
	}

	player.X, player.Y = x, y
	player.LastSeq = input.Seq
	player.LastInput = now
	player.Inputs++

	if room := r.roomOfPlayerLocked(player); room != nil {
		room.LastActivity = now
	}
}

func (r *Relay) send(addr *net.UDPAddr, payload []byte) error {
	if addr == nil {
		return nil
	}
	n, err := r.conn.WriteToUDP(payload, addr)
	if err != nil {
		return err
	}
	r.Stats.PacketsOut.Add(1)
	r.Stats.BytesOut.Add(int64(n))
	return nil
}

// tick advances every room once and broadcasts the resulting snapshot.
func (r *Relay) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.cfg.Now()
	for id, room := range r.rooms {
		if len(room.Players) == 0 {
			delete(r.rooms, id)
			r.Stats.RoomsClosed.Add(1)
			continue
		}
		if now.Sub(room.LastActivity) > r.cfg.IdleTimeout {
			r.closeRoomLocked(room, "room closed: idle")
			continue
		}

		room.Tick++
		snapshot := make([]protocol.PlayerState, 0, len(room.Players))
		for _, p := range room.Players {
			flags := uint8(0)
			idle := now.Sub(p.LastInput)
			if idle > r.cfg.GhostAfter {
				flags |= protocol.FlagGhost
			}
			if idle > r.cfg.GhostDropAfter {
				// A client that has been silent for this long is gone: peers should not keep
				// interpolating towards a player that is not coming back.
				r.removePlayerLocked(p, "ghost timeout")
				r.Stats.GhostDropped.Add(1)
				continue
			}
			if now.Sub(p.LastSeen) > r.cfg.GhostAfter {
				flags |= protocol.FlagIdle
			}
			snapshot = append(snapshot, protocol.PlayerState{ID: p.ID, Flags: flags, X: p.X, Y: p.Y})
		}

		if len(room.Players) == 0 {
			delete(r.rooms, id)
			r.Stats.RoomsClosed.Add(1)
			continue
		}

		for _, p := range room.Players {
			_ = r.send(p.Addr, protocol.EncodeSnapshot(room.Tick, p.LastSeq, snapshot))
		}
	}
}

func (r *Relay) closeRoomLocked(room *Room, reason string) {
	for _, p := range room.Players {
		_ = r.send(p.Addr, protocol.EncodeError(protocol.ErrNoRoom, reason+" ("+room.Region+")"))
		delete(r.byPlayer, p.PlayerKey)
		r.Stats.PlayersLeft.Add(1)
	}
	delete(r.rooms, room.ID)
	r.Stats.RoomsClosed.Add(1)
	r.cfg.Logger.Debug("room closed", "room", room.ID, "reason", reason)
}

func (r *Relay) removePlayerLocked(p *Player, reason string) {
	room := r.roomOfPlayerLocked(p)
	if room != nil {
		delete(room.Players, p.ID)
		delete(room.byAddr, p.Addr.String())
		if len(room.Players) == 0 {
			delete(r.rooms, room.ID)
			r.Stats.RoomsClosed.Add(1)
		}
	}
	delete(r.byPlayer, p.PlayerKey)
	r.Stats.PlayersLeft.Add(1)
	r.cfg.Logger.Debug("player left", "player", p.PlayerKey, "reason", reason)
}

// assignRoomLocked does region bucketed matchmaking: players only ever share a room with players
// in the same region, which is what keeps a match on one continent.
func (r *Relay) assignRoomLocked(region string) *Room {
	for _, room := range r.rooms {
		if room.Region == region && len(room.Players) < protocol.MaxPlayersPerRoom {
			return room
		}
	}
	if len(r.rooms) >= r.cfg.MaxRooms {
		return nil
	}

	room := &Room{
		ID:           r.nextRoomID,
		Region:       region,
		Players:      make(map[uint16]*Player),
		byAddr:       make(map[string]uint16),
		LastActivity: r.cfg.Now(),
	}
	r.nextRoomID++
	r.rooms[room.ID] = room
	r.Stats.RoomsOpened.Add(1)
	return room
}

func (r *Relay) playerByAddrLocked(addr *net.UDPAddr) *Player {
	key := addr.String()
	for _, room := range r.rooms {
		if id, ok := room.byAddr[key]; ok {
			return room.Players[id]
		}
	}
	return nil
}

func (r *Relay) roomOfPlayerLocked(p *Player) *Room {
	for _, room := range r.rooms {
		if _, ok := room.Players[p.ID]; ok {
			return room
		}
	}
	return nil
}

func (r *Relay) currentTick() uint32 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var max uint32
	for _, room := range r.rooms {
		if room.Tick > max {
			max = room.Tick
		}
	}
	return max
}

// RoomViews returns a copy of the room state for the HTTP API and the viewer.
func (r *Relay) RoomViews() []RoomView {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := r.cfg.Now()
	views := make([]RoomView, 0, len(r.rooms))
	for _, room := range r.rooms {
		view := RoomView{
			ID:           room.ID,
			Region:       room.Region,
			Tick:         room.Tick,
			LastActivity: room.LastActivity,
			Players:      make([]PlayerView, 0, len(room.Players)),
		}
		for _, p := range room.Players {
			view.Players = append(view.Players, PlayerView{
				ID:         p.ID,
				PlayerKey:  p.PlayerKey,
				X:          p.X,
				Y:          p.Y,
				Ghost:      now.Sub(p.LastInput) > r.cfg.GhostAfter,
				IdleMs:     now.Sub(p.LastInput).Milliseconds(),
				Violations: p.Violations,
			})
		}
		views = append(views, view)
	}
	return views
}

// Counts reports how many rooms and players are live right now.
func (r *Relay) Counts() (rooms, players int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, room := range r.rooms {
		rooms++
		players += len(room.Players)
	}
	return rooms, players
}

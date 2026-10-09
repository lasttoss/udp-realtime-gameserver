// Package protocol defines the wire format shared by the relay and its clients.
//
// Every frame is binary and starts with a one byte kind, so a game client can parse it without
// a schema and a relay can dispatch it without reflection:
//
//	client -> relay
//	  1  HELLO          [1][tokenLen u16][token]                       join a room
//	  2  INPUT          [2][seq u16][x i16][y i16]                     7 bytes, 30 Hz
//	  3  PING           [3][clientTime u64]                           RTT + tick jitter probe
//
//	relay -> client
//	128  WELCOME       [128][playerId u16][roomId u16][tickRate u8][spawnX i16][spawnY i16][serverTime u64]
//	129  SNAPSHOT      [129][tick u32][ackSeq u16][count u8] then count * [id u16][flags u8][x i16][y i16]
//	130  PONG          [130][clientTime u64][serverTick u32]
//	131  ERROR         [131][code u8][msgLen u8][msg]
//
// Positions are int16 in centimetres: a 300x300 m arena fits in the range with 1 cm precision,
// which is what keeps a 10 player snapshot at 74 bytes instead of a few hundred.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Frame kinds.
const (
	KindHello    = 1
	KindInput    = 2
	KindPing     = 3
	KindWelcome  = 128
	KindSnapshot = 129
	KindPong     = 130
	KindError    = 131
)

// Error codes sent back in an ERROR frame.
const (
	ErrBadFrame uint8 = iota + 1
	ErrBadToken
	ErrNoRoom
	ErrRoomFull
	ErrRateLimited
)

// Player flags inside a snapshot.
const (
	FlagGhost uint8 = 1 << 0 // no input recently: the position is the last known one
	FlagIdle  uint8 = 1 << 1 // connected but has not moved for a while
)

// Limits the relay enforces on every packet.
const (
	MaxPlayersPerRoom = 10
	MaxTokenBytes     = 512
	MaxInputPerSecond = 60 // 30 Hz expected; anything above this is dropped
)

// TickRate is the rate the relay sends snapshots at.
const TickRate = 30

// TickInterval is the time between snapshots.
const TickInterval = time.Second / TickRate

// MaxSpeed, in centimetres per second. Input that would move a player faster than this is
// clamped: the relay owns the player's position, not the client.
const MaxSpeed = 800

// Quantize converts a world position in centimetres into the wire representation.
func Quantize(centimetres float64) int16 {
	switch {
	case centimetres > 32767:
		return 32767
	case centimetres < -32768:
		return -32768
	default:
		return int16(centimetres)
	}
}

// Dequantize converts a wire position back into centimetres.
func Dequantize(v int16) float64 { return float64(v) }

// Clamp applies server authority: an INPUT may never move a player further than the maximum
// speed allows for one tick, whatever the client claims.
func Clamp(fromX, fromY, toX, toY int16) (int16, int16, bool) {
	maxStep := MaxSpeed / TickRate // centimetres per tick
	dx := float64(toX) - float64(fromX)
	dy := float64(toY) - float64(fromY)
	dist := dx*dx + dy*dy
	if dist <= float64(maxStep*maxStep) {
		return toX, toY, false
	}
	scale := float64(maxStep) / sqrt(dist)
	return Quantize(float64(fromX) + dx*scale), Quantize(float64(fromY) + dy*scale), true
}

func sqrt(v float64) float64 {
	if v <= 0 {
		return 0
	}
	x := v
	for i := 0; i < 24; i++ {
		x = (x + v/x) / 2
	}
	return x
}

// Hello is a decoded HELLO frame.
type Hello struct {
	Token string
}

// Input is a decoded INPUT frame.
type Input struct {
	Seq uint16
	X   int16
	Y   int16
}

// PlayerState is one entry of a snapshot.
type PlayerState struct {
	ID    uint16
	Flags uint8
	X     int16
	Y     int16
}

// Snapshot is a decoded SNAPSHOT frame.
type Snapshot struct {
	Tick    uint32
	AckSeq  uint16
	Players []PlayerState
}

// EncodeHello builds a HELLO frame.
func EncodeHello(token string) ([]byte, error) {
	if len(token) == 0 || len(token) > MaxTokenBytes {
		return nil, fmt.Errorf("token length must be between 1 and %d bytes", MaxTokenBytes)
	}
	buf := make([]byte, 3+len(token))
	buf[0] = KindHello
	binary.BigEndian.PutUint16(buf[1:3], uint16(len(token)))
	copy(buf[3:], token)
	return buf, nil
}

// EncodeInput builds an INPUT frame.
func EncodeInput(in Input) []byte {
	buf := make([]byte, 7)
	buf[0] = KindInput
	binary.BigEndian.PutUint16(buf[1:3], in.Seq)
	binary.BigEndian.PutUint16(buf[3:5], uint16(in.X))
	binary.BigEndian.PutUint16(buf[5:7], uint16(in.Y))
	return buf
}

// EncodePing builds a PING frame.
func EncodePing(clientTime uint64) []byte {
	buf := make([]byte, 9)
	buf[0] = KindPing
	binary.BigEndian.PutUint64(buf[1:9], clientTime)
	return buf
}

// WelcomeSize is the size of a WELCOME frame.
const WelcomeSize = 18

// EncodeWelcome builds a WELCOME frame.
//
// It carries the spawn position because the relay owns the simulation: a client that starts
// where it feels like would spend the rest of the match being clamped back by the server (which
// is exactly what the load test showed before this field existed).
func EncodeWelcome(playerID, roomID uint16, spawnX, spawnY int16, serverTime uint64) []byte {
	buf := make([]byte, WelcomeSize)
	buf[0] = KindWelcome
	binary.BigEndian.PutUint16(buf[1:3], playerID)
	binary.BigEndian.PutUint16(buf[3:5], roomID)
	buf[5] = TickRate
	binary.BigEndian.PutUint16(buf[6:8], uint16(spawnX))
	binary.BigEndian.PutUint16(buf[8:10], uint16(spawnY))
	binary.BigEndian.PutUint64(buf[10:18], serverTime)
	return buf
}

// Welcome is a decoded WELCOME frame.
type Welcome struct {
	PlayerID   uint16
	RoomID     uint16
	TickRate   uint8
	SpawnX     int16
	SpawnY     int16
	ServerTime uint64
}

// DecodeWelcome parses a WELCOME frame.
func DecodeWelcome(buf []byte) (Welcome, error) {
	if len(buf) < WelcomeSize {
		return Welcome{}, ErrShortFrame
	}
	return Welcome{
		PlayerID:   binary.BigEndian.Uint16(buf[1:3]),
		RoomID:     binary.BigEndian.Uint16(buf[3:5]),
		TickRate:   buf[5],
		SpawnX:     int16(binary.BigEndian.Uint16(buf[6:8])),
		SpawnY:     int16(binary.BigEndian.Uint16(buf[8:10])),
		ServerTime: binary.BigEndian.Uint64(buf[10:18]),
	}, nil
}

// SpawnPoint spreads the players of a room around the arena so that they do not all start on top
// of each other, and so every client gets the same answer for the same slot.
func SpawnPoint(slot int) (int16, int16) {
	const radius = 500.0
	angle := float64(slot) * (6.283185307179586 / float64(MaxPlayersPerRoom))
	return Quantize(radius * cos(angle)), Quantize(radius * sin(angle))
}

func cos(x float64) float64 {
	// Small, dependency free cosine: the relay only needs it once per join. The angle is folded
	// into [0, pi] before the series, because cos(2*pi - x) = cos(x) - folding without the flip is
	// the whole trick, and a sign error here silently puts half the room on the same spot.
	x = mod2pi(x)
	if x > 3.141592653589793 {
		x = 6.283185307179586 - x
	}
	term, sum := 1.0, 1.0
	for n := 1; n <= 8; n++ {
		term *= -x * x / float64((2*n-1)*(2*n))
		sum += term
	}
	return sum
}

func sin(x float64) float64 {
	return cos(x - 1.5707963267948966)
}

func mod2pi(x float64) float64 {
	const twoPi = 6.283185307179586
	for x < 0 {
		x += twoPi
	}
	for x >= twoPi {
		x -= twoPi
	}
	return x
}

// EncodeSnapshot builds a SNAPSHOT frame.
func EncodeSnapshot(tick uint32, ackSeq uint16, players []PlayerState) []byte {
	buf := make([]byte, 8+len(players)*7)
	buf[0] = KindSnapshot
	binary.BigEndian.PutUint32(buf[1:5], tick)
	binary.BigEndian.PutUint16(buf[5:7], ackSeq)
	buf[7] = uint8(len(players))
	offset := 8
	for _, p := range players {
		binary.BigEndian.PutUint16(buf[offset:offset+2], p.ID)
		buf[offset+2] = p.Flags
		binary.BigEndian.PutUint16(buf[offset+3:offset+5], uint16(p.X))
		binary.BigEndian.PutUint16(buf[offset+5:offset+7], uint16(p.Y))
		offset += 7
	}
	return buf
}

// EncodePong builds a PONG frame.
func EncodePong(clientTime uint64, serverTick uint32) []byte {
	buf := make([]byte, 13)
	buf[0] = KindPong
	binary.BigEndian.PutUint64(buf[1:9], clientTime)
	binary.BigEndian.PutUint32(buf[9:13], serverTick)
	return buf
}

// EncodeError builds an ERROR frame.
func EncodeError(code uint8, message string) []byte {
	if len(message) > 255 {
		message = message[:255]
	}
	buf := make([]byte, 3+len(message))
	buf[0] = KindError
	buf[1] = code
	buf[2] = uint8(len(message))
	copy(buf[3:], message)
	return buf
}

// ErrShortFrame is returned when a frame ends before the fields it claims to have.
var ErrShortFrame = errors.New("protocol: short frame")

// DecodeHello parses a HELLO frame.
func DecodeHello(buf []byte) (Hello, error) {
	if len(buf) < 3 {
		return Hello{}, ErrShortFrame
	}
	n := int(binary.BigEndian.Uint16(buf[1:3]))
	if n == 0 || len(buf) < 3+n {
		return Hello{}, ErrShortFrame
	}
	return Hello{Token: string(buf[3 : 3+n])}, nil
}

// DecodeInput parses an INPUT frame.
func DecodeInput(buf []byte) (Input, error) {
	if len(buf) < 7 {
		return Input{}, ErrShortFrame
	}
	return Input{
		Seq: binary.BigEndian.Uint16(buf[1:3]),
		X:   int16(binary.BigEndian.Uint16(buf[3:5])),
		Y:   int16(binary.BigEndian.Uint16(buf[5:7])),
	}, nil
}

// DecodeSnapshot parses a SNAPSHOT frame.
func DecodeSnapshot(buf []byte) (Snapshot, error) {
	if len(buf) < 8 {
		return Snapshot{}, ErrShortFrame
	}
	count := int(buf[7])
	if len(buf) < 8+count*7 {
		return Snapshot{}, ErrShortFrame
	}
	snap := Snapshot{
		Tick:    binary.BigEndian.Uint32(buf[1:5]),
		AckSeq:  binary.BigEndian.Uint16(buf[5:7]),
		Players: make([]PlayerState, 0, count),
	}
	offset := 8
	for i := 0; i < count; i++ {
		snap.Players = append(snap.Players, PlayerState{
			ID:    binary.BigEndian.Uint16(buf[offset : offset+2]),
			Flags: buf[offset+2],
			X:     int16(binary.BigEndian.Uint16(buf[offset+3 : offset+5])),
			Y:     int16(binary.BigEndian.Uint16(buf[offset+5 : offset+7])),
		})
		offset += 7
	}
	return snap, nil
}

// DecodePong parses a PONG frame.
func DecodePong(buf []byte) (clientTime uint64, serverTick uint32, err error) {
	if len(buf) < 13 {
		return 0, 0, ErrShortFrame
	}
	return binary.BigEndian.Uint64(buf[1:9]), binary.BigEndian.Uint32(buf[9:13]), nil
}

// ErrorMessage parses an ERROR frame.
func ErrorMessage(buf []byte) (uint8, string, error) {
	if len(buf) < 3 {
		return 0, "", ErrShortFrame
	}
	n := int(buf[2])
	if len(buf) < 3+n {
		return 0, "", ErrShortFrame
	}
	return buf[1], string(buf[3 : 3+n]), nil
}

// Kind returns the frame kind of an encoded packet.
func Kind(buf []byte) (uint8, error) {
	if len(buf) == 0 {
		return 0, ErrShortFrame
	}
	return buf[0], nil
}

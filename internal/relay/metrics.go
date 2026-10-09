package relay

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics are the numbers you need to answer "is the realtime tier healthy?": packet rates, how
// many inputs were dropped or clamped, and the distribution of the tick loop itself.
type Metrics struct {
	PacketsIn     atomic.Int64
	PacketsOut    atomic.Int64
	BytesIn       atomic.Int64
	BytesOut      atomic.Int64
	BadFrames     atomic.Int64
	BadTokens     atomic.Int64
	RateLimited   atomic.Int64
	ClampedInputs atomic.Int64
	PlayersJoined atomic.Int64
	PlayersLeft   atomic.Int64
	RoomsOpened   atomic.Int64
	RoomsClosed   atomic.Int64
	RoomsOpenedGC atomic.Int64
	GhostDropped  atomic.Int64

	mu        sync.Mutex
	tickDurs  []time.Duration
	tickIndex int32
}

func NewMetrics() *Metrics {
	return &Metrics{tickDurs: make([]time.Duration, 1024)}
}

// ObserveTick records how long one tick of the relay took.
func (m *Metrics) ObserveTick(d time.Duration) {
	m.mu.Lock()
	m.tickDurs[m.tickIndex%int32(len(m.tickDurs))] = d
	m.tickIndex++
	m.mu.Unlock()
}

// Snapshot is a point in time view of the counters.
type Snapshot struct {
	PacketsIn     int64 `json:"packets_in"`
	PacketsOut    int64 `json:"packets_out"`
	BytesIn       int64 `json:"bytes_in"`
	BytesOut      int64 `json:"bytes_out"`
	BadFrames     int64 `json:"bad_frames"`
	BadTokens     int64 `json:"bad_tokens"`
	RateLimited   int64 `json:"rate_limited_inputs"`
	ClampedInputs int64 `json:"clamped_inputs"`
	PlayersJoined int64 `json:"players_joined"`
	PlayersLeft   int64 `json:"players_left"`
	GhostDropped  int64 `json:"ghost_dropped"`
	RoomsOpened   int64 `json:"rooms_opened"`
	RoomsClosed   int64 `json:"rooms_closed"`
	TickP50Micros int64 `json:"tick_p50_micros"`
	TickP95Micros int64 `json:"tick_p95_micros"`
	TickP99Micros int64 `json:"tick_p99_micros"`
}

func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	seen := m.tickDurs[:min(int(m.tickIndex), len(m.tickDurs))]
	durs := make([]time.Duration, len(seen))
	copy(durs, seen)
	m.mu.Unlock()
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })

	return Snapshot{
		PacketsIn:     m.PacketsIn.Load(),
		PacketsOut:    m.PacketsOut.Load(),
		BytesIn:       m.BytesIn.Load(),
		BytesOut:      m.BytesOut.Load(),
		BadFrames:     m.BadFrames.Load(),
		BadTokens:     m.BadTokens.Load(),
		RateLimited:   m.RateLimited.Load(),
		ClampedInputs: m.ClampedInputs.Load(),
		PlayersJoined: m.PlayersJoined.Load(),
		PlayersLeft:   m.PlayersLeft.Load(),
		GhostDropped:  m.GhostDropped.Load(),
		RoomsOpened:   m.RoomsOpened.Load(),
		RoomsClosed:   m.RoomsClosed.Load(),
		TickP50Micros: micros(percentile(durs, 0.50)),
		TickP95Micros: micros(percentile(durs, 0.95)),
		TickP99Micros: micros(percentile(durs, 0.99)),
	}
}

// WritePrometheus renders the counters in the text exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	s := m.Snapshot()
	write := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	write("# HELP udp_relay_packets_in_total UDP datagrams received.")
	write("# TYPE udp_relay_packets_in_total counter")
	write("udp_relay_packets_in_total %d", s.PacketsIn)
	write("udp_relay_packets_out_total %d", s.PacketsOut)
	write("udp_relay_bytes_in_total %d", s.BytesIn)
	write("udp_relay_bytes_out_total %d", s.BytesOut)
	write("udp_relay_bad_frames_total %d", s.BadFrames)
	write("udp_relay_bad_tokens_total %d", s.BadTokens)
	write("udp_relay_rate_limited_inputs_total %d", s.RateLimited)
	write("udp_relay_clamped_inputs_total %d", s.ClampedInputs)
	write("udp_relay_players_joined_total %d", s.PlayersJoined)
	write("udp_relay_players_left_total %d", s.PlayersLeft)
	write("udp_relay_ghost_dropped_total %d", s.GhostDropped)
	write("udp_relay_rooms_opened_total %d", s.RoomsOpened)
	write("udp_relay_rooms_closed_total %d", s.RoomsClosed)

	write("# HELP udp_relay_tick_duration_micros Tick loop duration.")
	write("# TYPE udp_relay_tick_duration_micros summary")
	write("udp_relay_tick_duration_micros{quantile=\"0.5\"} %d", s.TickP50Micros)
	write("udp_relay_tick_duration_micros{quantile=\"0.95\"} %d", s.TickP95Micros)
	write("udp_relay_tick_duration_micros{quantile=\"0.99\"} %d", s.TickP99Micros)
}

func micros(d time.Duration) int64 { return d.Microseconds() }

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * q)
	return sorted[idx]
}

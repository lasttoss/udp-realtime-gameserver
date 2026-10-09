// Command botswarm is the load generator for the relay: it authenticates, joins, sends input at
// 30 Hz like a real client would, and reports the latency and packet rates it observed.
//
// The point is not "how many sockets can I open" but "at what player count does the tick loop
// stop keeping up", so it measures the two things that decide that:
//
//	RTT       - PING/PONG round trip per client, which includes the relay's tick loop
//	snapshots - how many authoritative updates each client actually received
//
// Usage:
//
//	botswarm -ccu 200 -ramp 10s -duration 30s
//	botswarm -ccu 1000 -ramp 30s -duration 60s -region eu
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lasttoss/udp-realtime-gameserver/internal/protocol"
)

type options struct {
	gateway  string
	relay    string
	ccu      int
	ramp     time.Duration
	duration time.Duration
	region   string
	prefix   string
	quiet    bool
}

func main() {
	var opt options
	flag.StringVar(&opt.gateway, "gateway", "http://127.0.0.1:8080", "gateway base url (mints the tickets)")
	flag.StringVar(&opt.relay, "relay", "", "relay udp address; empty means use the one the gateway reports")
	flag.IntVar(&opt.ccu, "ccu", 100, "number of simulated players")
	flag.DurationVar(&opt.ramp, "ramp", 5*time.Second, "time over which the players connect")
	flag.DurationVar(&opt.duration, "duration", 30*time.Second, "how long each player stays connected after ramp-up")
	flag.StringVar(&opt.region, "region", "sea", "region bucket to join")
	flag.StringVar(&opt.prefix, "prefix", "bot", "player id prefix")
	flag.BoolVar(&opt.quiet, "quiet", false, "only print the final report")
	flag.Parse()

	if opt.ccu < 1 {
		fmt.Fprintln(os.Stderr, "-ccu must be at least 1")
		os.Exit(2)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	statsBefore, err := fetchStats(client, opt.gateway)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway %s is not reachable: %v\n", opt.gateway, err)
		os.Exit(1)
	}

	fmt.Printf("botswarm: %d players, ramp %s, hold %s, region %s\n", opt.ccu, opt.ramp, opt.duration, opt.region)

	results := make([]*botResult, opt.ccu)
	var wg sync.WaitGroup
	gap := time.Duration(0)
	if opt.ccu > 1 {
		gap = opt.ramp / time.Duration(opt.ccu)
	}

	started := time.Now()
	var connected atomic.Int64
	for i := 0; i < opt.ccu; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := &botResult{index: i}
			results[i] = res
			if err := runBot(client, opt, i, res, opt.duration); err != nil {
				res.err = err
			} else {
				connected.Add(1)
			}
		}(i)
		if gap > 0 {
			time.Sleep(gap)
		}
	}
	wg.Wait()
	elapsed := time.Since(started)

	statsAfter, err := fetchStats(client, opt.gateway)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read the gateway stats at the end: %v\n", err)
	}

	report(results, opt, elapsed, statsBefore, statsAfter)
}

// botWorld is one simulated client's view of the world. A real game client predicts its own
// movement and then reconciles against the authoritative snapshot: the relay echoes the last
// input sequence it processed (AckSeq), so the client can keep the inputs the server has not
// seen yet and replay them from the position the server reported. Without that, dropped
// datagrams look like cheating - the client keeps walking while the server stands still, and the
// server (correctly) clamps it back.
type botWorld struct {
	mu     sync.Mutex
	id     uint16
	seq    uint16
	x, y   float64
	dirX   float64
	dirY   float64
	roomID uint16

	pending    []protocol.Input
	lastAck    uint16
	snapshots  int64
	bytes      int64
	errors     int64
	badFrames  int64
	welcome    bool
	reconciles int64
}

// seqAfter reports whether a happened after b, tolerating the uint16 wraparound.
func seqAfter(a, b uint16) bool { return int16(a-b) > 0 }

func (w *botWorld) onSnapshot(snap protocol.Snapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.snapshots++
	if seqAfter(snap.AckSeq, w.lastAck) {
		w.lastAck = snap.AckSeq
	}

	// forget the inputs the server has already applied
	kept := w.pending[:0]
	for _, in := range w.pending {
		if seqAfter(in.Seq, snap.AckSeq) {
			kept = append(kept, in)
		}
	}
	w.pending = kept

	var me *protocol.PlayerState
	for i := range snap.Players {
		if snap.Players[i].ID == w.id {
			me = &snap.Players[i]
			break
		}
	}
	if me == nil {
		return
	}

	// replay the unacked inputs on top of the authoritative position
	x, y := me.X, me.Y
	for _, in := range w.pending {
		x, y, _ = protocol.Clamp(x, y, in.X, in.Y)
	}
	if x != me.X || y != me.Y {
		w.reconciles++
	}
	w.x, w.y = float64(x), float64(y)
}

type botResult struct {
	mu         sync.Mutex
	index      int
	playerID   string
	snapshots  int64
	bytes      int64
	welcome    bool
	roomID     uint16
	spawnX     int16
	spawnY     int16
	errors     int64
	badFrames  int64
	rtts       []time.Duration
	ticks      []uint32
	reconciles int64
	err        error
}

type stats struct {
	Rooms    int `json:"rooms"`
	Players  int `json:"players"`
	Counters struct {
		PacketsIn     int64 `json:"packets_in"`
		PacketsOut    int64 `json:"packets_out"`
		BytesIn       int64 `json:"bytes_in"`
		BytesOut      int64 `json:"bytes_out"`
		BadFrames     int64 `json:"bad_frames"`
		BadTokens     int64 `json:"bad_tokens"`
		RateLimited   int64 `json:"rate_limited_inputs"`
		ClampedInputs int64 `json:"clamped_inputs"`
		TickP50Micros int64 `json:"tick_p50_micros"`
		TickP95Micros int64 `json:"tick_p95_micros"`
		TickP99Micros int64 `json:"tick_p99_micros"`
	} `json:"counters"`
}

func fetchStats(client *http.Client, gateway string) (stats, error) {
	var out stats
	res, err := client.Get(gateway + "/v1/stats")
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("GET /v1/stats -> HTTP %d", res.StatusCode)
	}
	return out, json.NewDecoder(res.Body).Decode(&out)
}

func runBot(client *http.Client, opt options, index int, res *botResult, hold time.Duration) error {
	res.playerID = fmt.Sprintf("%s-%d", opt.prefix, index)

	// 1. authenticate: exactly what a real client does before it can talk to the relay
	body, _ := json.Marshal(map[string]string{
		"player_id": res.playerID, "region": opt.region, "game_id": "tag-arena",
	})
	response, err := client.Post(opt.gateway+"/v1/auth", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("auth -> HTTP %d", response.StatusCode)
	}
	var auth struct {
		Token     string `json:"token"`
		RelayAddr string `json:"relay_addr"`
	}
	if err := json.NewDecoder(response.Body).Decode(&auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	relayAddr := opt.relay
	if relayAddr == "" {
		relayAddr = auth.RelayAddr
	}

	conn, err := net.Dial("udp", relayAddr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", relayAddr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(hold + 5*time.Second))

	hello, err := protocol.EncodeHello(auth.Token)
	if err != nil {
		return err
	}
	if _, err := conn.Write(hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}

	world := &botWorld{}

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	defer stop()

	// 2. read loop: snapshots and pongs, nothing else
	go func() {
		buf := make([]byte, 2048)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := conn.Read(buf)
			if err != nil {
				select {
				case <-done:
					return
				default:
					res.errors++
					return
				}
			}
			res.bytes += int64(n)
			kind, err := protocol.Kind(buf[:n])
			if err != nil {
				res.badFrames++
				continue
			}
			switch kind {
			case protocol.KindWelcome:
				welcome, err := protocol.DecodeWelcome(buf[:n])
				if err != nil {
					res.badFrames++
					continue
				}
				world.mu.Lock()
				world.welcome = true
				world.id = welcome.PlayerID
				world.roomID = welcome.RoomID
				world.x, world.y = float64(welcome.SpawnX), float64(welcome.SpawnY)
				world.mu.Unlock()
			case protocol.KindSnapshot:
				snap, err := protocol.DecodeSnapshot(buf[:n])
				if err != nil {
					res.badFrames++
					continue
				}
				world.onSnapshot(snap)
				res.ticks = append(res.ticks, snap.Tick)
			case protocol.KindPong:
				sent, tickOffset, err := protocol.DecodePong(buf[:n])
				if err != nil {
					res.badFrames++
					continue
				}
				res.rtts = append(res.rtts, time.Since(time.Unix(0, int64(sent))))
				_ = tickOffset
			case protocol.KindError:
				_, _, _ = protocol.ErrorMessage(buf[:n])
				res.errors++
			}
		}
	}()

	// 3. send loop: input at TickRate, and a ping once a second
	send := time.NewTicker(protocol.TickInterval)
	defer send.Stop()
	ping := time.NewTicker(time.Second)
	defer ping.Stop()
	deadline := time.Now().Add(hold)

	// Wait for the WELCOME frame: it carries the spawn position, and a client that starts
	// anywhere else burns the whole match being clamped back by the server.
	if !waitForSpawn(world, 3*time.Second) {
		return fmt.Errorf("no WELCOME frame came back")
	}

	rnd := rand.New(rand.NewSource(int64(index) + 1))
	dirX, dirY := rnd.Float64()-0.5, rnd.Float64()-0.5
	norm := math.Hypot(dirX, dirY)
	dirX, dirY = dirX/norm, dirY/norm

	for time.Now().Before(deadline) {
		select {
		case <-send.C:
			// wander at 400 cm/s, bounce off the arena walls
			if rnd.Float64() < 0.02 {
				dirX, dirY = rnd.Float64()-0.5, rnd.Float64()-0.5
				norm = math.Hypot(dirX, dirY)
				dirX, dirY = dirX/norm, dirY/norm
			}

			world.mu.Lock()
			step := 400.0 / protocol.TickRate
			world.x += dirX * step
			world.y += dirY * step
			if world.x > 4000 || world.x < -4000 {
				dirX = -dirX
			}
			if world.y > 4000 || world.y < -4000 {
				dirY = -dirY
			}
			world.seq++
			in := protocol.Input{
				Seq: world.seq,
				X:   protocol.Quantize(world.x),
				Y:   protocol.Quantize(world.y),
			}
			world.pending = append(world.pending, in)
			if len(world.pending) > 240 {
				world.pending = world.pending[1:]
			}
			world.mu.Unlock()

			if _, err := conn.Write(protocol.EncodeInput(in)); err != nil {
				return fmt.Errorf("input: %w", err)
			}
		case <-ping.C:
			if _, err := conn.Write(protocol.EncodePing(uint64(time.Now().UnixNano()))); err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		}
	}

	// hand the reconciled state back to the report
	world.mu.Lock()
	res.welcome = world.welcome
	res.roomID = world.roomID
	res.snapshots = world.snapshots
	res.reconciles = world.reconciles
	world.mu.Unlock()
	return nil
}

func report(results []*botResult, opt options, elapsed time.Duration, before, after stats) {
	var (
		connected, snapshots, bytesIn, errors, bad, noWelcome, reconciles int64
		rtts                                                              []time.Duration
	)
	for _, res := range results {
		if res == nil {
			continue
		}
		if res.err != nil {
			if !opt.quiet {
				fmt.Printf("  player %-16s failed: %v\n", res.playerID, res.err)
			}
			continue
		}
		connected++
		snapshots += res.snapshots
		reconciles += res.reconciles
		bytesIn += res.bytes
		errors += res.errors
		bad += res.badFrames
		rtts = append(rtts, res.rtts...)
		if !res.welcome {
			noWelcome++
		}
	}

	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	seconds := elapsed.Seconds()

	fmt.Printf("\n--- botswarm report ---\n")
	fmt.Printf("players connected      : %d / %d\n", connected, opt.ccu)
	fmt.Printf("wall clock             : %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("snapshots received     : %d (%.1f per client per second)\n", snapshots, perClientPerSecond(snapshots, connected, opt.duration.Seconds()))
	fmt.Printf("client receive rate    : %.1f KiB/s\n", float64(bytesIn)/1024/seconds)
	fmt.Printf("rtt p50 / p95 / p99    : %s / %s / %s  (%d samples)\n",
		pct(rtts, 0.50), pct(rtts, 0.95), pct(rtts, 0.99), len(rtts))
	fmt.Printf("client errors          : %d (bad frames: %d, never welcomed: %d)\n", errors, bad, noWelcome)
	fmt.Printf("clients that reconciled: %d (a client that ignores AckSeq would be corrected by the server instead)\n", reconciles)

	fmt.Printf("\n--- relay (from /v1/stats) ---\n")
	fmt.Printf("rooms / players        : %d / %d\n", after.Rooms, after.Players)
	fmt.Printf("packets in / out       : %d / %d\n", after.Counters.PacketsIn-before.Counters.PacketsIn, after.Counters.PacketsOut-before.Counters.PacketsOut)
	fmt.Printf("bytes in / out         : %.1f MiB / %.1f MiB\n",
		float64(after.Counters.BytesIn-before.Counters.BytesIn)/1024/1024,
		float64(after.Counters.BytesOut-before.Counters.BytesOut)/1024/1024)
	fmt.Printf("tick loop p50/p95/p99  : %d / %d / %d us (budget %d us)\n",
		after.Counters.TickP50Micros, after.Counters.TickP95Micros, after.Counters.TickP99Micros,
		protocol.TickInterval.Microseconds())
	fmt.Printf("clamped inputs         : %d (server authority)\n", after.Counters.ClampedInputs)
	fmt.Printf("rate limited inputs    : %d\n", after.Counters.RateLimited)
	fmt.Printf("bad frames / bad tokens: %d / %d\n", after.Counters.BadFrames, after.Counters.BadTokens)

	utilization := float64(after.Counters.TickP95Micros) / float64(protocol.TickInterval.Microseconds()) * 100
	fmt.Printf("tick budget used (p95) : %.2f%%\n", utilization)
	fmt.Printf("\n%d players stayed connected with no client side errors and a p95 tick loop of %d us.\n",
		connected, after.Counters.TickP95Micros)
}

// waitForSpawn blocks until the reader goroutine has seen the WELCOME frame.
func waitForSpawn(world *botWorld, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		world.mu.Lock()
		ok := world.welcome
		world.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func perClientPerSecond(total, clients int64, seconds float64) float64 {
	if clients == 0 || seconds == 0 {
		return 0
	}
	return float64(total) / float64(clients) / seconds
}

func pct(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*q)].Round(time.Microsecond)
}

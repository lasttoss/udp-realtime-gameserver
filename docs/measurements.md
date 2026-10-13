# Measurements

The README answers "how many players per box?" with a load run, and a load run is only worth
something with the command next to it.

## Environment

| | |
|---|---|
| Machine | 12-core x86-64 (AMD64), Ubuntu 24.04, Docker 29.1.3 |
| Go | 1.27.2 |
| Date | 2026-10-13 (UTC) |

Every host port in this file is the one the repository publishes: bound to loopback and offset from
the default, so a machine already running a Postgres, a Redis or three other stacks does not have to
stop them first.


## 1000 bots at a flat 30.0 snapshots/second each

```
$ make up                                  # the relay container on loopback: 18090/tcp, 19000/udp
$ make load CCU=1000 DURATION=60s
botswarm: 1000 players, ramp 5s, hold 1m0s, region sea
players connected      : 1000 / 1000
snapshots received     : 1799872 (30.0 per client per second)
rtt p50 / p95 / p99    : 249µs / 6.401ms / 9.798ms  (58894 samples)
client errors          : 0 (bad frames: 0, never welcomed: 0)

--- relay (from /v1/stats) ---
rooms / players        : 93 / 917
tick loop p50/p95/p99  : 5525 / 9316 / 11676 us (budget 33333 us)
tick budget used (p95) : 27.95%
clamped inputs         : 2165 (server authority)
bad frames / bad tokens: 2 / 2

1000 players stayed connected with no client side errors and a p95 tick loop of 9316 us.
```

**Result.** 1000 of 1000 players connected and the snapshot rate is **30.0 per client per second**,
the tick rate the server advertises. The tick loop numbers differ from the README's run
(p95 9.3 ms here, 18 ms there) because they are the same command on different machines: what has to
repeat is the rate and the zero client errors, and they do.

## 44 tests, coverage `internal/` 79.3%

```
$ go test ./... -count=1 -v | grep -c '^--- PASS: Test'
44

$ go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | tail -1
total:  (statements)  48.5%
```

Per package, weighted by statement count from the profile itself:

```
internal/gateway     90.2%  (55/61)
internal/protocol    93.5%  (145/155)
internal/relay       68.3%  (179/262)
internal/            79.3%  (379/478)
```

**Result.** Both the test count and both coverage figures are exact. `cmd/relay` and `cmd/botswarm`
are wiring and a load generator, which is why the module total is 48.5% and the `internal/` total
79.3%: the README says so rather than quoting the larger number alone.

## The smoke test

```
$ make up && make smoke
[PASS] authenticated over http - relay=127.0.0.1:19000 tick_rate=30
[PASS] a forged ticket is refused - kind=131 code=2
[PASS] joined a room over udp and was given a spawn point - tick_rate=30 spawn=500,0
[PASS] receiving authoritative snapshots - 45 snapshots, tick advanced 44
[PASS] the relay reports the session - rooms=1 players=1
OK - auth, ticket verification, room join, 30 Hz snapshots and ping all work over UDP
```

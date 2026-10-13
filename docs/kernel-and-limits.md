# The UDP tier is the second biggest CPU consumer, and the first to hit its limit

A 30 Hz relay is cheap per packet and expensive per thousand players, and it is the component most
likely to be given a default resource limit. Measured on a live game backend serving around a
thousand concurrent players at ~8 packets per second per player:

| | application server | **realtime UDP tier** | ingress |
|---|---|---|---|
| core / 1,000 players (quiet client) | 0.28–0.33 | **0.14** | 0.23 |
| core / 1,000 players (chatty client) | 0.92 | **0.66** | 0.74 |

At a **500m limit the UDP tier saturates at roughly 750 players** — the limit, not the machine,
becomes the ceiling. Two reflexes that are wrong here:

- *"UDP is cheap, give it a small limit."* It is cheap per packet and grows linearly with players;
  a limit sized for an idle server caps the busy one.
- *"Keep the relay in the application pod."* It scales independently once its registry is shared
  (the rooms a relay serves live in a shared store, not in its own memory), and it should have its
  own node budget so that a redistribution of players cannot take CPU from the match loop.

## Seeing it: cgroup v2

```bash
cat /sys/fs/cgroup/cpu.stat   # nr_periods, nr_throttled, throttled_usec
```
A container can be throttled while the node is comfortable. If `nr_throttled` is not zero, the
number you measured is the limit, not the server.

## Kernel and OS knobs that actually mattered

- `net.core.somaxconn` and `net.ipv4.tcp_max_syn_backlog` — raised together with the application's
  own backlog settings; raising a client limit without the listen backlog just relocates the failure.
- **conntrack capacity** — a busy relay with many short-lived peers consumes table entries; track
  `nf_conntrack_count` against the maximum rather than discovering it during an incident.
- **received-buffer sizing** for the UDP socket (`SO_RCVBUF`) and the drop counters
  (`/proc/net/udp`, `netstat -su`) — for a 30 Hz relay, a full receive buffer looks exactly like
  packet loss on the client.
- the shared store's own limits (`maxclients`, `timeout`) — the realtime tier, the session registry
  and the match relay all touch it, so its ceiling is a game-server ceiling.

## The honest limit of these numbers

They were measured with synthetic players at a known packet rate. A real client's rate changes
everything above it by a factor of three (see `go-loadtest-swarm/docs/capacity-model.md`), so quote
the profile with the coefficient, or the coefficient means nothing.

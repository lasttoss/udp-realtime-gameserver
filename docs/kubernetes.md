# Running the relay on Kubernetes

The chart is in [`deploy/helm/udp-relay`](../deploy/helm/udp-relay). This file is the reasoning
behind it: what is different about shipping a UDP game server, which of the usual Kubernetes
reflexes are wrong here, and what is still missing before this belongs on the public internet.

```bash
kind create cluster --name arena
helm install arena deploy/helm/udp-relay
kubectl port-forward svc/arena-udp-relay 8080:8080
helm test arena
```

## The one thing that shapes everything: a room lives in one process

Rooms, players and the tick loop are in memory, in one pod. There is no shared state to fail over
to and no peer to take over a match. A client is not talking to "the game", it is talking to *the
pod that holds its room*, and everything else follows from that:

- **The ticket names the pod.** The gateway mints `relay_addr` from `RELAY_ADVERTISE`, which the
  chart sets to `$(POD_IP):9000`. Two pods, two answers, each correct for the client that asked.
  Point that at a Service and every ticket would name a random pod - a client would be told to
  play in a room that is not on the pod it reaches.
- **`helm upgrade`, and the players in a match are still disconnected.** `maxUnavailable: 0` and
  `preStop: sleep 5` make the rollout polite (the load balancer stops sending *new* players before
  the old pod starts refusing them), but the two hundred players in live rooms on that pod are
  still dropped when it exits. Fixing that properly means a drain protocol: the pod stops
  accepting joins, keeps ticking the rooms it has, tells each client "reconnect in 10 seconds",
  and exits when the last room is empty. The relay has the socket and the room list to do it; the
  chart has `terminationGracePeriodSeconds: 20` waiting for it.
- **`hostPort` means one relay pod per node.** A node can only bind one socket per UDP port, so
  the scheduler will not put a second `hostPort: 9000` pod on the same machine. That is fine for
  the "one pod, one big room" shape and it is why `replicaCount` defaults to `1`. Scale out by
  giving each node a pod (the scheduler spreads them with the chart's topology constraint), or
  turn on `service.udp.enabled` for a `NodePort` Service instead.

## Why not just a Service with `sessionAffinity: ClientIP`?

It works, and the chart can render it. But a UDP Service is a per-packet DNAT rule: the client's
datagram is rewritten to a pod the first time and, with `ClientIP` affinity, kube-proxy remembers
that choice for a while. Every one of those rules is a chance for a player's inputs to land on a
pod that has never heard of them, and the failure looks like packet loss in a game - the hardest
kind of bug to attribute. `hostPort` removes the middleman: the node's kernel delivers the
datagram straight to the pod that owns the room, and there is nothing between them to get it
wrong.

The cost is the one above: no packing. If you need density more than you need the shortest path,
the Service is the trade.

## What the first cluster run found: a rollout that cannot finish

The chart was installed on a single node cluster and then upgraded. The upgrade hung:

```
Waiting for deployment "arena-udp-relay" rollout to finish: 1 old replicas are pending termination...
arena-udp-relay-69c7c7dcd4-9b5dg   0/1   Pending   0   5m1s
arena-udp-relay-7784f8cb76-6n9fq   1/1   Running   0   5m20s
Error: UPGRADE FAILED: context deadline exceeded
```

`maxUnavailable: 0` and `maxSurge: 1` say "start the new pod, and only then stop the old one". With
`hostPort` on a **single node** that is a deadlock: the new pod cannot schedule because the old pod
is holding UDP 9000, and the old pod will not be stopped because the new one is not Ready. Nothing
is broken, the deployment just cannot make progress - and a `helm upgrade --wait` fails on it.

The fix is a one line choice, and the chart now documents it: **`maxUnavailable: 1` on a single
node**, `0` on a cluster with room to put the new pod somewhere else. That is the real cost of
`hostPort`: it makes the scheduler an active participant in your rollout.

```bash
helm upgrade arena deploy/helm/udp-relay --set deploymentStrategy.maxUnavailable=1
```

## Probes, resources, security

| Setting | Why |
|---|---|
| `readinessProbe: /healthz`, `startupProbe` | The gateway answers from the same process as the relay, so Ready means the UDP socket is bound and the tick loop is running. A relay that cannot bind the socket exits, so there is no "up but deaf" state to detect. |
| No `resources.limits.cpu` | A CPU limit is a throttle, and a throttled tick loop turns a 33.3 ms budget into missed ticks. Requests are 250m; the limit is memory only. |
| `readOnlyRootFilesystem`, `runAsNonRoot`, `capabilities: drop ALL`, `seccompProfile: RuntimeDefault` | The relay reads no files, writes no files and binds one port above 1024. Nothing in the image needs a capability, and the process can run as the unprivileged `app` user (uid 10001). |
| `topologySpreadConstraints` | Two relay pods on one node share its network stack and its CPU, and the tick loop is competing for that CPU. |

Scale on the tick budget, not on CPU. `/metrics` already exports
`udp_relay_tick_duration_micros{quantile="0.95"}` and the process logs `rooms` and `players` every
ten seconds; a CPU target is a proxy that fires late and adds pods that have no players yet. The
HPA in the chart is there for completeness and labelled as the blunt instrument it is.

## Verified how

The chart was installed on a real cluster (`kind`), not just rendered:

- `helm lint` and `helm template` for the default and every optional feature.
- `kubectl rollout status`, `helm test arena`, a `kubectl exec` into a pod asserting its own IP is
  the `relay_addr` it hands out, and a UDP datagram from outside the cluster reaching the pod
  through the node's `hostPort` and coming back as an `ERROR` frame.

## Still missing for production

- A **rendezvous service**: which region maps to which relay pods, and who has room. Today the
  matchmaking is eight lines inside the relay and knows only about its own rooms; a fleet needs a
  registry in front, and that is the one component this repository does not have.
- A **drain protocol** as described above, so a rollout costs nobody their match.
- **`PodDisruptionBudget` is a floor, not a plan**: voluntary disruptions are covered, a node
  dying is not, and nothing here survives a node dying with rooms on it.
- **Encryption and a replay window** on the UDP path. The ticket proves the join, nothing protects
  the datagrams after that.

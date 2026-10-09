# udp-relay (Helm chart)

Installs the relay and its HTTP gateway. One container does both: the relay owns the simulation and
the gateway mints the signed tickets the relay accepts.

```bash
helm install arena deploy/helm/udp-relay
kubectl port-forward svc/arena-udp-relay 8080:8080
helm test arena
```

## Why the chart looks like this

| Choice | Why |
|---|---|
| `hostPort` on the UDP port (one pod per node) | Rooms live in a pod's memory, so the ticket has to name a pod. `hostPort` makes every node able to deliver to its local pod, which is what a small cluster needs; the `service.udp` block is the managed-cloud alternative, and it needs `sessionAffinity: ClientIP` because a UDP Service is a per-packet DNAT rule. |
| `RELAY_ADVERTISE=$(POD_IP)` | The address in a ticket is the pod that will hold the room. Behind a Service it would be a random pod whose rooms belong to somebody else. |
| `maxUnavailable: 0`, `maxSurge: 1`, `preStop: sleep 5` | A restart drops the rooms on that pod. Rolling one pod at a time and letting the load balancer stop sending new players first keeps the damage to the players who were already in a match there. |
| No CPU limit | A throttled tick loop is worse than a noisy neighbour: the tick budget is the service level, not the CPU percentage. |
| Readiness on `/healthz` | The gateway answers from the same process as the relay, so Ready means the UDP socket is bound and the tick loop is running. |
| `topologySpreadConstraints` | Two relay pods on one node share that node's network stack and its CPU, which is exactly the resource the tick loop needs. |

## Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `1` | Pods when autoscaling is off: `hostPort` allows one per node. |
| `image.repository` / `image.tag` | GHCR / chart `appVersion` | Image to run. |
| `udp.port` | `9000` | UDP game port. |
| `udp.hostPort.enabled` | `true` | Bind the UDP port on every node. |
| `http.port` | `8080` | HTTP port: auth, stats, rooms, health, metrics, viewer. |
| `http.hostPort.enabled` | `false` | Reach the API straight through a node (single node demo). |
| `extraEnv` | `[]` | Extra environment for the relay, e.g. a pinned `RELAY_ADVERTISE`. |
| `service.udp.enabled` | `false` | Add a `NodePort` UDP Service instead of `hostPort`. |
| `advertisePodIP` | `true` | Each pod advertises its own IP in the tickets it mints. |
| `secret.existingSecret` | `""` | Use your own Secret for the ticket signing key. |
| `config.*` | see `values.yaml` | `MAX_ROOMS`, `ROOM_IDLE_TIMEOUT`, `GHOST_DROP_AFTER`, `LOG_LEVEL`. |
| `resources.limits.cpu` | *unset* | Left open on purpose. |
| `autoscaling.enabled` | `false` | HPA on CPU, documented as a blunt instrument. |
| `podDisruptionBudget.enabled` | `true` | `minAvailable: 1`. |
| `prometheus.serviceMonitor.enabled` | `false` | Scrape `/metrics` with the Prometheus Operator. |
| `networkPolicy.enabled` | `false` | Allow UDP in, and TCP only from `networkPolicy.httpFrom`. |
| `topologySpreadConstraints.enabled` | `true` | Spread pods over nodes. |

Design notes, including what to do about a player whose pod is being replaced, are in
[`docs/kubernetes.md`](../../../docs/kubernetes.md).

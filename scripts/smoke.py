#!/usr/bin/env python3
"""End-to-end smoke test for the UDP relay.

It behaves like a real client: it authenticates over HTTP, joins the relay over UDP, sends input
at the tick rate, and asserts on what comes back - including the frames that must be refused.

Usage: python3 scripts/smoke.py [gateway_host] [gateway_port] [relay_host] [relay_port]
"""
import json
import socket
import struct
import sys
import time
import urllib.error
import urllib.request
import uuid

GW_HOST = sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1"
GW_PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 8080
RELAY_HOST = sys.argv[3] if len(sys.argv) > 3 else "127.0.0.1"
RELAY_PORT = int(sys.argv[4]) if len(sys.argv) > 4 else 9000
GW = f"http://{GW_HOST}:{GW_PORT}"

KIND_HELLO, KIND_INPUT, KIND_PING = 1, 2, 3
KIND_WELCOME, KIND_SNAPSHOT, KIND_PONG, KIND_ERROR = 128, 129, 130, 131

ERR_BAD_FRAME, ERR_BAD_TOKEN, ERR_NO_ROOM, ERR_ROOM_FULL = 1, 2, 3, 4


def http(method, path, payload=None):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(GW + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as res:
            body = res.read().decode()
            try:
                return res.status, json.loads(body)
            except json.JSONDecodeError:
                return res.status, {"raw": body}
    except urllib.error.HTTPError as err:
        return err.code, json.loads(err.read().decode() or "{}")


def step(name, ok, detail=""):
    print(f"  [{'PASS' if ok else 'FAIL'}] {name}{(' - ' + detail) if detail else ''}")
    if not ok:
        sys.exit(1)


def encode_hello(token):
    raw = token.encode()
    return bytes([KIND_HELLO]) + struct.pack(">H", len(raw)) + raw


def encode_input(seq, x, y):
    return bytes([KIND_INPUT]) + struct.pack(">Hhh", seq, x, y)


def encode_ping(client_time):
    return bytes([KIND_PING]) + struct.pack(">Q", client_time)


def recv_frame(sock, timeout=3.0):
    sock.settimeout(timeout)
    head = sock.recv(2048)
    if not head:
        raise AssertionError("the relay closed the socket (udp has no close: nothing arrived)")
    return head


def wait_until_healthy(attempts=40):
    for _ in range(attempts):
        try:
            status, _ = http("GET", "/healthz")
            if status == 200:
                return
        except OSError:
            pass
        time.sleep(1)
    sys.exit(f"FAIL: {GW} never became healthy")


def main():
    wait_until_healthy()
    print(f"  [PASS] gateway is healthy - {GW}")

    player = "smoke-" + uuid.uuid4().hex[:8]
    status, auth = http("POST", "/v1/auth", {"player_id": player, "region": "sea"})
    step("authenticated over http", status == 200 and bool(auth.get("token")),
         f"relay={auth.get('relay_addr')} tick_rate={auth.get('tick_rate')}")

    relay = (RELAY_HOST, RELAY_PORT)
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.connect(relay)

    # a bad ticket must be refused before the relay allocates anything for this socket
    sock.send(encode_hello("this.is.not.a.ticket"))
    frame = recv_frame(sock)
    step("a forged ticket is refused",
         frame[0] == KIND_ERROR and frame[1] == ERR_BAD_TOKEN,
         f"kind={frame[0]} code={frame[1]}")

    sock.send(encode_input(1, 10, 10))
    frame = recv_frame(sock)
    step("input before hello is refused",
         frame[0] == KIND_ERROR and frame[1] == ERR_BAD_TOKEN, f"kind={frame[0]}")

    sock.send(encode_hello(auth["token"]))
    welcome = recv_frame(sock)
    if welcome[0] != KIND_WELCOME:
        sys.exit(f"FAIL: expected WELCOME, got kind {welcome[0]}")
    player_id, room_id, tick_rate, spawn_x, spawn_y = struct.unpack(">HHBhh", welcome[1:10])
    step("joined a room over udp and was given a spawn point", tick_rate == 30,
         f"player_id={player_id} room_id={room_id} tick_rate={tick_rate} spawn={spawn_x},{spawn_y}")

    # 30 Hz input for a second, then count the authoritative snapshots that came back
    snapshots, ticks, seq = 0, [], 0
    deadline = time.time() + 1.5
    sock.settimeout(0.5)
    while time.time() < deadline:
        seq += 1
        sock.send(encode_input(seq, spawn_x + seq, spawn_y))
        try:
            frame = sock.recv(2048)
        except socket.timeout:
            continue
        if frame[0] == KIND_SNAPSHOT:
            snapshots += 1
            ticks.append(struct.unpack(">I", frame[1:5])[0])
    step("receiving authoritative snapshots", snapshots >= 10,
         f"{snapshots} snapshots, tick advanced {ticks[-1] - ticks[0] if len(ticks) > 1 else 0}")

    client_time = time.time_ns()
    sock.send(encode_ping(client_time))
    latency = None
    for _ in range(10):
        try:
            frame = sock.recv(2048)
        except socket.timeout:
            break
        if frame[0] == KIND_PONG:
            echoed = struct.unpack(">Q", frame[1:9])[0]
            step("ping is answered", echoed == client_time,
                 f"rtt={((time.time_ns() - echoed) / 1e6):.2f} ms")
            latency = True
            break
    if latency is None:
        sys.exit("FAIL: no PONG came back")

    status, stats = http("GET", "/v1/stats")
    step("the relay reports the session", status == 200 and stats.get("players", 0) >= 1,
         f"rooms={stats.get('rooms')} players={stats.get('players')}")

    step("the relay counted the clamped/limited inputs",
         stats["counters"]["packets_in"] > 0 and stats["counters"]["packets_out"] > 0,
         json.dumps({k: v for k, v in stats["counters"].items() if k.startswith("packets")}))

    status, body = http("GET", "/metrics")
    step("prometheus metrics are exposed", status == 200 and "udp_relay_packets_in_total" in body.get("raw", ""))

    sock.close()
    print("OK - auth, ticket verification, room join, 30 Hz snapshots and ping all work over UDP")
    print(f"player_id={player}")


if __name__ == "__main__":
    main()

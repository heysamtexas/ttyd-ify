#!/usr/bin/env python3
"""Drive a real terminal over /ws and prove input reaches the shell and its output comes back.

    wsprobe.py <host:port> <session-name>            # type the marker command, expect its output
    wsprobe.py <host:port> <session-name> replay     # type NOTHING, expect the marker from replay

Replay mode sends no input at all, so the only way the marker can arrive is from the server's
replay of output an *earlier* probe produced. smoke.sh runs it against the same session after
`systemctl restart wt.service`, which is what proves a saved replay buffer survived the restart
(#92) — a fresh shell could never print SMOKE_OK unprompted.

test/smoke.sh proves an installed box answers on its port, upgrades /ws, and creates sessions. None
of that exchanges a single byte of terminal I/O, so a server whose relay path is broken *when run
under systemd as the service user* would pass every one of those assertions. This closes that gap,
which is the difference between "the HTTP surface responds" and the claim smoke.sh actually makes.

Raw sockets, no dependencies: the systemd test image has python3 (the Makefile already needs it for
the spec pipeline) but no pip, and adding a wheel to a fresh-machine test to check a fresh machine
would defeat the point. The framing here is only as complete as this one job needs.

The protocol is api/ws-protocol.md, sections 5 and 6:

  - the handshake is the first client message, JSON, with NO opcode prefix
  - after it, client messages are one ASCII opcode byte then payload: '0' INPUT, '1' RESIZE
  - server messages are the same shape: '0' OUTPUT, '1' SET_WINDOW_TITLE, '2' SET_PREFERENCES
  - server frames are always binary; the client's must be masked, per RFC 6455
"""

import base64
import json
import os
import socket
import struct
import sys
import time

# `echo SMOKE""_OK` rather than `echo SMOKE_OK`, and this is the whole trick: the shell echoes the
# characters typed, so the input itself appears in the output stream. With the quotes, the echoed
# input reads SMOKE""_OK and only the *executed* command can produce SMOKE_OK. So finding the marker
# proves a shell ran the command, not merely that the bytes made a round trip.
COMMAND = 'echo SMOKE""_OK\n'
MARKER = b"SMOKE_OK"
DEADLINE_SECONDS = 20


def http_upgrade(sock, host_header, path):
    key = base64.b64encode(os.urandom(16)).decode()
    request = (
        f"GET {path} HTTP/1.1\r\n"
        f"Host: {host_header}\r\n"
        "Connection: Upgrade\r\n"
        "Upgrade: websocket\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        f"Sec-WebSocket-Key: {key}\r\n"
        # The subprotocol is not optional: wtd refuses an upgrade that offers subprotocols without
        # `tty` (#36), and the iOS client offers exactly this one.
        "Sec-WebSocket-Protocol: tty\r\n"
        "\r\n"
    )
    sock.sendall(request.encode())

    # Read only as far as the end of the headers. Anything past them is already WebSocket framing
    # and must stay in the buffer for the frame reader.
    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = sock.recv(1)
        if not chunk:
            raise SystemExit(f"server closed during the upgrade; got {buf!r}")
        buf += chunk
    head = buf.decode("latin-1")
    status = head.split("\r\n", 1)[0]
    if "101" not in status:
        raise SystemExit(f"no upgrade: {status}")
    if "sec-websocket-protocol: tty" not in head.lower():
        raise SystemExit(f"upgrade did not select the tty subprotocol:\n{head}")


def send_message(sock, payload):
    """One masked binary message. Both real clients send post-handshake messages as binary."""
    frame = bytearray([0x82])  # FIN + binary
    mask = os.urandom(4)
    length = len(payload)
    if length < 126:
        frame.append(0x80 | length)
    elif length < 65536:
        frame.append(0x80 | 126)
        frame += struct.pack("!H", length)
    else:
        frame.append(0x80 | 127)
        frame += struct.pack("!Q", length)
    frame += mask
    frame += bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload))
    sock.sendall(bytes(frame))


def recv_exact(sock, count):
    buf = b""
    while len(buf) < count:
        chunk = sock.recv(count - len(buf))
        if not chunk:
            raise ConnectionError("server closed mid-frame")
        buf += chunk
    return buf


def recv_message(sock):
    """One complete message, reassembled across fragments. Returns (opcode, payload).

    Fragments matter because the opcode byte is a property of the *message*: a payload split across
    continuation frames would otherwise look like a message whose first byte is terminal output.
    """
    payload = b""
    first_opcode = None
    while True:
        header = recv_exact(sock, 2)
        fin = header[0] & 0x80
        opcode = header[0] & 0x0F
        masked = header[1] & 0x80
        length = header[1] & 0x7F
        if length == 126:
            length = struct.unpack("!H", recv_exact(sock, 2))[0]
        elif length == 127:
            length = struct.unpack("!Q", recv_exact(sock, 8))[0]
        mask = recv_exact(sock, 4) if masked else None
        data = recv_exact(sock, length) if length else b""
        if mask:
            data = bytes(byte ^ mask[i % 4] for i, byte in enumerate(data))

        if opcode == 0x8:  # close
            code = struct.unpack("!H", data[:2])[0] if len(data) >= 2 else 0
            raise SystemExit(f"server closed the connection: code {code}, reason {data[2:]!r}")
        if opcode == 0x9:  # ping — answer it, a probe that stalls a keepalive is its own bug
            pong = bytearray([0x8A, 0x80]) + os.urandom(4)
            sock.sendall(bytes(pong))
            continue
        if opcode == 0xA:  # pong
            continue

        if first_opcode is None:
            first_opcode = opcode
        payload += data
        if fin:
            return first_opcode, payload


def main():
    if len(sys.argv) not in (3, 4) or (len(sys.argv) == 4 and sys.argv[3] != "replay"):
        raise SystemExit(f"usage: {sys.argv[0]} <host:port> <session-name> [replay]")
    endpoint, session = sys.argv[1], sys.argv[2]
    replay_only = len(sys.argv) == 4
    host, port = endpoint.rsplit(":", 1)

    try:
        sock = socket.create_connection((host, int(port)), timeout=5)
    except OSError as err:
        # A traceback here would be the least useful thing in a failing CI log: the interesting
        # fact is that nothing was listening, and smoke.sh prints this file's output verbatim.
        raise SystemExit(f"cannot reach {endpoint}: {err}")
    sock.settimeout(2)
    http_upgrade(sock, endpoint, f"/ws?arg={session}")

    # No opcode prefix on this one, and the field names are exact. wtd creates the PTY at these
    # dimensions before the start command runs.
    send_message(sock, json.dumps({"AuthToken": "", "columns": 80, "rows": 24}).encode())
    # In replay mode nothing is typed, which is the entire assertion: the marker can then only
    # come from replayed history. Note the marker itself is immune to the echo trick here too —
    # the echoed command in that history still reads SMOKE""_OK.
    if not replay_only:
        send_message(sock, b"0" + COMMAND.encode())

    seen_opcodes = set()
    output = b""
    deadline = time.monotonic() + DEADLINE_SECONDS
    while time.monotonic() < deadline:
        try:
            opcode, payload = recv_message(sock)
        except socket.timeout:
            continue
        except ConnectionError as err:
            raise SystemExit(f"{err}; output so far: {output!r}")
        if opcode not in (0x1, 0x2):  # text or binary
            continue
        if not payload:
            continue
        seen_opcodes.add(payload[:1])
        if payload[:1] == b"0":  # OUTPUT
            output += payload[1:]
        if MARKER in output:
            if replay_only:
                print(f"replay confirmed: {MARKER.decode()} arrived with no input sent")
            else:
                print(f"terminal I/O confirmed: server opcodes seen {sorted(o.decode() for o in seen_opcodes)}")
                print(f"the shell ran the command and returned {MARKER.decode()}")
            return 0

    mode = "replayed" if replay_only else "typed and returned"
    raise SystemExit(
        f"no {MARKER.decode()} {mode} in {DEADLINE_SECONDS}s. Opcodes seen: "
        f"{sorted(o.decode() for o in seen_opcodes)}. Output was:\n{output!r}"
    )


if __name__ == "__main__":
    sys.exit(main())

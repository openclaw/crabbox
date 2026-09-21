"""Private SSH stdio relay for managed WayVNC; never reconnect its control socket."""
import json
import codecs
import os
import secrets
import shutil
import signal
import socket
import struct
import sys
import tempfile
import threading
import time


def read_exact(stream, size):
    data = b""
    while len(data) < size:
        chunk = stream.read(size - len(data))
        if not chunk:
            raise EOFError("VNC connection ended")
        data += chunk
    return data


class Control:
    def __init__(self, path):
        self.socket = socket.socket(socket.AF_UNIX)
        self.socket.settimeout(5)
        self.buffer = ""
        self.decoder = codecs.getincrementaldecoder("utf-8")()
        self.sequence = 0
        self.disconnected = set()
        try:
            self.socket.connect(path)
            self.rpc("event-receive")
        except Exception:
            self.socket.close()
            raise

    def rpc(self, method, params=None, deadline=None):
        deadline = deadline or time.monotonic() + 5
        self.socket.settimeout(max(0.001, deadline - time.monotonic()))
        self.sequence += 1
        request = self.sequence
        self.socket.sendall(json.dumps({"method": method, "params": params or {}, "id": request}).encode() + b"\n")
        while True:
            message = self.receive(deadline)
            if message.get("method") == "client-disconnected":
                self.disconnected.add(message["params"]["id"])
            if message.get("id") == request:
                if message.get("code") != 0:
                    raise RuntimeError("WayVNC control command failed")
                return message.get("data")

    def receive(self, deadline):
        # The control wire is concatenated JSON, unlike wayvncctl's JSON lines.
        while True:
            if time.monotonic() >= deadline:
                raise TimeoutError("WayVNC control deadline exceeded")
            self.buffer = self.buffer.lstrip()
            try:
                message, end = json.JSONDecoder().raw_decode(self.buffer)
                self.buffer = self.buffer[end:]
                return message
            except json.JSONDecodeError:
                if len(self.buffer) > 65536:
                    raise RuntimeError("WayVNC control reply exceeded its limit")
                if deadline:
                    self.socket.settimeout(max(0.001, deadline - time.monotonic()))
                data = self.socket.recv(4096)
                if not data:
                    raise RuntimeError("WayVNC control connection ended")
                self.buffer += self.decoder.decode(data)

    def retire(self, client, successor, allowed, deadline):
        # Both IDs were bound to initialized, dedicated-source relay connections.
        # Unmanaged clients may own the layout: do not guess which one to retire.
        if client == successor or client not in allowed or successor not in allowed:
            raise RuntimeError("invalid retirement binding")
        clients = self.rpc("client-list", deadline=deadline)
        ids = {entry["id"] for entry in clients}
        if not ids <= set(allowed) or successor not in ids:
            raise RuntimeError("unrelated or missing WayVNC client")
        if client in ids:
            self.rpc("client-disconnect", {"id": client}, deadline)
        while True:
            clients = self.rpc("client-list", deadline=deadline)
            ids = {entry["id"] for entry in clients}
            if not ids <= set(allowed) or successor not in ids:
                raise RuntimeError("WayVNC client set changed during retirement")
            if client not in ids:
                return {"acknowledgement": "client-disconnected" if client in self.disconnected else "client-list"}
            if time.monotonic() >= deadline:
                raise TimeoutError("WayVNC retirement timed out")
            time.sleep(min(0.05, max(0, deadline - time.monotonic())))

    def close(self):
        self.socket.close()


def upstream_init(vnc):
    stream = vnc.makefile("rb", buffering=0)
    if read_exact(stream, 12) != b"RFB 003.008\n":
        raise RuntimeError("unsupported WayVNC RFB version")
    vnc.sendall(b"RFB 003.008\n")
    count = read_exact(stream, 1)[0]
    if not count or 1 not in read_exact(stream, count):
        raise RuntimeError("managed WayVNC must use SSH-only authentication")
    vnc.sendall(b"\x01")
    if read_exact(stream, 4) != b"\0\0\0\0":
        raise RuntimeError("WayVNC authentication failed")
    vnc.sendall(b"\x01")  # Always share the desktop; never evict other clients.
    header = read_exact(stream, 24)
    length = struct.unpack("!I", header[20:24])[0]
    if length > 65536:
        raise RuntimeError("WayVNC desktop name too long")
    return stream, header + read_exact(stream, length)


def bind_client(control, vnc, address):
    clients = control.rpc("client-list")
    matches = [c for c in clients if c.get("address") == address]
    # A completed RFB handshake and a still-established local TCP connection
    # prove our initialized client is among the list's unique address match.
    if len(matches) != 1 or vnc.getsockopt(socket.IPPROTO_TCP, socket.TCP_INFO, 1) != b"\x01":
        raise RuntimeError("ambiguous WayVNC client identity")
    client = matches[0]["id"]
    if client in control.disconnected:
        raise RuntimeError("WayVNC client disappeared during binding")
    return client


def relay():
    runtime = "/tmp/crabbox-runtime-" + str(os.getuid())
    control = Control(runtime + "/wayvncctl")
    pid, uid, _ = struct.unpack("3i", control.socket.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    if uid != os.getuid():
        raise RuntimeError("WayVNC control peer has another owner")
    with open("/proc/sys/kernel/random/boot_id") as source:
        boot = source.read().strip()
    with open("/proc/%d/stat" % pid) as source:
        started = source.read().rsplit(")", 1)[1].split()[19]
    server = "%s:%d:%s" % (boot, pid, started)
    vnc = socket.socket()
    vnc.settimeout(5)
    # A dedicated source address plus our completed RFB initialization binds the
    # exact client. Shared SSH loopback addresses and list differences cannot.
    address = "127." + ".".join(str(secrets.randbelow(254) + 1) for _ in range(3))
    if any(c.get("address") == address for c in control.rpc("client-list")):
        raise RuntimeError("WayVNC source address collision")
    vnc.bind((address, 0))
    vnc.connect(("127.0.0.1", 5900))
    upstream, server_init = upstream_init(vnc)
    client = bind_client(control, vnc, address)
    directory = tempfile.mkdtemp(prefix="retire-", dir=runtime)
    path = directory + "/control"
    listener = socket.socket(socket.AF_UNIX)
    listener.bind(path)
    listener.listen(1)
    retiring = threading.Event()
    retired = threading.Event()

    def serve_retirement():
        while True:
            conn, _ = listener.accept()
            with conn:
                conn.settimeout(6)
                request = {}
                try:
                    line = conn.makefile("rb").readline(8193)
                    if len(line) > 8192:
                        raise RuntimeError("retirement request too large")
                    request = json.loads(line)
                    if request.get("server") != server:
                        raise RuntimeError("WayVNC lifetime changed")
                    allowed = request["clients"]
                    if not isinstance(allowed, list) or len(allowed) > 128:
                        raise RuntimeError("invalid client bindings")
                    retired.clear()
                    retiring.set()
                    proof = control.retire(client, request["successor"], allowed, time.monotonic() + 5)
                    conn.sendall(json.dumps({"request": request.get("request"), "retired": True, **proof}).encode() + b"\n")
                except Exception:
                    conn.sendall(json.dumps({"request": request.get("request"), "retired": False}).encode() + b"\n")
                finally:
                    retired.set()

    threading.Thread(target=serve_retirement, daemon=True).start()
    output = sys.stdout.buffer
    output.write(json.dumps({"client": client, "server": server, "path": path}).encode() + b"\n")
    output.flush()
    try:
        output.write(b"RFB 003.008\n")
        output.flush()
        if read_exact(sys.stdin.buffer, 12) != b"RFB 003.008\n":
            raise RuntimeError("unsupported viewer RFB version")
        output.write(b"\x01\x01")
        output.flush()
        if read_exact(sys.stdin.buffer, 1) != b"\x01":
            raise RuntimeError("unsupported viewer authentication")
        output.write(b"\0\0\0\0")
        output.flush()
        read_exact(sys.stdin.buffer, 1)
        output.write(server_init)
        output.flush()
        vnc.settimeout(None)

        def send_input():
            try:
                while True:
                    data = os.read(0, 65536)
                    if not data:
                        break
                    vnc.sendall(data)
            finally:
                vnc.shutdown(socket.SHUT_RDWR)

        threading.Thread(target=send_input, daemon=True).start()
        while True:
            data = upstream.read(65536)
            if not data:
                break
            output.write(data)
            output.flush()
    finally:
        if retiring.is_set():
            retired.wait(6)
        vnc.close()
        listener.close()
        control.close()
        shutil.rmtree(directory)


if __name__ == "__main__":
    def stop(signum, frame):
        raise SystemExit(0)
    signal.signal(signal.SIGHUP, stop)
    signal.signal(signal.SIGTERM, stop)
    relay()

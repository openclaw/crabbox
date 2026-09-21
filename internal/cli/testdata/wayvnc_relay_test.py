import importlib.util
import json
import pathlib
import socket
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("relay", pathlib.Path(__file__).parents[1] / "wayvnc_relay.py")
relay = importlib.util.module_from_spec(spec)
spec.loader.exec_module(relay)


class Server:
    def __init__(self, mode):
        self.mode = mode
        self.clients = [{"id": "10"}, {"id": "20"}]
        if mode == "unrelated":
            self.clients.append({"id": "30"})
        self.commands = []
        self.directory = tempfile.TemporaryDirectory(prefix="cbx-wv-", dir="/tmp")
        self.path = self.directory.name + "/ctl"
        self.socket = socket.socket(socket.AF_UNIX)
        self.socket.bind(self.path)
        self.socket.listen()
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def serve(self):
        conn, _ = self.socket.accept()
        with conn, conn.makefile("rb") as reader:
            for line in reader:
                request = json.loads(line)
                method = request["method"]
                self.commands.append(method)
                data = None
                if method == "client-list":
                    data = self.clients
                if method == "client-disconnect":
                    assert request["params"]["id"] == "10"
                    if self.mode == "restart":
                        return
                    if self.mode != "timeout":
                        self.clients = [{"id": "20"}]
                    if self.mode == "success":
                        conn.sendall(json.dumps({"method": "client-disconnected", "params": {"id": "10"}}).encode())
                try:
                    conn.sendall(json.dumps({"id": request["id"], "code": 0, "data": data}).encode())
                except BrokenPipeError:
                    return  # The timeout case deliberately closes the peer.

    def close(self):
        self.thread.join(2)
        self.socket.close()
        self.directory.cleanup()


class RetirementTests(unittest.TestCase):
    def run_case(self, mode):
        server = Server(mode)
        control = relay.Control(server.path)
        try:
            if mode in ("success", "list-ack"):
                proof = control.retire("10", "20", ["10", "20"], time.monotonic()+.3)
                self.assertEqual(proof["acknowledgement"], "client-disconnected" if mode == "success" else "client-list")
            else:
                with self.assertRaises((RuntimeError, TimeoutError, EOFError)):
                    control.retire("10", "20", ["10", "20"], time.monotonic()+.15)
            if mode == "unrelated":
                self.assertNotIn("client-disconnect", server.commands)
            else:
                self.assertEqual(server.commands.count("client-disconnect"), 1)
        finally:
            control.close()
            server.close()

    @patch.object(socket, "TCP_INFO", 11, create=True)
    def test_binding_requires_unique_live_initialized_connection(self):
        class VNC:
            state = b"\x01"
            def getsockopt(self, *args): return self.state
        class Control:
            disconnected = set()
            clients = [{"id": "10", "address": "127.1.2.3"}]
            def rpc(self, method): return self.clients
        control, vnc = Control(), VNC()
        self.assertEqual(relay.bind_client(control, vnc, "127.1.2.3"), "10")
        control.clients.append({"id": "20", "address": "127.1.2.3"})
        with self.assertRaises(RuntimeError): relay.bind_client(control, vnc, "127.1.2.3")
        control.clients.pop()
        vnc.state = b"\x08"  # CLOSE_WAIT: another client's address cannot replace ours.
        with self.assertRaises(RuntimeError): relay.bind_client(control, vnc, "127.1.2.3")
        vnc.state = b"\x01"
        control.disconnected = {"10"}
        with self.assertRaises(RuntimeError): relay.bind_client(control, vnc, "127.1.2.3")

    def test_success(self): self.run_case("success")
    def test_authoritative_list_ack(self): self.run_case("list-ack")
    def test_timeout(self): self.run_case("timeout")
    def test_unrelated_client(self): self.run_case("unrelated")
    def test_server_restart(self): self.run_case("restart")
    def test_socket_unavailable(self):
        with tempfile.TemporaryDirectory(prefix="cbx-wv-", dir="/tmp") as directory:
            with self.assertRaises(OSError): relay.Control(directory + "/missing")


if __name__ == "__main__":
    unittest.main()

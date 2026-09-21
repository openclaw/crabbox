//go:build !windows

package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestWayVNCRemoteRetirementControlSocket(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "testdata/wayvnc_relay_test.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("WayVNC control socket regressions: %v\n%s", err, output)
	}
}

// Uses an already authorized lease; this test never creates or releases one.
func TestWayVNCRelayLive(t *testing.T) {
	path := os.Getenv("CRABBOX_WAYVNC_LIVE_TARGET")
	if path == "" {
		t.Skip("set CRABBOX_WAYVNC_LIVE_TARGET to an SSH target JSON file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var target SSHTarget
	if err := json.Unmarshal(data, &target); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	connect := func() *wayVNCRelayConn {
		conn, err := dialWayVNCRelay(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		read := func(n int) []byte {
			b := make([]byte, n)
			if _, err := io.ReadFull(conn, b); err != nil {
				t.Fatal(err)
			}
			return b
		}
		if string(read(12)) != "RFB 003.008\n" {
			t.Fatal("unexpected relay banner")
		}
		_, _ = conn.Write([]byte("RFB 003.008\n"))
		if string(read(2)) != "\x01\x01" {
			t.Fatal("unexpected relay security")
		}
		_, _ = conn.Write([]byte{1})
		if string(read(4)) != "\x00\x00\x00\x00" {
			t.Fatal("relay security failed")
		}
		_, _ = conn.Write([]byte{1})
		if _, _, err := readRFBServerInit(conn); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Time{})
		encodings := []byte{2, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(encodings[4:8], 0xffffff21)  // DesktopSize (-223)
		binary.BigEndian.PutUint32(encodings[8:12], 0xfffffecc) // ExtendedDesktopSize (-308)
		if _, err := conn.Write(encodings); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	one, two := connect(), connect()
	go func() { _, _ = io.Copy(io.Discard, two) }()
	resize := func(conn *wayVNCRelayConn, width, height uint16) {
		message := make([]byte, 24)
		message[0] = 251
		message[6] = 1
		binary.BigEndian.PutUint16(message[2:4], width)
		binary.BigEndian.PutUint16(message[4:6], height)
		binary.BigEndian.PutUint16(message[16:18], width)
		binary.BigEndian.PutUint16(message[18:20], height)
		if _, err := conn.Write(message); err != nil {
			t.Fatal(err)
		}
	}
	output := func(width, height int) {
		deadline := time.Now().Add(3 * time.Second)
		for {
			out, err := runSSHOutput(ctx, target, "XDG_RUNTIME_DIR=/tmp/crabbox-runtime-$(id -u) wayvncctl --json output-list")
			var outputs []struct{ Width, Height int }
			if err == nil && json.Unmarshal([]byte(out), &outputs) == nil && len(outputs) == 1 && outputs[0].Width == width && outputs[0].Height == height {
				t.Log(out)
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("desktop did not reach %dx%d: %s (%v)", width, height, out, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	resize(one, 1280, 720)
	output(1280, 720)
	resize(two, 1600, 900)
	output(1280, 720)
	request := wayVNCRetirement{Type: "wayvnc_retire", Request: "live-test", Server: one.binding.Server, Successor: two.binding.Client, Clients: []string{one.binding.Client, two.binding.Client}}
	ack := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			ack <- false
			return
		}
		defer ws.CloseNow()
		data, _ := json.Marshal(request)
		if ws.Write(ctx, websocket.MessageText, data) != nil {
			ack <- false
			return
		}
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				ack <- false
				return
			}
			if typ == websocket.MessageText {
				var reply struct {
					Type, Request string
					Retired       bool
				}
				ack <- json.Unmarshal(data, &reply) == nil && reply.Type == "wayvnc_retired" && reply.Request == request.Request && reply.Retired
				return
			}
		}
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &webVNCBridge{tcp: one, ws: ws}
	go func() { _ = bridge.Serve(ctx) }()
	select {
	case retired := <-ack:
		if !retired {
			t.Fatal("missing authoritative retirement acknowledgement before bridge closure")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	resize(two, 1600, 900)
	output(1600, 900)
	list, err := runSSHOutput(ctx, target, "XDG_RUNTIME_DIR=/tmp/crabbox-runtime-$(id -u) wayvncctl --json client-list")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("client-list after retirement:", list)
	var clients []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(list), &clients) != nil || len(clients) != 1 || clients[0].ID != two.binding.Client {
		t.Fatal("unexpected remote clients:", list)
	}
}

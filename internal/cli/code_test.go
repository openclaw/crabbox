package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestWebCodeURLs(t *testing.T) {
	if got := webCodeAgentURL("https://broker.example.com", "cbx_abcdef123456"); got != "wss://broker.example.com/v1/leases/cbx_abcdef123456/code/agent" {
		t.Fatalf("agent URL=%q", got)
	}
	if got := webCodePortalURL("https://broker.example.com/", "cbx_abcdef123456"); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/code/" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webCodePortalURL("https://broker.example.com/", "cbx_abcdef123456", "/work/cbx/repo/worker"); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/code/?folder=%2Fwork%2Fcbx%2Frepo%2Fworker" {
		t.Fatalf("portal URL with folder=%q", got)
	}
}

func TestConnectCodeBridgeSendsTicketInDedicatedHeader(t *testing.T) {
	agentConnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/leases/cbx_abcdef123456/code/ticket":
			if r.Method != http.MethodPost {
				t.Errorf("ticket method=%s", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("authorization=%q", got)
			}
			_ = json.NewEncoder(w).Encode(coordinatorCodeTicket{
				Ticket:  "code_abcdef1234567890abcdef1234567890",
				LeaseID: "cbx_abcdef123456",
			})
		case "/v1/leases/cbx_abcdef123456/code/agent":
			if got := r.URL.Query().Get("ticket"); got != "" {
				t.Errorf("query ticket=%q", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("bridge authorization=%q", got)
			}
			if got := r.Header.Get("X-Crabbox-Bridge-Ticket"); got != "code_abcdef1234567890abcdef1234567890" {
				t.Errorf("bridge ticket=%q", got)
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("websocket accept: %v", err)
				return
			}
			close(agentConnected)
			_, _, _ = conn.Read(context.Background())
			_ = conn.Close(websocket.StatusNormalClosure, "test done")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	coord := &CoordinatorClient{BaseURL: server.URL, Token: "test-token", Client: server.Client()}
	bridge, err := connectCodeBridge(ctx, coord, "cbx_abcdef123456", "127.0.0.1", "8080")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close(websocket.StatusNormalClosure, "test done")

	select {
	case <-agentConnected:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestConnectCodeBridgeRetriesBearerWithoutTicketURL(t *testing.T) {
	var agentAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/leases/cbx_abcdef123456/code/ticket":
			_ = json.NewEncoder(w).Encode(coordinatorCodeTicket{
				Ticket:  "code_abcdef1234567890abcdef1234567890",
				LeaseID: "cbx_abcdef123456",
			})
		case "/v1/leases/cbx_abcdef123456/code/agent":
			attempt := agentAttempts.Add(1)
			if got := r.URL.Query().Get("ticket"); got != "" {
				t.Errorf("attempt %d query ticket=%q", attempt, got)
			}
			if attempt == 1 {
				if got := r.Header.Get("X-Crabbox-Bridge-Ticket"); got == "" {
					t.Error("first attempt missing dedicated bridge ticket")
				}
				http.Error(w, "older coordinator", http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer code_abcdef1234567890abcdef1234567890" {
				t.Errorf("fallback authorization=%q", got)
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("websocket accept: %v", err)
				return
			}
			_, _, _ = conn.Read(context.Background())
			_ = conn.Close(websocket.StatusNormalClosure, "test done")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	coord := &CoordinatorClient{BaseURL: server.URL, Token: "test-token", Client: server.Client()}
	bridge, err := connectCodeBridge(ctx, coord, "cbx_abcdef123456", "127.0.0.1", "8080")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close(websocket.StatusNormalClosure, "test done")
	if got := agentAttempts.Load(); got != 2 {
		t.Fatalf("agent attempts=%d want 2", got)
	}
}

func TestMappedRemoteCodeFolderTracksCurrentSubdirectory(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "worker", "src")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	got := mappedRemoteCodeFolder("/work/cbx/repo", Repo{Root: root})
	if got != "/work/cbx/repo/worker/src" {
		t.Fatalf("mapped folder=%q", got)
	}

	t.Chdir(t.TempDir())
	if got := mappedRemoteCodeFolder("/work/cbx/repo", Repo{Root: root}); got != "/work/cbx/repo" {
		t.Fatalf("outside repo folder=%q", got)
	}
}

func TestCodeUpstreamPathStripsPortalLeasePrefix(t *testing.T) {
	tests := map[string]string{
		"/portal/leases/cbx_abcdef123456/code/":                    "/",
		"/portal/leases/cbx_abcdef123456/code/static/main.js":      "/static/main.js",
		"/portal/leases/cbx_abcdef123456/code/?folder=/work/repo":  "/?folder=/work/repo",
		"/portal/leases/blue-lobster/code/vscode-remote-resource":  "/vscode-remote-resource",
		"/portal/leases/blue-lobster/vnc/viewer":                   "/portal/leases/blue-lobster/vnc/viewer",
		"/portal/leases/blue-lobster/code/proxy/3000/?q=hello+you": "/proxy/3000/?q=hello+you",
	}
	for input, want := range tests {
		got, err := codeUpstreamPath(input)
		if err != nil {
			t.Fatalf("codeUpstreamPath(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("codeUpstreamPath(%q)=%q want %q", input, got, want)
		}
	}
}

func TestCodeUpstreamPathRejectsAbsoluteURLs(t *testing.T) {
	for _, input := range []string{"https://evil.example/socket", "//evil.example/socket"} {
		if _, err := codeUpstreamPath(input); err == nil {
			t.Fatalf("codeUpstreamPath(%q) expected error", input)
		}
	}
}

func TestCodeBridgeUpstreamURLPinsLoopbackHost(t *testing.T) {
	got, err := codeBridgeUpstreamURL("http://127.0.0.1:8080", "ws", "/proxy/3000/?q=hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ws://127.0.0.1:8080/proxy/3000/?q=hello" {
		t.Fatalf("upstream url=%q", got)
	}
	for _, baseURL := range []string{"http://example.com:8080", "https://127.0.0.1:8080"} {
		if _, err := codeBridgeUpstreamURL(baseURL, "http", "/"); err == nil {
			t.Fatalf("codeBridgeUpstreamURL(%q) expected error", baseURL)
		}
	}
}

func TestCodeBridgeBodyChunkStaysBelowWebSocketFrameLimit(t *testing.T) {
	if maxCodeBridgeBodyChunkBytes%3 != 0 {
		t.Fatalf("chunk length=%d must be divisible by 3 to avoid mid-stream base64 padding", maxCodeBridgeBodyChunkBytes)
	}
	encoded := base64.StdEncoding.EncodeToString(make([]byte, maxCodeBridgeBodyChunkBytes))
	if len(encoded) >= 64*1024 {
		t.Fatalf("encoded chunk length=%d should stay below 64KiB websocket frame budget", len(encoded))
	}
	if codeBridgeBodyChunkDelay <= 0 {
		t.Fatal("large bridge responses should be paced")
	}
	if maxCodeBridgeReadBytes < 16*1024*1024 {
		t.Fatalf("read limit=%d should allow VS Code websocket messages", maxCodeBridgeReadBytes)
	}
}

func TestStartCodeServerCommand(t *testing.T) {
	got := startCodeServerCommand("/work/crabbox/cbx_abcdef123456/repo")
	for _, want := range []string{
		"/usr/local/bin/code-server",
		"--auth none",
		"--bind-addr 127.0.0.1:8080",
		"VSCODE_PROXY_URI='./proxy/{{port}}'",
		"workbench.colorTheme",
		"Default Dark Modern",
		"/tmp/crabbox-code-server.log",
		"/tmp/crabbox-code-server.pid",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("startCodeServerCommand missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pkill -f") {
		t.Fatalf("startCodeServerCommand should not use pkill -f:\n%s", got)
	}
}

func TestRewriteCodeHTMLRemovesEmptyLocaleScript(t *testing.T) {
	input := []byte(`<script type="module" src=""></script><script type="module" src="workbench.js"></script>`)
	got := string(rewriteCodeHTML(input))
	if strings.Contains(got, `src=""`) {
		t.Fatalf("rewriteCodeHTML left empty script:\n%s", got)
	}
	if !strings.Contains(got, `src="workbench.js"`) {
		t.Fatalf("rewriteCodeHTML removed non-empty script:\n%s", got)
	}
}

func TestCodeServerStaticFallbackServesVSDAStub(t *testing.T) {
	body, headers, ok := codeServerStaticFallback("/stable/static/node_modules/vsda/rust/web/vsda.js", 404)
	if !ok {
		t.Fatal("expected vsda.js fallback")
	}
	if headers.Get("content-type") != "text/javascript" {
		t.Fatalf("content-type=%q", headers.Get("content-type"))
	}
	text := string(body)
	if !strings.Contains(text, "define(") || !strings.Contains(text, "globalThis.vsda_web") {
		t.Fatalf("fallback body missing AMD vsda_web stub:\n%s", body)
	}

	wasm, headers, ok := codeServerStaticFallback("/stable/static/node_modules/vsda/rust/web/vsda_bg.wasm", 404)
	if !ok {
		t.Fatal("expected vsda wasm fallback")
	}
	if headers.Get("content-type") != "application/wasm" {
		t.Fatalf("wasm content-type=%q", headers.Get("content-type"))
	}
	if string(wasm[:4]) != "\x00asm" {
		t.Fatalf("wasm header=%v", wasm[:4])
	}

	if _, _, ok := codeServerStaticFallback("/missing.js", 404); ok {
		t.Fatal("unexpected fallback for unrelated path")
	}
	if _, _, ok := codeServerStaticFallback("/stable/static/node_modules/vsda/rust/web/vsda.js", 500); ok {
		t.Fatal("unexpected fallback for non-404")
	}
}

func TestCodeFrameType(t *testing.T) {
	if got := codeFrameType(websocket.MessageText); got != "text" {
		t.Fatalf("text frame=%q", got)
	}
	if got := codeFrameType(websocket.MessageBinary); got != "binary" {
		t.Fatalf("binary frame=%q", got)
	}
	if got := websocketMessageType("text"); got != websocket.MessageText {
		t.Fatalf("websocketMessageType text=%v", got)
	}
	if got := websocketMessageType("binary"); got != websocket.MessageBinary {
		t.Fatalf("websocketMessageType binary=%v", got)
	}
	if got := websocketMessageType(""); got != websocket.MessageBinary {
		t.Fatalf("websocketMessageType default=%v", got)
	}
}

func TestWebSocketSubprotocols(t *testing.T) {
	headers := http.Header{}
	headers.Add("Sec-WebSocket-Protocol", "vscode-remote, crabbox")
	headers.Add("Sec-WebSocket-Protocol", " second-token ")
	got := websocketSubprotocols(headers)
	want := []string{"vscode-remote", "crabbox", "second-token"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("websocketSubprotocols=%q want %q", got, want)
	}
}

func TestCodeWebSocketDialHeadersRewritesOrigin(t *testing.T) {
	headers, subprotocols := codeWebSocketDialHeaders("http://127.0.0.1:8081", map[string]string{
		"cookie":                 "vscode-tkn=remote-token",
		"origin":                 "https://broker.example.com",
		"sec-websocket-protocol": "proto-a, proto-b",
	})

	if headers.Get("Origin") != "http://127.0.0.1:8081" {
		t.Fatalf("origin=%q", headers.Get("Origin"))
	}
	if headers.Get("Cookie") != "vscode-tkn=remote-token" {
		t.Fatalf("cookie=%q", headers.Get("Cookie"))
	}
	if headers.Get("Sec-WebSocket-Protocol") != "" {
		t.Fatalf("raw subprotocol header should be removed: %q", headers.Get("Sec-WebSocket-Protocol"))
	}
	if strings.Join(subprotocols, "|") != "proto-a|proto-b" {
		t.Fatalf("subprotocols=%q", subprotocols)
	}
}

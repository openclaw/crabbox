package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Environment keys that drive the in-test fake `phala` CLI (TestPhalaCLIHelper).
// A thin platform wrapper re-execs the test binary with the helper's -test.run
// filter so the fake is cross-platform (POSIX shell + Windows .bat) and the
// canned-JSON resolution and credential-leak assertions run on Windows too.
const (
	phalaHelperEnv     = "CRABBOX_PHALA_PROXY_HELPER"
	phalaHelperStdout  = "CRABBOX_PHALA_PROXY_HELPER_STDOUT"
	phalaHelperArgv    = "CRABBOX_PHALA_PROXY_HELPER_ARGV"
	phalaHelperCode    = "CRABBOX_PHALA_PROXY_HELPER_EXIT"
	phalaHelperBin     = "CRABBOX_PHALA_PROXY_HELPER_BIN"
	phalaHelperRunFlag = "-test.run=^TestPhalaCLIHelper$"
)

// fakePhalaCLI writes a platform wrapper that, when run as `phala ...`, re-execs
// the test binary into TestPhalaCLIHelper. The helper records its argv to argvPath
// and prints the canned stdout. It returns the wrapper path and the argv-record
// path.
func fakePhalaCLI(t *testing.T, stdout string, exitCode int) (binary, argvPath string) {
	t.Helper()
	dir := t.TempDir()
	argvPath = filepath.Join(dir, "argv")
	t.Setenv(phalaHelperEnv, "1")
	t.Setenv(phalaHelperStdout, stdout)
	t.Setenv(phalaHelperArgv, argvPath)
	t.Setenv(phalaHelperCode, strconv.Itoa(exitCode))
	t.Setenv(phalaHelperBin, os.Args[0])
	if runtime.GOOS == "windows" {
		wrapper := filepath.Join(dir, "phala.bat")
		// %* forwards the cvms-get arguments to the helper after the run filter.
		body := "@echo off\r\n\"%" + phalaHelperBin + "%\" " + phalaHelperRunFlag + " %*\r\n"
		if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return wrapper, argvPath
	}
	wrapper := filepath.Join(dir, "phala")
	body := "#!/bin/sh\nexec \"$" + phalaHelperBin + "\" " + phalaHelperRunFlag + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return wrapper, argvPath
}

// TestPhalaCLIHelper is the re-exec'd fake `phala` binary, gated on phalaHelperEnv
// so it is inert during a normal test run. Arguments after the -test.run filter
// are the cvms-get invocation under test.
func TestPhalaCLIHelper(t *testing.T) {
	if os.Getenv(phalaHelperEnv) != "1" {
		return
	}
	// Record only the cvms-get invocation: drop the binary path and every leading
	// `-test.*` flag injected by the wrapper / go test harness. The Go testing
	// flag parser normalizes -test.run (e.g. strips a leading ^), so match by
	// prefix rather than exact string.
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-test.") {
		args = args[1:]
	}
	if argvPath := os.Getenv(phalaHelperArgv); argvPath != "" {
		_ = os.WriteFile(argvPath, []byte(strings.Join(args, " ")), 0o600)
	}
	if stdout := os.Getenv(phalaHelperStdout); stdout != "" {
		_, _ = os.Stdout.WriteString(stdout)
	}
	code, _ := strconv.Atoi(os.Getenv(phalaHelperCode))
	os.Exit(code)
}

func TestResolvePhalaProxyEndpoint(t *testing.T) {
	for _, test := range []struct {
		name    string
		json    string
		want    string
		wantErr string
	}{
		{
			name: "flat host and explicit port",
			json: `{"success":true,"host":"cvm-abc.dstack.example","port":2222}`,
			want: "cvm-abc.dstack.example:2222",
		},
		{
			name: "cvm wrapper gateway url carries host and port",
			json: `{"success":true,"cvm":{"gateway_url":"https://gw-1.phala.network:8443"}}`,
			want: "gw-1.phala.network:8443",
		},
		{
			name: "gateway host:port string parsed for both host and port",
			json: `{"cvm":{"gateway":"gw-2.phala.network:7000"}}`,
			want: "gw-2.phala.network:7000",
		},
		{
			name: "domain without port defaults to 443",
			json: `{"domain":"cvm-xyz.dstack.example"}`,
			want: "cvm-xyz.dstack.example:443",
		},
		{
			name: "gateway_port wins over flat port",
			json: `{"host":"cvm.example","port":10,"gateway_port":9001}`,
			want: "cvm.example:9001",
		},
		{
			name: "cvm wrapper merges host with top-level port",
			json: `{"ssh_port":6001,"cvm":{"host":"merged.example"}}`,
			want: "merged.example:6001",
		},
		{
			name:    "no host fields is an error",
			json:    `{"success":true,"port":2222}`,
			wantErr: "omitted an SSH gateway host",
		},
		{
			name:    "non-json output is an error",
			json:    "libuv: assertion failed",
			wantErr: "no JSON output",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			phala, argvPath := fakePhalaCLI(t, test.json, 0)
			var stderr bytes.Buffer
			got, err := resolvePhalaProxyEndpoint(context.Background(), phala, "cvm-id-123", &stderr)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err=%v want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Fatalf("endpoint=%q want %q", got, test.want)
			}
			argv, readErr := os.ReadFile(argvPath)
			if readErr != nil {
				t.Fatalf("read recorded argv: %v", readErr)
			}
			recorded := strings.TrimSpace(string(argv))
			if recorded != "cvms get --cvm-id cvm-id-123 --json" {
				t.Fatalf("phala argv=%q want canonical cvms get invocation", recorded)
			}
			// The phala CLI authenticates from its own stored credentials, never
			// from crabbox-passed arguments. Guard against an API key ever leaking
			// onto the command line where it would land in process listings.
			for _, leak := range []string{"api-key", "api_key", "apikey", "PHALA_CLOUD_API_KEY", "token", "secret"} {
				if strings.Contains(strings.ToLower(recorded), strings.ToLower(leak)) {
					t.Fatalf("phala argv leaked credential-like token %q: %q", leak, recorded)
				}
			}
		})
	}
}

func TestResolvePhalaProxyEndpointPropagatesCLIFailureCode(t *testing.T) {
	phala, _ := fakePhalaCLI(t, "", 7)
	var stderr bytes.Buffer
	_, err := resolvePhalaProxyEndpoint(context.Background(), phala, "cvm-id", &stderr)
	if err == nil {
		t.Fatal("expected error when phala CLI exits non-zero")
	}
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v want ExitError code 7", err)
	}
}

func TestMergePhalaProxyEndpoint(t *testing.T) {
	primary := phalaProxyEndpoint{Gateway: "primary-gw", Port: 100}
	fallback := phalaProxyEndpoint{
		Gateway:     "fallback-gw",
		GatewayHost: "fallback-host",
		GatewayURL:  "https://fallback-url",
		Host:        "fallback-direct-host",
		Domain:      "fallback-domain",
		Port:        200,
		GatewayPort: 300,
		SSHPort:     400,
	}
	merged := mergePhalaProxyEndpoint(primary, fallback)
	if merged.Gateway != "primary-gw" {
		t.Fatalf("primary gateway overwritten: %q", merged.Gateway)
	}
	if merged.Port != 100 {
		t.Fatalf("primary port overwritten: %d", merged.Port)
	}
	if merged.GatewayHost != "fallback-host" || merged.GatewayURL != "https://fallback-url" ||
		merged.Host != "fallback-direct-host" || merged.Domain != "fallback-domain" {
		t.Fatalf("blank primary fields not backfilled: %#v", merged)
	}
	if merged.GatewayPort != 300 || merged.SSHPort != 400 {
		t.Fatalf("zero primary ports not backfilled: %#v", merged)
	}

	// A whitespace-only primary string is treated as blank and backfilled.
	whitespace := mergePhalaProxyEndpoint(phalaProxyEndpoint{Gateway: "   "}, phalaProxyEndpoint{Gateway: "real-gw"})
	if whitespace.Gateway != "real-gw" {
		t.Fatalf("whitespace gateway not backfilled: %q", whitespace.Gateway)
	}
}

func TestPhalaGatewayHost(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{"gw.phala.network", "gw.phala.network"},
		{"  gw.phala.network  ", "gw.phala.network"},
		{"https://gw.phala.network", "gw.phala.network"},
		{"http://gw.phala.network", "gw.phala.network"},
		{"ssh://gw.phala.network", "gw.phala.network"},
		{"https://gw.phala.network:8443", "gw.phala.network"},
		{"gw.phala.network:8443", "gw.phala.network"},
		{"https://gw.phala.network/path?q=1#frag", "gw.phala.network"},
		{"gw.phala.network/cvm/abc", "gw.phala.network"},
		{"", ""},
		{"   ", ""},
		// net.SplitHostPort splits on the final colon without validating the port
		// is numeric, so a "host:garbage" form still yields the bare host.
		{"gw.phala.network:notaport", "gw.phala.network"},
		// A value with no host:port colon but a stray scheme-less path is trimmed
		// at the first path separator.
		{"gw.phala.network#frag", "gw.phala.network"},
	} {
		if got := phalaGatewayHost(test.raw); got != test.want {
			t.Fatalf("phalaGatewayHost(%q)=%q want %q", test.raw, got, test.want)
		}
	}
}

func TestPhalaGatewayPort(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int
	}{
		{"gw.phala.network:8443", 8443},
		{"https://gw.phala.network:8443", 8443},
		{"ssh://gw.phala.network:22", 22},
		{"gw.phala.network", 0},
		{"", 0},
		{"   ", 0},
		{"gw.phala.network:notaport", 0},
		{"https://gw.phala.network/path", 0},
	} {
		if got := phalaGatewayPort(test.raw); got != test.want {
			t.Fatalf("phalaGatewayPort(%q)=%d want %d", test.raw, got, test.want)
		}
	}
}

func TestPhalaJSONObjectPrefixDiscardsTrailingNoise(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{`{"host":"a"}` + "\nlibuv: assertion failed", `{"host":"a"}`},
		{`  [{"a":1}]  trailing`, `[{"a":1}]`},
		{`{"s":"}not-a-close"}rest`, `{"s":"}not-a-close"}`},
		{"libuv noise only", ""},
		{"", ""},
	} {
		if got := phalaJSONObjectPrefix(test.raw); got != test.want {
			t.Fatalf("phalaJSONObjectPrefix(%q)=%q want %q", test.raw, got, test.want)
		}
	}
}

// TestCopyPhalaProxyStreamsForwardsServerResponse drives the bidirectional copy
// the way `phala proxy` uses it for an SSH transport: the client sends a request,
// the server echoes a response and closes, and the response must reach the output
// writer. The input is held open until the response is observed so the read side
// completes before the write side, matching the offline gateway's request/response
// ordering rather than racing the input-EOF teardown.
func TestCopyPhalaProxyStreamsForwardsServerResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer conn.Close()
		buf := make([]byte, len("request"))
		if _, readErr := io.ReadFull(conn, buf); readErr != nil {
			serverDone <- readErr
			return
		}
		_, writeErr := conn.Write([]byte("remote-tail"))
		serverDone <- writeErr
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// input emits the request, then blocks until the server response has been
	// copied to output, so the server-read goroutine finishes first.
	responseSeen := make(chan struct{})
	input := io.MultiReader(strings.NewReader("request"), readerFunc(func(p []byte) (int, error) {
		<-responseSeen
		return 0, io.EOF
	}))
	output := &notifyingWriter{notifyAt: len("remote-tail"), done: responseSeen}
	streamDone := make(chan error, 1)
	go func() { streamDone <- copyPhalaProxyStreams(context.Background(), conn, input, output) }()

	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
	select {
	case streamErr := <-streamDone:
		if streamErr != nil {
			t.Fatalf("streamErr=%v", streamErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copyPhalaProxyStreams did not return after server closed")
	}
	if output.buf.String() != "remote-tail" {
		t.Fatalf("output=%q want remote-tail", output.buf.String())
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// notifyingWriter closes done once at least notifyAt bytes have been written.
type notifyingWriter struct {
	buf      bytes.Buffer
	notifyAt int
	done     chan struct{}
	closed   bool
}

func (w *notifyingWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if !w.closed && w.buf.Len() >= w.notifyAt {
		w.closed = true
		close(w.done)
	}
	return n, err
}

func TestCopyPhalaProxyStreamsReturnsOnCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	input, inputWriter := io.Pipe()
	defer inputWriter.Close()
	done := make(chan error, 1)
	go func() {
		done <- copyPhalaProxyStreams(ctx, client, input, io.Discard)
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil || err != context.Canceled {
			t.Fatalf("err=%v want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("copyPhalaProxyStreams did not return after cancellation")
	}
}

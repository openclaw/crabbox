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

func TestResolvePhalaProxyHostDerivesAppAndGatewayDomain(t *testing.T) {
	for _, test := range []struct {
		name    string
		json    string
		want    string
		wantErr string
	}{
		{
			name: "gateway object gateway_domain",
			json: `{"success":true,"cvm":{"app_id":"app-abc","gateway":{"gateway_domain":"gw.phala.network"}}}`,
			want: "app-abc-22.gw.phala.network",
		},
		{
			name: "gateway object nested base_domain",
			json: `{"success":true,"cvm":{"app_id":"app-xyz","gateway":{"base_domain":"base.dstack.example"}}}`,
			want: "app-xyz-22.base.dstack.example",
		},
		{
			name: "top-level app id and gateway object",
			json: `{"success":true,"app_id":"top-app","gateway":{"gateway_domain":"top.gw.example"}}`,
			want: "top-app-22.top.gw.example",
		},
		{
			name: "camelCase appId alias",
			json: `{"cvm":{"appId":"camel-app","gateway":{"gateway_domain":"camel.gw.example"}}}`,
			want: "camel-app-22.camel.gw.example",
		},
		{
			name:    "missing gateway domain is an error",
			json:    `{"success":true,"cvm":{"app_id":"app-abc","gateway":{}}}`,
			wantErr: "omitted the gateway domain",
		},
		{
			name:    "missing app id is an error",
			json:    `{"success":true,"cvm":{"gateway":{"gateway_domain":"gw.phala.network"}}}`,
			wantErr: "omitted the CVM app id",
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
			got, err := resolvePhalaProxyHost(context.Background(), phala, "cvm-id-123", &stderr)
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
				t.Fatalf("host=%q want %q", got, test.want)
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

func TestResolvePhalaProxyHostPropagatesCLIFailureCode(t *testing.T) {
	phala, _ := fakePhalaCLI(t, "", 7)
	var stderr bytes.Buffer
	_, err := resolvePhalaProxyHost(context.Background(), phala, "cvm-id", &stderr)
	if err == nil {
		t.Fatal("expected error when phala CLI exits non-zero")
	}
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v want ExitError code 7", err)
	}
}

func TestTunnelPhalaProxyStreamsThroughTLSConnection(t *testing.T) {
	host := "app-abc-22.gw.phala.network"
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	dial := func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != host+":443" {
			t.Fatalf("dial network=%q address=%q", network, address)
		}
		return client, nil
	}
	go func() {
		request := make([]byte, len("request"))
		_, _ = io.ReadFull(server, request)
		if string(request) != "request" {
			t.Errorf("gateway request=%q", request)
		}
		_, _ = server.Write([]byte("remote-tail"))
		_ = server.Close()
	}()
	output := &bytes.Buffer{}
	err := tunnelPhalaProxyWithDialer(context.Background(), host, strings.NewReader("request"), output, dial)
	if err != nil {
		t.Fatalf("tunnel error: %v", err)
	}
	if output.String() != "remote-tail" {
		t.Fatalf("output=%q want remote-tail", output.String())
	}
}

func TestPhalaTLSConfigVerifiesGatewayHostname(t *testing.T) {
	const host = "app-abc-22.gw.phala.network"
	config := phalaTLSConfig(host)
	if config.ServerName != host {
		t.Fatalf("ServerName=%q want %q", config.ServerName, host)
	}
	if config.InsecureSkipVerify {
		t.Fatal("Phala TLS config disabled certificate verification")
	}
	if config.MinVersion < 0x0303 {
		t.Fatalf("MinVersion=%x want TLS 1.2+", config.MinVersion)
	}
}

func TestTunnelPhalaProxyReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return nil, ctx.Err()
	}
	err := tunnelPhalaProxyWithDialer(ctx, "app-abc-22.gw.phala.network", strings.NewReader(""), &bytes.Buffer{}, dial)
	if err == nil || err != context.Canceled {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

// TestPhalaProxyResolvesHostViaCVMSGetWithoutCachedFlag preserves the existing
// behavior: with NO --gateway-host the proxy resolves the host itself via
// `phala cvms get` and tunnels to the derived host.
func TestPhalaProxyResolvesHostViaCVMSGetWithoutCachedFlag(t *testing.T) {
	const getStdout = `{"success":true,"app_id":"app-abc","gateway":{"gateway_domain":"gw.phala.network"}}`
	phala, argvPath := fakePhalaCLI(t, getStdout, 0)
	var tunneledHost string
	tunnel := func(_ context.Context, host string, _ io.Reader, _ io.Writer) error {
		tunneledHost = host
		return nil
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("request")}
	if err := app.phalaProxyWithTunnel(context.Background(), []string{"--phala", phala, "cvm-id-123"}, tunnel); err != nil {
		t.Fatalf("phalaProxy failed: %v", err)
	}
	// The cvms-get lookup MUST run to resolve the host.
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("phala cvms get was not invoked: %v", err)
	}
	if strings.TrimSpace(string(argv)) != "cvms get --cvm-id cvm-id-123 --json" {
		t.Fatalf("phala argv=%q", strings.TrimSpace(string(argv)))
	}
	if tunneledHost != "app-abc-22.gw.phala.network" {
		t.Fatalf("tunnel host=%q", tunneledHost)
	}
}

// TestPhalaProxyUsesCachedGatewayHostWithoutCVMSGet proves the cached
// --gateway-host short-circuits the per-connection `phala cvms get` lookup
// entirely: the fake phala CLI is never invoked (its argv record is never
// written) and the TLS tunnel connects straight to the supplied host.
func TestPhalaProxyUsesCachedGatewayHostWithoutCVMSGet(t *testing.T) {
	const host = "b60d1f55-22.dstack-pha-prod5.phala.network"
	// A fake phala that would exit non-zero AND record its argv if ever called.
	phala, argvPath := fakePhalaCLI(t, "should-not-be-called", 9)
	var tunneledHost string
	tunnel := func(_ context.Context, gotHost string, _ io.Reader, output io.Writer) error {
		tunneledHost = gotHost
		_, err := io.WriteString(output, "remote-tail")
		return err
	}
	output := &bytes.Buffer{}
	app := App{Stdout: output, Stderr: io.Discard, Stdin: strings.NewReader("request")}
	if err := app.phalaProxyWithTunnel(context.Background(), []string{"--phala", phala, "--gateway-host", host, "cvm-id-123"}, tunnel); err != nil {
		t.Fatalf("phalaProxy with cached host failed: %v", err)
	}
	// The phala CLI must NOT have been invoked: its argv record stays absent.
	if _, err := os.Stat(argvPath); err == nil {
		recorded, _ := os.ReadFile(argvPath)
		t.Fatalf("phala cvms get was invoked despite cached --gateway-host: argv=%q", string(recorded))
	}
	if tunneledHost != host {
		t.Fatalf("tunnel host=%q want %q", tunneledHost, host)
	}
	if output.String() != "remote-tail" {
		t.Fatalf("output=%q want remote-tail", output.String())
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
		// Leading human progress line ahead of the JSON object (deploy-style noise).
		{"Provisioning CVM crabbox-x...\n" + `{"app_id":"a"}`, `{"app_id":"a"}`},
	} {
		if got := phalaJSONObjectPrefix(test.raw); got != test.want {
			t.Fatalf("phalaJSONObjectPrefix(%q)=%q want %q", test.raw, got, test.want)
		}
	}
}

// TestResolvePhalaProxyHostParsesRealCVMSGetShape pins the gateway-host
// derivation against the EXACT snake_case `phala cvms get --cvm-id <id> --json`
// payload observed on real TDX hardware: top-level app_id/vm_uuid/name plus a
// nested gateway.base_domain. The host must be <appId>-22.<base_domain>.
func TestResolvePhalaProxyHostParsesRealCVMSGetShape(t *testing.T) {
	const realGetStdout = `{
  "success": true,
  "app_id": "b60d1f55eeb01f17e0a5220b4c03792248d49f92",
  "vm_uuid": "42fd1f82-7b4c-47cc-92f9-a5d39476c649",
  "name": "crabbox-cbx-abcdef123456",
  "status": "running",
  "gateway": {
    "base_domain": "dstack-pha-prod5.phala.network",
    "cname": "abc.cname.phala.network"
  }
}`
	phala, _ := fakePhalaCLI(t, realGetStdout, 0)
	var stderr bytes.Buffer
	got, err := resolvePhalaProxyHost(context.Background(), phala, "b60d1f55eeb01f17e0a5220b4c03792248d49f92", &stderr)
	if err != nil {
		t.Fatalf("resolvePhalaProxyHost failed on real cvms get shape: %v", err)
	}
	const want = "b60d1f55eeb01f17e0a5220b4c03792248d49f92-22.dstack-pha-prod5.phala.network"
	if got != want {
		t.Fatalf("host=%q want %q", got, want)
	}
}

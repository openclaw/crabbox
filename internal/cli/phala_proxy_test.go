package cli

import (
	"bytes"
	"context"
	"io"
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

// TestTunnelPhalaProxyUsesOpenSSLSClient asserts the SSH transport tunnels
// through the TLS gateway with `openssl s_client -connect <host>:443` and the
// derived host as the SNI servername, not a raw TCP dial. A fake `openssl` on
// PATH records its argv and echoes a response so the stdio piping is exercised.
func TestTunnelPhalaProxyUsesOpenSSLSClient(t *testing.T) {
	host := "app-abc-22.gw.phala.network"
	argvPath := fakeOpenSSL(t, "remote-tail")
	var stderr bytes.Buffer
	output := &bytes.Buffer{}
	err := tunnelPhalaProxy(context.Background(), host, strings.NewReader("request"), output, &stderr)
	if err != nil {
		t.Fatalf("tunnel error: %v", err)
	}
	if output.String() != "remote-tail" {
		t.Fatalf("output=%q want remote-tail", output.String())
	}
	argv, readErr := os.ReadFile(argvPath)
	if readErr != nil {
		t.Fatalf("read recorded openssl argv: %v", readErr)
	}
	recorded := strings.TrimSpace(string(argv))
	for _, want := range []string{
		"s_client",
		"-connect " + host + ":443",
		"-servername " + host,
		// TLS is the only server authentication (SSH host-key checking is off),
		// so a chain failure must abort and the leaf cert must match the host.
		"-verify_return_error",
		"-verify_hostname " + host,
	} {
		if !strings.Contains(recorded, want) {
			t.Fatalf("openssl argv=%q missing %q", recorded, want)
		}
	}
	// A raw TCP port other than the TLS gateway 443 must never be dialed.
	if strings.Contains(recorded, ":22") || strings.Contains(recorded, ":2222") {
		t.Fatalf("openssl argv dialed a raw SSH port: %q", recorded)
	}
}

func TestTunnelPhalaProxyReturnsOnCancellation(t *testing.T) {
	fakeOpenSSL(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	err := tunnelPhalaProxy(ctx, "app-abc-22.gw.phala.network", strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if err == nil || err != context.Canceled {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

// Environment keys for the fake `openssl` re-exec helper (TestOpenSSLHelper).
const (
	opensslHelperEnv    = "CRABBOX_OPENSSL_HELPER"
	opensslHelperStdout = "CRABBOX_OPENSSL_HELPER_STDOUT"
	opensslHelperArgv   = "CRABBOX_OPENSSL_HELPER_ARGV"
	opensslHelperBin    = "CRABBOX_OPENSSL_HELPER_BIN"
	opensslHelperRun    = "-test.run=^TestOpenSSLHelper$"
)

// fakeOpenSSL puts a fake `openssl` on PATH that re-execs the test binary into
// TestOpenSSLHelper. The helper records its argv, drains stdin, and emits the
// canned stdout. Re-execing the test binary (rather than a shell/.bat script)
// keeps the fake robust across cmd.exe and POSIX sh quoting. Returns the argv
// record path.
func fakeOpenSSL(t *testing.T, stdout string) (argvPath string) {
	t.Helper()
	dir := t.TempDir()
	argvPath = filepath.Join(dir, "openssl-argv")
	t.Setenv(opensslHelperEnv, "1")
	t.Setenv(opensslHelperStdout, stdout)
	t.Setenv(opensslHelperArgv, argvPath)
	t.Setenv(opensslHelperBin, os.Args[0])
	if runtime.GOOS == "windows" {
		wrapper := filepath.Join(dir, "openssl.bat")
		body := "@echo off\r\n\"%" + opensslHelperBin + "%\" " + opensslHelperRun + " %*\r\n"
		if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	} else {
		wrapper := filepath.Join(dir, "openssl")
		body := "#!/bin/sh\nexec \"$" + opensslHelperBin + "\" " + opensslHelperRun + " \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvPath
}

// TestOpenSSLHelper is the re-exec'd fake `openssl`, gated on opensslHelperEnv so
// it is inert during a normal test run. Arguments after the -test.run filter are
// the s_client invocation under test.
func TestOpenSSLHelper(t *testing.T) {
	if os.Getenv(opensslHelperEnv) != "1" {
		return
	}
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-test.") {
		args = args[1:]
	}
	if argvPath := os.Getenv(opensslHelperArgv); argvPath != "" {
		_ = os.WriteFile(argvPath, []byte(strings.Join(args, " ")), 0o600)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if stdout := os.Getenv(opensslHelperStdout); stdout != "" {
		_, _ = os.Stdout.WriteString(stdout)
	}
	os.Exit(0)
}

// TestPhalaProxyResolvesHostViaCVMSGetWithoutCachedFlag preserves the existing
// behavior: with NO --gateway-host the proxy resolves the host itself via
// `phala cvms get` and tunnels to the derived host.
func TestPhalaProxyResolvesHostViaCVMSGetWithoutCachedFlag(t *testing.T) {
	const getStdout = `{"success":true,"app_id":"app-abc","gateway":{"gateway_domain":"gw.phala.network"}}`
	phala, argvPath := fakePhalaCLI(t, getStdout, 0)
	opensslArgv := fakeOpenSSL(t, "remote-tail")
	app := App{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("request")}
	if err := app.phalaProxy(context.Background(), []string{"--phala", phala, "cvm-id-123"}); err != nil {
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
	openssl, err := os.ReadFile(opensslArgv)
	if err != nil {
		t.Fatalf("openssl was not invoked: %v", err)
	}
	if !strings.Contains(string(openssl), "-connect app-abc-22.gw.phala.network:443") {
		t.Fatalf("openssl argv=%q did not tunnel to the resolved host", string(openssl))
	}
}

// TestPhalaProxyUsesCachedGatewayHostWithoutCVMSGet proves the cached
// --gateway-host short-circuits the per-connection `phala cvms get` lookup
// entirely: the fake phala CLI is never invoked (its argv record is never
// written) and openssl tunnels straight to the supplied host.
func TestPhalaProxyUsesCachedGatewayHostWithoutCVMSGet(t *testing.T) {
	const host = "b60d1f55-22.dstack-pha-prod5.phala.network"
	// A fake phala that would exit non-zero AND record its argv if ever called.
	phala, argvPath := fakePhalaCLI(t, "should-not-be-called", 9)
	opensslArgv := fakeOpenSSL(t, "remote-tail")
	output := &bytes.Buffer{}
	app := App{Stdout: output, Stderr: io.Discard, Stdin: strings.NewReader("request")}
	if err := app.phalaProxy(context.Background(), []string{"--phala", phala, "--gateway-host", host, "cvm-id-123"}); err != nil {
		t.Fatalf("phalaProxy with cached host failed: %v", err)
	}
	// The phala CLI must NOT have been invoked: its argv record stays absent.
	if _, err := os.Stat(argvPath); err == nil {
		recorded, _ := os.ReadFile(argvPath)
		t.Fatalf("phala cvms get was invoked despite cached --gateway-host: argv=%q", string(recorded))
	}
	openssl, err := os.ReadFile(opensslArgv)
	if err != nil {
		t.Fatalf("openssl was not invoked: %v", err)
	}
	recorded := string(openssl)
	for _, want := range []string{"-connect " + host + ":443", "-servername " + host} {
		if !strings.Contains(recorded, want) {
			t.Fatalf("openssl argv=%q missing %q", recorded, want)
		}
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

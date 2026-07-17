package cli

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestResolveMacOSWebVNCCredentialsFallsBackToManagedPassword(t *testing.T) {
	target := SSHTarget{TargetOS: targetMacOS, User: "steipete"}
	var gotCommand string
	credentials, authMode, err := resolveMacOSWebVNCCredentials(
		context.Background(),
		Config{Provider: parallelsProvider},
		target,
		func(_ context.Context, gotTarget SSHTarget, command string) (string, error) {
			if gotTarget.TargetOS != target.TargetOS || gotTarget.User != target.User {
				t.Fatalf("target = %#v, want %#v", gotTarget, target)
			}
			gotCommand = command
			return "managed-secret\n", nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotCommand != "sudo cat '/var/db/crabbox/vnc.password'" {
		t.Fatalf("password command = %q", gotCommand)
	}
	if credentials.Username != "steipete" || credentials.Password != "managed-secret" {
		t.Fatalf("credentials = %#v", credentials)
	}
	if authMode != localWebVNCAuthVNC {
		t.Fatalf("auth mode = %d, want VNC", authMode)
	}
}

func TestDesktopConfigUsesResolvedProviderIdentity(t *testing.T) {
	cfg := desktopConfigForResolvedLease(
		Config{Provider: "auto", TargetOS: targetLinux},
		Server{Provider: parallelsProvider},
		SSHTarget{TargetOS: targetMacOS},
	)
	if cfg.Provider != parallelsProvider || cfg.TargetOS != targetMacOS {
		t.Fatalf("resolved desktop config=%#v", cfg)
	}
	_, authMode, err := resolveMacOSWebVNCCredentials(
		context.Background(),
		cfg,
		SSHTarget{TargetOS: targetMacOS, User: "lease-user"},
		func(context.Context, SSHTarget, string) (string, error) { return "managed-secret", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if authMode != localWebVNCAuthVNC {
		t.Fatalf("resolved provider auth mode=%d, want VNC", authMode)
	}
}

func TestResolveMacOSWebVNCCredentialsUsesProviderARDAccount(t *testing.T) {
	target := SSHTarget{TargetOS: targetMacOS, User: "lease-user"}
	credentials, authMode, err := resolveMacOSWebVNCCredentials(
		context.Background(),
		Config{Provider: "direct-webvnc-test"},
		target,
		func(context.Context, SSHTarget, string) (string, error) {
			t.Fatal("managed password fallback should not run")
			return "", nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username != "provider-user" || credentials.Password != " provider-secret " {
		t.Fatalf("credentials = %#v", credentials)
	}
	if authMode != localWebVNCAuthARD {
		t.Fatalf("auth mode = %d, want ARD", authMode)
	}
}

func TestResolveMacOSWebVNCCredentialsUsesARDForManagedAccountPassword(t *testing.T) {
	credentials, authMode, err := resolveMacOSWebVNCCredentials(
		context.Background(),
		Config{Provider: "aws"},
		SSHTarget{TargetOS: targetMacOS, User: "ec2-user"},
		func(context.Context, SSHTarget, string) (string, error) {
			return "account-secret\n", nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Username != "ec2-user" || credentials.Password != "account-secret" {
		t.Fatalf("credentials=%#v", credentials)
	}
	if authMode != localWebVNCAuthARD {
		t.Fatalf("auth mode=%d, want ARD", authMode)
	}
}

func TestCreateMacOSWebVNCHandoffKeepsTokenOutOfOpenURL(t *testing.T) {
	session := macOSWebVNCSession{
		Token:    "deadbeefcafef00d",
		Protocol: "crabbox.deadbeefcafef00d",
	}
	handoff, err := createMacOSWebVNCHandoff("6080", session, true)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(handoff.Path)
	if strings.Contains(handoff.URL, "deadbeefcafef00d") {
		t.Fatalf("handoff URL exposes token: %s", handoff.URL)
	}
	info, err := os.Stat(handoff.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("handoff permissions = %o, want 600", got)
	}
	content, err := os.ReadFile(handoff.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"http://127.0.0.1:6080/credentials",
		"ws://127.0.0.1:6080/websockify",
		"deadbeefcafef00d",
		"crabbox.deadbeefcafef00d",
		"wsProtocols",
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("handoff missing %q: %s", want, content)
		}
	}
}

func TestCreateMacOSWebVNCHandoffOmitsCredentialsForARD(t *testing.T) {
	session := macOSWebVNCSession{
		Token:    "deadbeefcafef00d",
		Protocol: "crabbox.deadbeefcafef00d",
	}
	handoff, err := createMacOSWebVNCHandoff("6080", session, false)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(handoff.Path)
	content, err := os.ReadFile(handoff.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"/credentials", `"credentialsURL"`} {
		if strings.Contains(string(content), forbidden) {
			t.Fatalf("ARD handoff contains %q: %s", forbidden, content)
		}
	}
	for _, want := range []string{
		"ws://127.0.0.1:6080/websockify",
		"deadbeefcafef00d",
		"crabbox.deadbeefcafef00d",
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("ARD handoff missing %q: %s", want, content)
		}
	}
}

func TestRandomTokenUnique(t *testing.T) {
	a, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := randomToken()
	if a == "" || a == b || len(a) != 32 {
		t.Fatalf("tokens should be unique 16-byte hex: %q %q", a, b)
	}
}

func TestMacOSWebVNCCredentialsHandler(t *testing.T) {
	session := macOSWebVNCSession{
		Token:    "deadbeef",
		Protocol: "crabbox.deadbeef",
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:6080/credentials", strings.NewReader("token=deadbeef"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	recorder := httptest.NewRecorder()
	macOSWebVNCCredentialsHandler(session, rfbCredentials{Username: "admin", Password: "secret"}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if origin := recorder.Header().Get("Access-Control-Allow-Origin"); origin != "null" {
		t.Fatalf("allow origin = %q", origin)
	}
	for _, want := range []string{`"username":"admin"`, `"password":"secret"`} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("credentials response missing %q: %s", want, recorder.Body.String())
		}
	}

	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:6080/credentials", strings.NewReader("token=deadbeef"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	macOSWebVNCCredentialsHandler(session, rfbCredentials{}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing file origin status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestMacOSWebVNCCredentialsHandlerRejectsOversizedBody(t *testing.T) {
	session := macOSWebVNCSession{Token: "deadbeef", Protocol: "crabbox.deadbeef"}
	body := strings.NewReader("token=" + strings.Repeat("x", maxMacOSWebVNCCredentialBodyBytes))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:6080/credentials", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	recorder := httptest.NewRecorder()
	macOSWebVNCCredentialsHandler(session, rfbCredentials{Username: "admin", Password: "secret"}).ServeHTTP(recorder, req)

	if recorder.Code == http.StatusOK {
		t.Fatal("oversized credential request was accepted")
	}
}

func TestMacOSWebVNCProtocolAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:6080/websockify", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "binary, crabbox.deadbeef")
	if !macOSWebVNCProtocolAllowed(req, "crabbox.deadbeef") {
		t.Fatal("matching WebSocket subprotocol should be accepted")
	}
	req.Header.Set("Sec-WebSocket-Protocol", "binary, crabbox.wrong")
	if macOSWebVNCProtocolAllowed(req, "crabbox.deadbeef") {
		t.Fatal("wrong WebSocket subprotocol should be rejected")
	}
}

func TestMacOSWebVNCSessionsUseDistinctProtocols(t *testing.T) {
	first, err := newMacOSWebVNCSession()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newMacOSWebVNCSession()
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.Protocol == second.Protocol {
		t.Fatalf("sessions should be isolated: first=%#v second=%#v", first, second)
	}
	if first.Protocol != "crabbox."+first.Token {
		t.Fatalf("protocol = %q, token = %q", first.Protocol, first.Token)
	}
}

func TestAvailableLocalVNCPortExcept(t *testing.T) {
	// The tunnel port must never equal the (possibly user-supplied) web port.
	webPort := availableLocalVNCPort()
	for i := 0; i < 20; i++ {
		if got := availableLocalVNCPortExcept(webPort); got == webPort {
			t.Fatalf("availableLocalVNCPortExcept(%q) returned the excluded port", webPort)
		}
	}
	// Excluding the fallback (5901) must still yield a different fallback.
	if got := availableLocalVNCPortExcept("5901"); got == "5901" {
		t.Errorf("availableLocalVNCPortExcept(5901) = 5901, want a different port")
	}
}

func TestWebVNCAssetsEmbedded(t *testing.T) {
	assets := webVNCAssets()
	for _, name := range []string{"rfb.js", "LICENSE.txt"} {
		b, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatalf("embedded asset %s missing: %v", name, err)
		}
		if len(b) == 0 {
			t.Fatalf("embedded asset %s is empty", name)
		}
	}
}

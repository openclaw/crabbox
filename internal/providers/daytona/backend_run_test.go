package daytona

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apidaytona "github.com/daytonaio/daytona/libs/api-client-go"
)

func TestCreateDaytonaSyncArchiveWritesTempFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := createDaytonaSyncArchive(t.Context(), Repo{Root: root}, SyncManifest{Files: []string{"hello.txt"}, Bytes: 5}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("archive temp file is empty")
	}
	if _, err := archive.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "hello.txt" {
			return
		}
	}
	t.Fatal("archive missing hello.txt")
}

func TestDaytonaToolboxUploadURL(t *testing.T) {
	sandbox := &apidaytona.Sandbox{}
	sandbox.SetToolboxProxyUrl("https://proxy.example/base/")
	got, err := daytonaToolboxUploadURL(sandbox, "sbx-123", "/tmp/crabbox archive.tgz")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://proxy.example/base/sbx-123/files/upload?path=%2Ftmp%2Fcrabbox+archive.tgz"
	if got != want {
		t.Fatalf("url=%q, want %q", got, want)
	}
}

func TestDaytonaExtractArchiveCommandCleansArchiveOnFailure(t *testing.T) {
	cmd := daytonaExtractArchiveCommand("/workspace/repo", "/tmp/crabbox-archive.tgz", "rm -rf '/workspace/repo' && ")
	for _, want := range []string{
		"rm -rf '/workspace/repo' && mkdir -p '/workspace/repo'",
		"tar -xzf '/tmp/crabbox-archive.tgz' -C '/workspace/repo'",
		"; crabbox_status=$?; rm -f '/tmp/crabbox-archive.tgz'; exit $crabbox_status",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
	if strings.Index(cmd, "rm -f '/tmp/crabbox-archive.tgz'") < strings.Index(cmd, "tar -xzf") {
		t.Fatalf("cleanup should run after extract attempt: %s", cmd)
	}
}

func TestUploadDaytonaFileStreamDoesNotPrebuffer(t *testing.T) {
	sourceReader, sourceWriter := io.Pipe()
	requestStarted := make(chan struct{})
	bodyRead := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s, want POST", r.Method)
		}
		if r.URL.Path != "/sbx-123/files/upload" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.URL.Query().Get("path") != "/tmp/archive.tgz" {
			t.Errorf("query path=%q", r.URL.Query().Get("path"))
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		close(requestStarted)
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart reader: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Errorf("next part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" {
			t.Errorf("form name=%q", part.FormName())
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Errorf("read part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodyRead <- data
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- uploadDaytonaFileStream(t.Context(), srv.Client(), srv.URL+"/sbx-123/files/upload?path=%2Ftmp%2Farchive.tgz", map[string]string{
			"Authorization": "Bearer token",
		}, sourceReader, "archive.tgz")
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("upload did not start until the source reader completed")
	}
	if _, err := sourceWriter.Write([]byte("hello archive")); err != nil {
		t.Fatal(err)
	}
	if err := sourceWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("upload did not finish")
	}
	select {
	case got := <-bodyRead:
		if string(got) != "hello archive" {
			t.Fatalf("body=%q", got)
		}
	default:
		t.Fatal("server did not read body")
	}
}

func TestUploadDaytonaFileStreamRedactsAuthorizationFromError(t *testing.T) {
	const token = "daytona-provider-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("authorization=%q", got)
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"authorization":"Bearer ` + token + `","message":"upload failed for ` + token + `"}`))
	}))
	defer srv.Close()

	err := uploadDaytonaFileStream(t.Context(), srv.Client(), srv.URL+"/files/upload?path=%2Ftmp%2Farchive.tgz", map[string]string{
		"Authorization": "Bearer " + token,
	}, strings.NewReader("archive"), "archive.tgz")
	if err == nil {
		t.Fatal("expected upload error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("err=%q still contains token", err.Error())
	}
	if !strings.Contains(err.Error(), daytonaTokenRedacted) {
		t.Fatalf("err=%q, want redacted token marker", err.Error())
	}
}

func TestDaytonaAuthRequiresOrganizationForJWT(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	cfg.Daytona.APIKey = ""
	cfg.Daytona.JWTToken = "jwt"
	cfg.Daytona.OrganizationID = ""
	_, err := newDaytonaClient(cfg, Runtime{})
	if err == nil || !strings.Contains(err.Error(), "DAYTONA_ORGANIZATION_ID") {
		t.Fatalf("err=%v, want organization requirement", err)
	}
}

func TestDaytonaAuthFallsBackToCLIConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "daytona", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{
  "activeProfile": "prod",
  "profiles": [
    {
      "id": "dev",
      "name": "dev",
      "api": {"url": "https://dev.example/api", "key": "wrong"}
    },
    {
      "id": "prod",
      "name": "prod",
      "api": {"url": "https://daytona.example/api/", "key": "cli-api-key"},
      "activeOrganizationId": "org-123"
    }
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	cfg.Daytona.APIKey = ""
	cfg.Daytona.JWTToken = ""
	cfg.Daytona.OrganizationID = ""
	cfg.Daytona.APIURL = "https://app.daytona.io/api"
	auth, err := daytonaAuthConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if auth.APIKey != "cli-api-key" || auth.OrganizationID != "org-123" {
		t.Fatalf("auth=%#v", auth)
	}
	if got := daytonaAPIURL(cfg, auth); got != "https://daytona.example/api" {
		t.Fatalf("api url=%q", got)
	}
}

func TestDaytonaEnvAuthOverridesCLIConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "daytona", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{
  "activeProfile": "initial",
  "profiles": [
    {"id": "initial", "name": "initial", "api": {"url": "https://cli.example/api", "key": "cli-api-key"}}
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	cfg.Daytona.APIKey = "env-api-key"
	cfg.Daytona.APIURL = "https://env.example/api"
	auth, err := daytonaAuthConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if auth.APIKey != "env-api-key" {
		t.Fatalf("api key=%q", auth.APIKey)
	}
	if got := daytonaAPIURL(cfg, auth); got != "https://env.example/api" {
		t.Fatalf("api url=%q", got)
	}
}

func TestApplyDaytonaProviderFlagsRejectsResourceNoops(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "class", args: []string{"--class", "standard"}},
		{name: "type", args: []string{"--type", "large"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("class", "", "")
			fs.String("type", "", "")
			values := RegisterDaytonaProviderFlags(fs, cfg)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			err := ApplyDaytonaProviderFlags(&cfg, fs, values)
			if err == nil || !strings.Contains(err.Error(), "provider=daytona") {
				t.Fatalf("err=%v, want daytona resource flag rejection", err)
			}
		})
	}
}

type blockingDeleteDaytonaAPI struct {
	fakeDaytonaDoctorAPI
	canceled chan struct{}
}

func (a *blockingDeleteDaytonaAPI) DeleteSandbox(ctx context.Context, _ string) error {
	<-ctx.Done()
	close(a.canceled)
	return ctx.Err()
}

func TestDeleteDaytonaToolboxSandboxUsesBoundedContext(t *testing.T) {
	oldTimeout := daytonaCleanupTimeout
	daytonaCleanupTimeout = 10 * time.Millisecond
	t.Cleanup(func() { daytonaCleanupTimeout = oldTimeout })
	fake := &blockingDeleteDaytonaAPI{canceled: make(chan struct{})}
	oldClient := newDaytonaClient
	newDaytonaClient = func(Config, Runtime) (daytonaAPI, error) {
		return fake, nil
	}
	t.Cleanup(func() { newDaytonaClient = oldClient })

	var stderr bytes.Buffer
	backend := &daytonaLeaseBackend{cfg: baseConfig(), rt: Runtime{Stderr: &stderr}}
	started := time.Now()
	ctx, cancel := daytonaCleanupContext()
	defer cancel()
	backend.deleteDaytonaToolboxSandbox(ctx, "sandbox-one", "lease-one")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("delete cleanup took %s, want bounded timeout", elapsed)
	}
	select {
	case <-fake.canceled:
	default:
		t.Fatal("delete did not observe cleanup context cancellation")
	}
	if !strings.Contains(stderr.String(), "context deadline exceeded") {
		t.Fatalf("stderr=%q, want timeout warning", stderr.String())
	}
}

func TestDaytonaStopRequiresExactResourceClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_333333333333"
	sandbox := apidaytona.Sandbox{}
	sandbox.SetId("sandbox-owned")
	sandbox.SetName("crabbox-test-daytona")
	sandbox.SetLabels(map[string]string{
		"crabbox":  "true",
		"provider": daytonaProvider,
		"lease":    leaseID,
		"slug":     "daytona-owned",
	})
	fake := &fakeDaytonaDoctorAPI{sandboxes: []apidaytona.Sandbox{sandbox}}
	oldClient := newDaytonaClient
	newDaytonaClient = func(Config, Runtime) (daytonaAPI, error) { return fake, nil }
	t.Cleanup(func() { newDaytonaClient = oldClient })

	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	backend := &daytonaLeaseBackend{cfg: cfg, rt: Runtime{Stderr: io.Discard}}
	err := backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("Stop error=%v, want exact-claim refusal", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("claimless stop deleted sandboxes: %#v", fake.deleted)
	}

	repoRoot := t.TempDir()
	server := Server{Provider: daytonaProvider, CloudID: sandbox.GetId(), Labels: sandbox.GetLabels()}
	if err := claimLeaseTargetForRepoConfig(leaseID, "daytona-owned", cfg, server, SSHTarget{}, repoRoot, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	// Daytona's label-filtered inventory can lag immediately after creation.
	// Exact claims must still resolve their bound sandbox without widening trust.
	fake.sandboxes = nil
	fake.getSandboxes = map[string]*apidaytona.Sandbox{sandbox.GetId(): &sandbox}
	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != sandbox.GetId() {
		t.Fatalf("exact-claim stop deleted %#v, want [%s]", fake.deleted, sandbox.GetId())
	}
}

func TestDaytonaClaimLookupRejectsRemoteOwnershipMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_343434343434"
	sandbox := apidaytona.Sandbox{}
	sandbox.SetId("sandbox-mismatch")
	sandbox.SetLabels(map[string]string{
		"crabbox":  "true",
		"provider": daytonaProvider,
		"lease":    "cbx_353535353535",
		"slug":     "daytona-mismatch",
	})
	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	server := Server{Provider: daytonaProvider, CloudID: sandbox.GetId(), Labels: map[string]string{
		"crabbox":  "true",
		"provider": daytonaProvider,
		"lease":    leaseID,
		"slug":     "daytona-mismatch",
	}}
	if err := claimLeaseTargetForRepoConfig(leaseID, "daytona-mismatch", cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDaytonaDoctorAPI{getSandboxes: map[string]*apidaytona.Sandbox{sandbox.GetId(): &sandbox}}
	oldClient := newDaytonaClient
	newDaytonaClient = func(Config, Runtime) (daytonaAPI, error) { return fake, nil }
	t.Cleanup(func() { newDaytonaClient = oldClient })

	backend := &daytonaLeaseBackend{cfg: cfg, rt: Runtime{Stderr: io.Discard}}
	err := backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "does not match exact local claim") {
		t.Fatalf("Stop error=%v, want remote ownership mismatch refusal", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("mismatched claim deleted sandboxes: %#v", fake.deleted)
	}
}

func TestDaytonaResolveRejectsClaimOwnedByAnotherRepo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_444444444444"
	sandbox := apidaytona.Sandbox{}
	sandbox.SetId("sandbox-repo-owned")
	sandbox.SetName("crabbox-test-daytona")
	sandbox.SetLabels(map[string]string{
		"crabbox":  "true",
		"provider": daytonaProvider,
		"lease":    leaseID,
		"slug":     "daytona-repo-owned",
	})
	fake := &fakeDaytonaDoctorAPI{sandboxes: []apidaytona.Sandbox{sandbox}}
	oldClient := newDaytonaClient
	newDaytonaClient = func(Config, Runtime) (daytonaAPI, error) { return fake, nil }
	t.Cleanup(func() { newDaytonaClient = oldClient })

	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	repoA := t.TempDir()
	repoB := t.TempDir()
	server := Server{Provider: daytonaProvider, CloudID: sandbox.GetId(), Labels: sandbox.GetLabels()}
	if err := claimLeaseTargetForRepoConfig(leaseID, "daytona-repo-owned", cfg, server, SSHTarget{}, repoA, time.Hour, false); err != nil {
		t.Fatal(err)
	}

	backend := &daytonaLeaseBackend{cfg: cfg, rt: Runtime{Stderr: io.Discard}}
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, Repo: Repo{Root: repoB}})
	if err == nil || !strings.Contains(err.Error(), "is claimed by repo") || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("Resolve error=%v, want cross-repository claim refusal", err)
	}
	if fake.mutated {
		t.Fatal("cross-repository resolve mutated the Daytona sandbox")
	}
}

func TestDaytonaResolveRefusesImplicitAdoption(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_555555555555"
	sandbox := apidaytona.Sandbox{}
	sandbox.SetId("sandbox-unclaimed")
	sandbox.SetLabels(map[string]string{
		"crabbox":  "true",
		"provider": daytonaProvider,
		"lease":    leaseID,
		"slug":     "daytona-unclaimed",
	})
	fake := &fakeDaytonaDoctorAPI{sandboxes: []apidaytona.Sandbox{sandbox}}
	oldClient := newDaytonaClient
	newDaytonaClient = func(Config, Runtime) (daytonaAPI, error) { return fake, nil }
	t.Cleanup(func() { newDaytonaClient = oldClient })

	cfg := baseConfig()
	cfg.Provider = daytonaProvider
	backend := &daytonaLeaseBackend{cfg: cfg, rt: Runtime{Stderr: io.Discard}}
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, Repo: Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("Resolve error=%v, want explicit-adoption refusal", err)
	}
	if fake.mutated {
		t.Fatal("claimless resolve mutated the Daytona sandbox")
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider(leaseID, daytonaProvider); claimErr != nil {
		t.Fatal(claimErr)
	} else if ok {
		t.Fatal("claimless resolve implicitly created a Daytona claim")
	}
}

func TestDaytonaSSHTargetUsesReturnedSSHCommand(t *testing.T) {
	cfg := baseConfig()
	cfg.Daytona.SSHGatewayHost = "fallback.example"
	target, err := daytonaSSHTargetFromAccess(cfg, daytonaSSHAccess{
		Token:   "tok_live_secret",
		Command: "ssh -p 2222 tok_live_secret@region-ssh.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if target.User != "tok_live_secret" || target.Host != "region-ssh.example.com" || target.Port != "2222" {
		t.Fatalf("target=%#v", target)
	}
	if target.Key != "" || !target.AuthSecret || target.NetworkKind != NetworkPublic {
		t.Fatalf("target auth/network=%#v", target)
	}
}

func TestDaytonaSSHTargetFallsBackWhenCommandMissing(t *testing.T) {
	cfg := baseConfig()
	cfg.Daytona.SSHGatewayHost = "fallback.example"
	target, err := daytonaSSHTargetFromAccess(cfg, daytonaSSHAccess{Token: "tok_live_secret"})
	if err != nil {
		t.Fatal(err)
	}
	if target.User != "tok_live_secret" || target.Host != "fallback.example" || target.Port != "22" {
		t.Fatalf("target=%#v", target)
	}
}

func TestDaytonaBackendIsHybridSDKRunAndSSHAccess(t *testing.T) {
	backend := NewDaytonaLeaseBackend(ProviderSpec{Name: daytonaProvider}, baseConfig(), Runtime{})
	if _, ok := backend.(DelegatedRunBackend); !ok {
		t.Fatal("daytona should use delegated SDK run path")
	}
	if _, ok := backend.(SSHLeaseBackend); !ok {
		t.Fatal("daytona should still expose explicit SSH access")
	}
}

func TestDaytonaCommandString(t *testing.T) {
	if got := daytonaCommandString([]string{"go", "test", "./..."}, false); got != "'go' 'test' './...'" {
		t.Fatalf("command=%q", got)
	}
	if got := daytonaCommandString([]string{"FOO=bar", "go", "test"}, false); !strings.Contains(got, "FOO=") || !strings.Contains(got, "go") {
		t.Fatalf("shell command=%q", got)
	}
	if got := daytonaCommandString([]string{"echo hello && pwd"}, true); got != "echo hello && pwd" {
		t.Fatalf("shell mode=%q", got)
	}
}

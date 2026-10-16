package dockersandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const cloudFixtureID = "sbx_fixture001"
const cloudVersionJSON = `{"client":{"version":"v0.48.0"}}`
const cloudReadyJSON = `{"checks":[{"name":"CLI binary","status":"pass"},{"name":"Binary version","status":"pass"},{"name":"Authentication","status":"pass"},{"name":"Cloud API","status":"pass"},{"name":"Cloud entitlement","status":"pass"}]}`

func newCloudTest(t *testing.T) (*cloudBackend, *recordingCommandRunner, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := newTestConfig()
	cfg.DockerSandbox.Target = "cloud"
	cfg.TTL = 30 * time.Minute
	r := newRunner(map[string]scriptedReply{"version": {stdout: cloudVersionJSON}, "diagnose": {stdout: cloudReadyJSON}, "create": {stdout: cloudFixtureID}, "ls": {stdout: `[]`}, "exec": {stdout: "proof\n"}}, nil)
	var stderr bytes.Buffer
	b := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: r, Stdout: io.Discard, Stderr: &stderr}).(*cloudBackend)
	r.onRequest = func(_ context.Context, req core.LocalCommandRequest) {
		if req.Args[0] != "--cloud" {
			t.Errorf("not a cloud command: %v", req.Args)
		}
		switch scriptKey(req.Args) {
		case "create":
			var name string
			for i, arg := range req.Args {
				if arg == "--name" {
					name = req.Args[i+1]
				}
			}
			r.defaults["ls"] = scriptedReply{stdout: `[{"id":"` + cloudFixtureID + `","name":"` + name + `","status":"running"}]`}
		case "rm":
			r.defaults["ls"] = scriptedReply{stdout: `[]`}
		case "cp":
			if strings.HasPrefix(req.Args[2], cloudFixtureID+":") {
				if err := os.WriteFile(req.Args[3], []byte("proof\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return b, r, &stderr
}

func cloudRepo(t *testing.T) core.Repo {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	for name, body := range map[string]string{"tracked.txt": "original\n", ".gitignore": "ignored.txt\n", "ignored.txt": "omit\n", "edit.txt": "eligible untracked\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "add", "tracked.txt", ".gitignore")
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("local edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return core.Repo{Name: "fixture-app", Root: root}
}

func TestDockerCloudTargetAndConfig(t *testing.T) {
	cfg := newTestConfig()
	if cfg.DockerSandbox.Target != "local" || cloudTarget(cfg) {
		t.Fatalf("default changed: %#v", cfg.DockerSandbox)
	}
	fs := flag.NewFlagSet("cloud", flag.ContinueOnError)
	values := Provider{}.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--docker-sandbox-target", "cloud"}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !cloudTarget(cfg) || !(Provider{}).RequiresDirectLeasePermission(cfg) {
		t.Fatal("cloud flag not applied")
	}
	if !(Provider{}).ConfiguredFeatures(cfg).Has(core.FeatureArchiveSync) {
		t.Fatal("cloud missing archive capability")
	}
	cfg.DockerSandbox.Target = "invalid"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("invalid target accepted")
	}
}

func TestDockerCloudReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, version, diagnose string
		code                    int
		cause                   error
		want                    string
	}{
		{name: "ready", version: cloudVersionJSON, diagnose: cloudReadyJSON},
		{name: "missing", cause: os.ErrNotExist, want: "brew install"},
		{name: "old", version: `{"client":{"version":"v0.45.0"}}`, want: "v0.48.0"},
		{name: "malformed version", version: `garbage`, want: "version JSON"},
		{name: "not logged in", version: cloudVersionJSON, diagnose: strings.Replace(cloudReadyJSON, `"Authentication","status":"pass"`, `"Authentication","status":"fail"`, 1), code: 1, want: "sbx login"},
		{name: "not entitled", version: cloudVersionJSON, diagnose: strings.Replace(cloudReadyJSON, `"Cloud entitlement","status":"pass"`, `"Cloud entitlement","status":"fail"`, 1), code: 1, want: "subscription"},
		{name: "offline", version: cloudVersionJSON, diagnose: strings.Replace(cloudReadyJSON, `"Cloud API","status":"pass"`, `"Cloud API","status":"fail"`, 1), code: 1, want: "connectivity"},
		{name: "missing checks", version: cloudVersionJSON, diagnose: `{"checks":[]}`, want: "did not pass"},
		{name: "untrusted diagnostic", version: cloudVersionJSON, diagnose: `token=fixture-secret`, code: 1, want: "diagnose"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, r, _ := newCloudTest(t)
			r.defaults["version"] = scriptedReply{stdout: tc.version, err: tc.cause}
			r.defaults["diagnose"] = scriptedReply{stdout: tc.diagnose, exitCode: tc.code}
			result, err := b.Doctor(t.Context(), core.DoctorRequest{})
			if tc.want == "" {
				if err != nil || result.Status != "ok" {
					t.Fatalf("%#v %v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %s", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("raw diagnostic leaked")
			}
			if findCall(r, "create") != nil {
				t.Fatal("doctor allocated")
			}
		})
	}
}

func TestDockerCloudReadinessBeforeWorkspaceScan(t *testing.T) {
	for _, operation := range []string{"run", "warmup"} {
		for _, workspace := range []string{"existing", "missing"} {
			t.Run(operation+"/"+workspace, func(t *testing.T) {
				b, r, stderr := newCloudTest(t)
				repo := cloudRepo(t)
				if workspace == "missing" {
					// Scanning this path fails, so the readiness error must win.
					repo.Root = filepath.Join(t.TempDir(), "absent")
				}
				r.defaults["diagnose"] = scriptedReply{
					stdout:   strings.Replace(cloudReadyJSON, `"Cloud entitlement","status":"pass"`, `"Cloud entitlement","status":"fail"`, 1),
					exitCode: 1,
				}
				var err error
				if operation == "warmup" {
					err = b.Warmup(t.Context(), core.WarmupRequest{Repo: repo})
				} else {
					_, err = b.Run(t.Context(), core.RunRequest{Repo: repo, Command: []string{"true"}})
				}
				const want = "Docker cloud readiness: account lacks Cloud Sandboxes access; activate a Docker Agentic Platform subscription, then run sbx login"
				if err == nil || err.Error() != want || core.ExitCodeForError(err, 1) != 2 {
					t.Fatalf("err=%v, want exit 2: %s", err, want)
				}
				if strings.Contains(stderr.String(), "sync candidate") {
					t.Fatalf("workspace scanned before readiness: %s", stderr.String())
				}
				for _, command := range []string{"create", "ls", "cp", "exec", "rm"} {
					if call := findCall(r, command); call != nil {
						t.Fatalf("unexpected cloud operation before readiness: %v", call.Args)
					}
				}
			})
		}
	}
}

func TestDockerCloudLifecycleArchiveDownloadsAndReuse(t *testing.T) {
	b, r, _ := newCloudTest(t)
	repo := cloudRepo(t)
	old := r.onRequest
	uploaded := map[string]string{}
	r.onRequest = func(ctx context.Context, req core.LocalCommandRequest) {
		old(ctx, req)
		if scriptKey(req.Args) == "cp" && !strings.HasPrefix(req.Args[2], cloudFixtureID+":") {
			data, err := os.Open(req.Args[2])
			if err != nil {
				t.Fatal(err)
			}
			defer data.Close()
			gz, err := gzip.NewReader(data)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			tr := tar.NewReader(gz)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
				uploaded[hdr.Name] = string(body)
			}
		}
	}
	resultPath := filepath.Join(t.TempDir(), "proof.txt")
	result, err := b.Run(t.Context(), core.RunRequest{Repo: repo, Command: []string{"printf", "hello"}, Keep: true, RequestedSlug: "cloud-proof", Downloads: []string{"proof.txt=" + resultPath}, RequiredArtifactGlobs: []string{"proof.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseID != cloudLeasePrefix+cloudFixtureID || !result.Session.Kept || len(result.Artifacts) != 1 {
		t.Fatalf("result=%#v", result)
	}
	if uploaded["tracked.txt"] != "local edit\n" || uploaded["edit.txt"] != "eligible untracked\n" {
		t.Fatalf("upload=%#v", uploaded)
	}
	if _, ok := uploaded["ignored.txt"]; ok {
		t.Fatal("ignored file uploaded")
	}
	if data, err := os.ReadFile(resultPath); err != nil || string(data) != "proof\n" {
		t.Fatalf("download=%q %v", data, err)
	}
	create := findCall(r, "create")
	joined := strings.Join(create.Args, " ")
	if strings.Contains(joined, repo.Root) || !strings.Contains(joined, "--ttl 30m0s --on-timeout delete shell") {
		t.Fatalf("create args %s", joined)
	}
	list, err := b.List(t.Context(), core.ListRequest{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%#v %v", list, err)
	}
	view, err := b.Status(t.Context(), core.StatusRequest{ID: "cloud-proof"})
	if err != nil || !view.Ready || view.ServerID != cloudFixtureID {
		t.Fatalf("status=%#v %v", view, err)
	}
	if _, _, _, err := resolveLeaseID("cloud-proof", "", false, 0); err == nil {
		t.Fatal("local resolved cloud claim")
	}
	result, err = b.Run(t.Context(), core.RunRequest{Repo: repo, ID: "cloud-proof", NoSync: true, Command: []string{"true"}})
	if err != nil || !result.Session.Reused {
		t.Fatalf("reuse=%#v %v", result, err)
	}
	if err := b.Copy(t.Context(), core.CopyRequest{ID: "cloud-proof", Source: "SANDBOX:/tmp/proof.txt", Destination: filepath.Join(t.TempDir(), "copy")}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: "cloud-proof"}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(result.LeaseID)
	if err != nil || claim.LeaseID != "" {
		t.Fatalf("claim remains %#v %v", claim, err)
	}
}

func TestDockerCloudUnownedAndChangedIdentity(t *testing.T) {
	b, r, _ := newCloudTest(t)
	repo := cloudRepo(t)
	r.defaults["ls"] = scriptedReply{stdout: `[{"id":"sbx_unowned","name":"crabbox-unowned","status":"running"}]`}
	list, err := b.List(t.Context(), core.ListRequest{})
	if err != nil || len(list) != 0 {
		t.Fatalf("unowned listed %#v %v", list, err)
	}
	for _, id := range []string{"sbx_unowned", "crabbox-unowned", cloudLeasePrefix + "sbx_unowned"} {
		if err := b.Stop(t.Context(), core.StopRequest{ID: id}); err == nil {
			t.Fatal("unowned removed")
		}
	}
	result, err := b.Run(t.Context(), core.RunRequest{Repo: repo, Command: []string{"true"}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, inventory := range []string{`[]`, `[{"id":"sbx_other","name":"crabbox-collision","status":"running"}]`, `[{"id":"` + cloudFixtureID + `","name":"crabbox-renamed","status":"running"}]`} {
		r.defaults["ls"] = scriptedReply{stdout: inventory}
		if _, err := b.Run(t.Context(), core.RunRequest{Repo: repo, ID: result.LeaseID, NoSync: true, Command: []string{"true"}}); err == nil {
			t.Fatal("identity mismatch accepted")
		}
		if err := b.Stop(t.Context(), core.StopRequest{ID: result.LeaseID}); err == nil {
			t.Fatal("identity mismatch removed")
		}
	}
	if findCall(r, "rm") != nil {
		t.Fatal("unowned mutation")
	}
}

func TestDockerCloudCreationOptionsAndDenial(t *testing.T) {
	for _, mutate := range []func(*core.Config){func(c *core.Config) { c.DockerSandbox.Clone = true }, func(c *core.Config) { c.DockerSandbox.ExtraWorkspaces = []string{"/tmp/extra"} }, func(c *core.Config) { c.TTL = 25 * time.Hour }, func(c *core.Config) { c.DockerSandbox.CPUs = 3 }, func(c *core.Config) { c.DockerSandbox.CPUs = 2; c.DockerSandbox.Memory = "8g" }, func(c *core.Config) { c.DockerSandbox.Workdir = "/home/agent" }} {
		b, r, _ := newCloudTest(t)
		mutate(&b.cfg)
		if _, err := b.Run(t.Context(), core.RunRequest{Repo: cloudRepo(t), Command: []string{"true"}}); err == nil {
			t.Fatal("invalid config accepted")
		}
		if len(r.calls) != 0 {
			t.Fatal("invalid config reached sbx")
		}
	}
	b, r, _ := newCloudTest(t)
	b.cfg.DirectLeases = "deny"
	if _, err := b.Run(t.Context(), core.RunRequest{Repo: cloudRepo(t), Command: []string{"true"}}); err == nil || !strings.Contains(err.Error(), "directLeases: deny") {
		t.Fatalf("denial=%v", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("denied allocation reached sbx")
	}
	b.cfg.DirectLeaseAllow = []string{providerName}
	if _, err := b.Run(t.Context(), core.RunRequest{Repo: cloudRepo(t), Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if cloudTTL(core.Config{}) != time.Hour {
		t.Fatal("default TTL")
	}
}

func TestDockerCloudCleanupAndPendingDelete(t *testing.T) {
	b, r, stderr := newCloudTest(t)
	repo := cloudRepo(t)
	result, err := b.Run(t.Context(), core.RunRequest{Repo: repo, Command: []string{"true"}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(result.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim, err = core.UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(claim.LeaseID, claim, claim.Labels, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(t.Context(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "would remove") || findCall(r, "rm") != nil {
		t.Fatal("dry run mutated")
	}
	old := r.onRequest
	r.onRequest = func(ctx context.Context, req core.LocalCommandRequest) {
		if scriptKey(req.Args) != "rm" {
			old(ctx, req)
		}
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: result.LeaseID}); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending=%v", err)
	}
	retained, _ := core.ReadLeaseClaim(result.LeaseID)
	if retained.LeaseID == "" {
		t.Fatal("pending deletion lost claim")
	}
	if retained.Labels["docker_sandbox_delete_requested"] != "true" {
		t.Fatal("accepted deletion was not durably recorded")
	}
	view, err := b.Status(t.Context(), core.StatusRequest{ID: result.LeaseID})
	if err != nil || view.Ready || view.State != "deleting" {
		t.Fatalf("pending deletion status=%#v %v", view, err)
	}
	r.onRequest = old
	r.defaults["ls"] = scriptedReply{stdout: `[]`}
	if err := b.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	retained, _ = core.ReadLeaseClaim(result.LeaseID)
	if retained.LeaseID != "" {
		t.Fatal("cleanup retained claim")
	}
	removals := 0
	for _, call := range r.calls {
		if scriptKey(call.Args) == "rm" {
			removals++
		}
	}
	if removals != 1 {
		t.Fatalf("pending deletion was resubmitted %d times", removals)
	}
}

func TestDockerCloudFailuresPreserveOrCleanHandle(t *testing.T) {
	for _, stage := range []string{"create", "cp", "exec"} {
		t.Run(stage, func(t *testing.T) {
			b, r, _ := newCloudTest(t)
			repo := cloudRepo(t)
			r.defaults[stage] = scriptedReply{exitCode: 1, err: errors.New("fixture transport failure")}
			result, err := b.Run(t.Context(), core.RunRequest{Repo: repo, Command: []string{"true"}})
			if err == nil {
				t.Fatal("failure lost")
			}
			if stage == "create" {
				if findCall(r, "rm") != nil {
					t.Fatal("ambiguous create removed by name")
				}
				if !strings.Contains(err.Error(), "inspect sbx --cloud ls") {
					t.Fatal(err)
				}
			} else if result.Session == nil || result.Session.Kept || findCall(r, "rm") == nil {
				t.Fatalf("no rollback %#v %v", result, err)
			}
		})
	}
	b, r, _ := newCloudTest(t)
	repo := cloudRepo(t)
	r.defaults["exec"] = scriptedReply{exitCode: 7}
	result, err := b.Run(t.Context(), core.RunRequest{Repo: repo, NoSync: true, Command: []string{"false"}, KeepOnFailure: true})
	var exit core.ExitError
	if !errors.As(err, &exit) || exit.Code != 7 || !result.Session.Kept {
		t.Fatalf("exit %#v %v", result, err)
	}
}

func TestDockerCloudNativeParsingAndRedaction(t *testing.T) {
	b, r, _ := newCloudTest(t)
	c, _ := newSBXCLI(b.cfg, b.rt)
	for _, value := range []string{`{}`, `null`, `{"sandboxes":null}`, `[null]`, `[{"name":"crabbox-bad"}]`, `[{"id":"--flag","name":"crabbox-bad"}]`, `[{"id":"sbx_one","name":"one"},{"id":"sbx_one","name":"two"}]`} {
		r.defaults["ls"] = scriptedReply{stdout: value}
		if _, err := c.cloudList(t.Context()); err == nil {
			t.Fatal("invalid inventory accepted")
		}
	}
	for _, value := range []string{"sbx_one\nsbx_two", "../../bad", "--bad", "Created mystery"} {
		r.defaults["create"] = scriptedReply{stdout: value}
		if _, err := c.cloudCreate(t.Context(), "crabbox-test"); err == nil {
			t.Fatal("invalid create output accepted")
		}
	}
	r.defaults["cp"] = scriptedReply{exitCode: 1, stderr: "credential=fixture-secret"}
	if err := c.copy(t.Context(), "local", "sbx_one:/dest", false); err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("redaction=%v", err)
	}
	b.cfg.DockerSandbox.MCP = []string{"fixture-server"}
	b.cfg.DockerSandbox.CPUs = 4
	b.cfg.DockerSandbox.Memory = "8g"
	c.cfg = b.cfg
	r.defaults["create"] = scriptedReply{stdout: cloudFixtureID}
	if _, err := c.cloudCreate(t.Context(), "crabbox-test"); err != nil {
		t.Fatal(err)
	}
	last := r.calls[len(r.calls)-1].Args
	for _, sequence := range []string{"--static-mcp fixture-server", "--cpus 4", "--memory 8g"} {
		if !strings.Contains(strings.Join(last, " "), sequence) {
			t.Fatalf("args=%v missing %s", last, sequence)
		}
	}
}

func TestDockerCloudArtifactRejectsSymlinksAndLargeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "large", "host capture overflow"} {
		t.Run(kind, func(t *testing.T) {
			b, r, _ := newCloudTest(t)
			result, err := b.Run(t.Context(), core.RunRequest{Repo: cloudRepo(t), Command: []string{"true"}, Keep: true})
			if err != nil {
				t.Fatal(err)
			}
			claim, _ := core.ReadLeaseClaim(result.LeaseID)
			r.defaults["exec"] = scriptedReply{exitCode: 1}
			if kind == "large" {
				r.defaults["exec"] = scriptedReply{stdout: strings.Repeat("x", 65)}
			}
			if kind == "host capture overflow" {
				r.defaults["exec"] = scriptedReply{err: errors.New("captured command output exceeded 65-byte limit")}
			}
			r.onRequest = func(ctx context.Context, req core.LocalCommandRequest) {
				if scriptKey(req.Args) != "exec" || req.MaxCapturedOutputBytes != 65 || req.Stdout != nil || req.Stderr != nil {
					t.Fatalf("artifact transfer must use bounded capture, not disk copy: %#v", req)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
					t.Fatal("artifact capture has no bounded timeout")
				}
				command := strings.Join(req.Args, " ")
				if !strings.Contains(command, `test -f "$1" && test ! -L "$1"`) || !strings.Contains(command, `head -c "$2"`) {
					t.Fatalf("missing remote type or byte guard: %s", command)
				}
			}
			c, _ := newSBXCLI(b.cfg, b.rt)
			_, err = (cloudRunDownloader{b, c, claim}).FetchRunFile(t.Context(), core.DelegatedRunDownloadRequest{LeaseID: claim.LeaseID, RemotePath: "proof", MaxBytes: 64})
			if err == nil {
				t.Fatal("unsafe artifact accepted")
			}
		})
	}
}

func TestDockerCloudConfigProjection(t *testing.T) {
	cfg := newTestConfig()
	cfg.DockerSandbox.Target = "cloud"
	section := Provider{}.ConfigShowSection(cfg)
	for _, field := range section.Fields {
		if field.JSONName == "target" {
			if !reflect.DeepEqual(field.JSONValue, "cloud") {
				t.Fatal(field)
			}
			return
		}
	}
	data, _ := json.Marshal(section)
	t.Fatalf("target absent %s", data)
}

func TestDockerCloudArtifactCaptureBoundsMaliciousCLI(t *testing.T) {
	cfg := newTestConfig()
	cfg.DockerSandbox.Target = "cloud"
	cfg.DockerSandbox.CLIPath = filepath.Join(t.TempDir(), "sbx")
	if err := os.WriteFile(cfg.DockerSandbox.CLIPath, []byte("#!/bin/sh\nexec /usr/bin/head -c 1048576 /dev/zero\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := newSBXCLI(cfg, core.RuntimeForProviderOperation(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.cloudReadFile(t.Context(), cloudFixtureID, "/tmp/result", 64)
	if err == nil || len(data) != 0 {
		t.Fatalf("unbounded artifact returned: bytes=%d err=%v", len(data), err)
	}
}

func TestDockerCloudCoreMetadataIsNotUserEnvironmentForwarding(t *testing.T) {
	args, err := cloudCommandEnv(map[string]string{"CRABBOX_RUN_ID": "run_fixture", "CRABBOX_LEASE_ID": "lease_fixture", "CRABBOX_SLUG": "fixture"})
	if err != nil || len(args) != 6 {
		t.Fatalf("metadata args=%v err=%v", args, err)
	}
	_, err = cloudCommandEnv(map[string]string{"SECRET_TOKEN": "fixture-secret"})
	if err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("user env rejection=%v", err)
	}
}

func TestDockerCloudRunAcceptsCoreExecutionMetadata(t *testing.T) {
	b, r, _ := newCloudTest(t)
	_, err := b.Run(t.Context(), core.RunRequest{Repo: cloudRepo(t), NoSync: true, Keep: true, Command: []string{"true"}, Env: map[string]string{"CRABBOX_RUN_ID": "run_fixture", "CRABBOX_LEASE_ID": "lease_fixture", "CRABBOX_SLUG": "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	var command []string
	for _, call := range r.calls {
		if scriptKey(call.Args) == "exec" {
			command = call.Args
		}
	}
	for _, value := range []string{"CRABBOX_RUN_ID=run_fixture", "CRABBOX_LEASE_ID=lease_fixture", "CRABBOX_SLUG=fixture"} {
		if !strings.Contains(strings.Join(command, " "), value) {
			t.Fatalf("core metadata missing from command: %v", command)
		}
	}
}

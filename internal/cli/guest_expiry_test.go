package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"
)

func requireGuestExpiryShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux guest fixtures require POSIX tools")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX shell unavailable")
	}
}

func TestGuestExpiryDecisionParity(t *testing.T) {
	requireGuestExpiryShell(t)
	now := time.Unix(1800000000, 0).UTC()
	for _, tc := range []struct {
		name, owner, state, keep, expiry string
		want                             bool
	}{
		{"unowned terminal", "", "failed", "false", "", false},
		{"foreign", "false", "ready", "false", "1", false},
		{"unlabeled", "", "", "", "", false},
		{"kept expired", "true", "ready", "true", "1799999999", true},
		{"kept future", "true", "ready", "true", "1800000001", false},
		{"at expiry", "true", "ready", "false", "1800000000", false},
		{"running grace", "true", "running", "false", "1799996400", false},
		{"running expired", "true", "running", "true", "1799996399", true},
		{"provisioning expired", "true", "provisioning", "true", "1799996399", true},
		{"failed", "true", "failed", "true", "", true},
		{"released", "true", "released", "true", "1800000001", true},
		{"expired", "true", "expired", "false", "", true},
		{"active", "true", "active", "false", "1", true},
		{"leased", "true", "leased", "false", "1", true},
		{"missing deadline", "true", "ready", "false", "", false},
		{"invalid deadline", "true", "ready", "false", "$(touch should-not-exist)", false},
		{"overflow", "true", "ready", "false", "999999999999999999999", false},
		{"negative", "true", "ready", "false", "-1", false},
		{"zero", "true", "ready", "false", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{"crabbox": tc.owner, "state": tc.state, "keep": tc.keep, "expires_at": tc.expiry}
			got, reason := shouldCleanupServer(Server{Labels: labels}, now)
			if got != tc.want {
				t.Fatalf("Go=%t (%s) want=%t", got, reason, tc.want)
			}
			script := guestExpiryDecision + "\ncrabbox=" + shellQuote(tc.owner) + "\nstate=" + shellQuote(tc.state) + "\nkeep=" + shellQuote(tc.keep) + "\nexpires_at=" + shellQuote(tc.expiry) + "\nnow=1800000000\nif expiry_due; then echo due; else echo skip; fi\n"
			output, err := exec.Command("sh", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("shell: %v: %s", err, output)
			}
			if (strings.TrimSpace(string(output)) == "due") != tc.want {
				t.Fatalf("shell=%q want=%t", output, tc.want)
			}
		})
	}
}

func TestGuestExpiryGuardProviders(t *testing.T) {
	for _, name := range []string{"aws", "gcp", "azure", "hetzner", "digitalocean", "vultr", "linode", "ovh", "scaleway", "nebius", "lambda", "tencentcloud", "incus", "multipass", "firecracker"} {
		t.Run(name, func(t *testing.T) {
			cfg := WithGuestLeaseExpiry(Config{Provider: name, TargetOS: targetLinux}, map[string]string{"crabbox": "true", "state": "leased", "keep": "true", "expires_at": "1800000000"})
			data := cloudInit(cfg, "ssh-ed25519 fixture")
			var document struct {
				Files []struct{ Path, Owner, Permissions, Content string } `yaml:"write_files"`
			}
			if err := yaml.Unmarshal([]byte(data), &document); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{}
			for _, file := range document.Files {
				files[file.Path] = file.Content
				if strings.Contains(file.Path, "expiry") && file.Owner != "root:root" {
					t.Fatalf("owner=%q", file.Owner)
				}
			}
			script := files["/usr/local/sbin/crabbox-expiry-guard"]
			if script == "" || !strings.Contains(data, "OnUnitActiveSec=2min") || !strings.Contains(data, "systemctl enable --now crabbox-expiry-guard.timer") {
				t.Fatal("missing guard")
			}
			switch name {
			case "gcp":
				if !strings.Contains(script, "-X DELETE") || strings.Contains(script, "systemctl poweroff") {
					t.Fatal("GCP reclaim")
				}
			case "aws":
				if !strings.Contains(script, "tags/instance/$1") || !strings.Contains(script, "X-aws-ec2-metadata-token") {
					t.Fatal("AWS source")
				}
			case "azure":
				if !strings.Contains(script, ".compute.tagsList[]") {
					t.Fatal("Azure source")
				}
			default:
				if !strings.Contains(files["/etc/crabbox/lease-expiry"], "expires_at=1800000000") {
					t.Fatal("missing file deadline")
				}
			}
			if name != "gcp" && !strings.Contains(script, "systemctl poweroff") {
				t.Fatal("missing poweroff")
			}
			if shellcheck, err := exec.LookPath("shellcheck"); err == nil {
				cmd := exec.Command(shellcheck, "-s", "sh", "-")
				cmd.Stdin = strings.NewReader(script)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("shellcheck: %v\n%s", err, output)
				}
			}
			for _, target := range []string{targetWindows, targetMacOS} {
				cfg.TargetOS = target
				if cloudInitGuestExpiryGuardFiles(cfg) != "" {
					t.Fatal("non-Linux guard")
				}
			}
		})
	}
	for _, name := range []string{"aws", "azure", "gcp", "hetzner"} {
		cfg := Config{Provider: name, TargetOS: targetLinux, Coordinator: "https://coordinator.example"}
		if cloudInitGuestExpiryGuardFiles(cfg) != "" {
			t.Fatalf("brokered %s got guard", name)
		}
	}
}

func TestAWSDirectGuestExpiryLaunchOptions(t *testing.T) {
	for _, tc := range []struct {
		target, coordinator string
		want                bool
	}{{targetLinux, "", true}, {targetLinux, "https://coordinator.example", false}, {targetWindows, "", false}, {targetMacOS, "", false}} {
		input := &ec2.RunInstancesInput{}
		applyAWSGuestExpiryOptions(input, Config{Provider: "aws", TargetOS: tc.target, Coordinator: tc.coordinator})
		if !tc.want {
			if input.MetadataOptions != nil || input.InstanceInitiatedShutdownBehavior != "" {
				t.Fatal("unexpected launch options")
			}
			continue
		}
		if input.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorTerminate || input.MetadataOptions.InstanceMetadataTags != types.InstanceMetadataTagsStateEnabled || input.MetadataOptions.HttpTokens != types.HttpTokensStateRequired {
			t.Fatalf("options=%+v", input)
		}
		for _, tag := range awsTagsWithName(DirectLeaseLabels(baseConfig(), "cbx_123456abcdef", "fixture", "aws", "", false, time.Now()), "fixture") {
			if strings.ContainsAny(*tag.Key, " /\t\n") || *tag.Key == "." || *tag.Key == ".." || *tag.Key == "_index" {
				t.Fatalf("IMDS-illegal tag key %q", *tag.Key)
			}
		}
	}
}

func TestRefreshGuestLeaseExpiryAtomic(t *testing.T) {
	requireGuestExpiryShell(t)
	old := refreshGuestLeaseExpirySSH
	defer func() { refreshGuestLeaseExpirySSH = old }()
	fields := GuestLeaseExpiryFields{"true", "ready", "true", "1800000000"}
	called := false
	refreshGuestLeaseExpirySSH = func(ctx context.Context, target SSHTarget, remote, input string) error {
		called = true
		if target.Host != "fixture" || remote != "sudo -n sh -c "+shellQuote(guestExpiryRefreshScript)+" crabbox-expiry "+shellQuote(fmt.Sprintf("%x", sha256.Sum256([]byte(input)))) {
			t.Fatal("incorrect SSH transport")
		}
		if input != "crabbox=true\nstate=ready\nkeep=true\nexpires_at=1800000000\n" {
			t.Fatalf("input=%q", input)
		}
		return nil
	}
	if err := RefreshGuestLeaseExpiry(context.Background(), SSHTarget{Host: "fixture"}, fields); err != nil || !called {
		t.Fatalf("refresh: %v", err)
	}
	fields.State = "ready\n$(touch injected)"
	called = false
	if err := RefreshGuestLeaseExpiry(context.Background(), SSHTarget{}, fields); err == nil || called {
		t.Fatal("unsafe fields reached SSH")
	}
	// Execute the actual transaction locally, replacing only ownership operations
	// unavailable to non-root tests. Observe atomic rename and retained old bytes
	// when the input producer fails before publication.
	dir := t.TempDir()
	file := filepath.Join(dir, "lease-expiry")
	if err := os.WriteFile(file, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(guestExpiryRefreshScript, "/etc/crabbox", shellQuote(dir))
	script = strings.ReplaceAll(script, " -o root -g root", "")
	script = strings.ReplaceAll(script, "chown root:root \"$file\"", ":")
	// macOS mv has no -T; both forms perform the same atomic file rename here.
	script = strings.ReplaceAll(script, "mv -fT --", "mv -f --")
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("SHA-256 CLI unavailable")
		}
		script = strings.ReplaceAll(script, "sha256sum", "shasum -a 256")
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte("new")))
	cmd := exec.Command("sh", "-c", script, "crabbox-expiry", checksum)
	cmd.Stdin = strings.NewReader("new")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("transaction: %v %s", err, output)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "new" {
		t.Fatalf("data=%q", data)
	}
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0600 {
		t.Fatal("permissions")
	}
	failed := strings.Replace(script, "cat >\"$file\"", "false >\"$file\"", 1)
	if exec.Command("sh", "-c", failed, "crabbox-expiry", checksum).Run() == nil {
		t.Fatal("expected failed write")
	}
	data, _ = os.ReadFile(file)
	if string(data) != "new" {
		t.Fatal("failed write replaced deadline")
	}
	truncated := exec.Command("sh", "-c", script, "crabbox-expiry", checksum)
	truncated.Stdin = strings.NewReader("ne")
	if truncated.Run() == nil {
		t.Fatal("truncated transfer was published")
	}
	data, _ = os.ReadFile(file)
	if string(data) != "new" {
		t.Fatal("truncated transfer replaced deadline")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary file leaked")
	}
}

func TestGuestExpiryMetadataFailsClosed(t *testing.T) {
	requireGuestExpiryShell(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	for _, adapter := range []string{"aws", "azure"} {
		for _, mode := range []string{"expired", "future", "foreign", "missing", "injected", "duplicate"} {
			t.Run(adapter+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				mock := `#!/bin/sh
case "$*" in
 *latest/api/token*) echo synthetic-token ;;
 *latest/meta-data/tags/instance/*)
  arguments="$*"
  case "${arguments##*/}" in
   crabbox) if [ "$MODE" = foreign ]; then echo false; else echo true; fi ;;
   state) if [ "$MODE" = missing ]; then exit 22; else echo ready; fi ;;
   keep) echo true ;;
   expires_at) case "$MODE" in future) echo 999999999999 ;; injected) echo '$(touch injected)' ;; *) echo 1 ;; esac ;;
  esac ;;
 *metadata/instance*)
  owner=true; expires=1
  [ "$MODE" != foreign ] || owner=false
  [ "$MODE" != future ] || expires=999999999999
  if [ "$MODE" = injected ]; then expires='$(touch injected)'; fi
  extra=''; [ "$MODE" != duplicate ] || extra=',{"name":"crabbox","value":"false"}'
  state=',{"name":"state","value":"ready"}'; [ "$MODE" != missing ] || state=''
  printf '{"compute":{"tagsList":[{"name":"crabbox","value":"%s"},{"name":"keep","value":"true"},{"name":"expires_at","value":"%s"}%s%s]}}\n' "$owner" "$expires" "$state" "$extra" ;;
 *) exit 22 ;;
esac
`
				for name, content := range map[string]string{"curl": mock, "systemctl": "#!/bin/sh\ntouch \"$RECLAIMED\"\n", "logger": "#!/bin/sh\nexit 0\n"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
						t.Fatal(err)
					}
				}
				cmd := exec.Command("sh", "-c", guestExpiryScript(adapter))
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "MODE="+mode, "RECLAIMED="+filepath.Join(dir, "reclaimed"), "INJECTED="+filepath.Join(dir, "injected"))
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("guard: %v %s", err, output)
				}
				_, err := os.Stat(filepath.Join(dir, "reclaimed"))
				want := mode == "expired" || adapter == "aws" && mode == "duplicate"
				if (err == nil) != want {
					t.Fatalf("reclaim=%t want=%t", err == nil, want)
				}
				if _, err := os.Stat(filepath.Join(dir, "injected")); err == nil {
					t.Fatal("metadata executed")
				}
			})
		}
	}
}

func TestAWSDirectGuestExpiryLaunchRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		calls++
		for key, want := range map[string]string{"Action": "RunInstances", "InstanceInitiatedShutdownBehavior": "terminate", "MetadataOptions.InstanceMetadataTags": "enabled", "MetadataOptions.HttpTokens": "required"} {
			if got := r.Form.Get(key); got != want {
				t.Errorf("%s=%q want=%q", key, got, want)
			}
		}
		writeEC2Error(w, "InsufficientInstanceCapacity", "synthetic rejection", http.StatusBadRequest)
	}))
	defer server.Close()
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.AWSRootGB = 40
	_, err := testAWSClient(server.URL).createServer(context.Background(), cfg, "ssh-ed25519 fixture", "cbx_123456abcdef", "fixture", true, "ami-fixture", "sg-fixture", false, nil)
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

type guestExpiryTouchBackend struct{ fields map[string]string }

func (b guestExpiryTouchBackend) Spec() ProviderSpec { return ProviderSpec{Name: "hetzner"} }
func (b guestExpiryTouchBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{Labels: b.fields}, nil
}
func TestGuestExpiryTouchRefreshesFile(t *testing.T) {
	old := refreshGuestLeaseExpirySSH
	defer func() { refreshGuestLeaseExpirySSH = old }()
	calls := 0
	refreshGuestLeaseExpirySSH = func(_ context.Context, target SSHTarget, _, content string) error {
		calls++
		if target.Host != "fixture" || !strings.Contains(content, "state=running\n") || !strings.Contains(content, "expires_at=1800000000\n") {
			t.Fatalf("target=%v content=%q", target.Host, content)
		}
		return nil
	}
	backend := guestExpiryTouchBackend{map[string]string{"crabbox": "true", "state": "running", "keep": "true", "expires_at": "1800000000"}}
	req := TouchRequest{Lease: LeaseTarget{SSH: SSHTarget{Host: "fixture"}}}
	if _, err := touchLeaseWithGuestExpiry(context.Background(), backend, Config{Provider: "hetzner", TargetOS: targetLinux}, req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("file was not refreshed")
	}
	if _, err := touchLeaseWithGuestExpiry(context.Background(), backend, Config{Provider: "aws", TargetOS: targetLinux}, req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("metadata source refreshed a file")
	}
}

func TestGuestExpiryFileSnapshot(t *testing.T) {
	requireGuestExpiryShell(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "lease-expiry")
	if err := os.WriteFile(file, []byte("crabbox=true\nstate=running\nkeep=true\nexpires_at=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("#!/bin/sh\necho 0:600\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(guestExpiryFileSource, "/etc/crabbox/lease-expiry", file) +
		"test \"$(label crabbox)\" = true\nprintf 'crabbox=true\\nstate=ready\\nkeep=true\\nexpires_at=999999999999\\n' >\"$file\"\ntest \"$(label expires_at)\" = 1\n"
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("snapshot changed during read: %v %s", err, output)
	}
}

package machine0

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"golang.org/x/crypto/ssh"
)

type fakeAPI struct {
	machine                machine
	machines               []machine
	listCalls              int
	listFn                 func(context.Context, int) ([]machine, error)
	getSequence            []machine
	getFn                  func(context.Context, string) (machine, error)
	createFn               func(context.Context, createMachineRequest) error
	createErr              error
	selectedKey            *machineKey
	selectedKeyErr         error
	removeErr              error
	sizes                  []machineSize
	created                []createMachineRequest
	stopped                []string
	started                []string
	suspended              []string
	removed                []string
	primed                 []string
	images                 []machineImage
	imageDetail            machineImageDetail
	imageDetails           []machineImageDetail
	imageErrors            []error
	imageFn                func(context.Context, string) (machineImageDetail, error)
	savedImages            [][2]string
	removedImage           []string
	stopErr                error
	startErr               error
	saveErr                error
	versionErr             error
	actions                []string
	primeSSH               func(string) error
	doctorDelay            time.Duration
	imageSnapshotReady     bool
	rejectStartBeforeReady bool
}

func (f *fakeAPI) Version(ctx context.Context) (string, error) {
	if f.versionErr != nil {
		return "", f.versionErr
	}
	if err := f.waitDoctorProbe(ctx); err != nil {
		return "", err
	}
	return "machine0 1.0.155", nil
}

func (f *fakeAPI) List(ctx context.Context) ([]machine, error) {
	if err := f.waitDoctorProbe(ctx); err != nil {
		return nil, err
	}
	f.listCalls++
	if f.listFn != nil {
		return f.listFn(ctx, f.listCalls)
	}
	if f.machines != nil {
		return append([]machine(nil), f.machines...), nil
	}
	if f.machine.ID == "" {
		return nil, nil
	}
	return []machine{f.machine}, nil
}
func (f *fakeAPI) Get(ctx context.Context, name string) (machine, error) {
	if f.getFn != nil {
		return f.getFn(ctx, name)
	}
	if len(f.getSequence) > 0 {
		item := f.getSequence[0]
		f.getSequence = f.getSequence[1:]
		f.machine = item
		return item, nil
	}
	return f.machine, nil
}
func (f *fakeAPI) SelectedKey(context.Context, string) (*machineKey, error) {
	return f.selectedKey, f.selectedKeyErr
}
func (f *fakeAPI) Create(ctx context.Context, req createMachineRequest) error {
	f.created = append(f.created, req)
	for index := range f.getSequence {
		f.getSequence[index].Name = req.Name
	}
	if f.createFn != nil {
		return f.createFn(ctx, req)
	}
	return f.createErr
}
func (f *fakeAPI) Start(_ context.Context, name string) error {
	f.started = append(f.started, name)
	f.actions = append(f.actions, "start")
	if f.rejectStartBeforeReady && !f.imageSnapshotReady {
		return errors.New("fake start attempted before image snapshot was ready")
	}
	return f.startErr
}
func (f *fakeAPI) Stop(_ context.Context, name string) error {
	f.stopped = append(f.stopped, name)
	f.actions = append(f.actions, "stop")
	return f.stopErr
}
func (f *fakeAPI) Suspend(_ context.Context, name string) error {
	f.suspended = append(f.suspended, name)
	f.machine.Status, f.machine.IP = "SUSPENDED", ""
	for index := range f.machines {
		if f.machines[index].Name == name {
			f.machines[index].Status = "SUSPENDED"
			f.machines[index].IP = ""
		}
	}
	return nil
}
func (f *fakeAPI) Remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return f.removeErr
}
func (f *fakeAPI) PrimeSSH(_ context.Context, name string) error {
	f.primed = append(f.primed, name)
	if f.primeSSH != nil {
		return f.primeSSH(name)
	}
	return nil
}
func (f *fakeAPI) Sizes(ctx context.Context) ([]machineSize, error) {
	if err := f.waitDoctorProbe(ctx); err != nil {
		return nil, err
	}
	return f.sizes, nil
}

func (f *fakeAPI) waitDoctorProbe(ctx context.Context) error {
	if f.doctorDelay == 0 {
		return nil
	}
	timer := time.NewTimer(f.doctorDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (f *fakeAPI) ListImages(context.Context) ([]machineImage, error) { return f.images, nil }
func (f *fakeAPI) GetImage(ctx context.Context, name string) (machineImageDetail, error) {
	if f.imageFn != nil {
		return f.imageFn(ctx, name)
	}
	if len(f.imageErrors) > 0 && f.imageErrors[0] != nil {
		err := f.imageErrors[0]
		f.imageErrors = f.imageErrors[1:]
		return machineImageDetail{}, err
	}
	if len(f.imageErrors) > 0 {
		f.imageErrors = f.imageErrors[1:]
	}
	if len(f.imageDetails) > 0 {
		detail := f.imageDetails[0]
		f.imageDetails = f.imageDetails[1:]
		f.recordImageSnapshot(detail)
		return detail, nil
	}
	f.recordImageSnapshot(f.imageDetail)
	return f.imageDetail, nil
}

func (f *fakeAPI) recordImageSnapshot(detail machineImageDetail) {
	state := "MISSING"
	if len(detail.Versions) > 0 {
		state = strings.ToUpper(blank(detail.Versions[0].SnapshotStatus, "UNKNOWN"))
	}
	f.actions = append(f.actions, "image:"+state)
	if state == "READY" {
		f.imageSnapshotReady = true
	}
}
func (f *fakeAPI) SaveImage(_ context.Context, vm, image string, _ map[string]string) error {
	f.savedImages = append(f.savedImages, [2]string{vm, image})
	f.actions = append(f.actions, "save")
	return f.saveErr
}
func (f *fakeAPI) RemoveImage(_ context.Context, image string) error {
	f.removedImage = append(f.removedImage, image)
	for i, candidate := range f.images {
		if candidate.Name == image {
			f.images = append(f.images[:i], f.images[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeAPI) RemoveImageVersion(_ context.Context, image string, version int) error {
	f.removedImage = append(f.removedImage, image+"@"+string(rune(version)))
	return nil
}

func setupState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("XDG_STATE_HOME", home+"/.state")
	keyRoot := filepath.Join(home, ".ssh")
	t.Setenv("SSH_KEY_PATH", keyRoot)
	if err := os.MkdirAll(keyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyRoot, "machine0__ci"), []byte("fixture private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return t.TempDir()
}

func readyMachine(ip string) machine {
	return machine{ID: "vm-123", Name: "crabbox-blue", Status: "RUNNING", IP: ip, URL: "https://crabbox-blue.mac0.io", Size: "large", Region: "eu", Image: "ubuntu-24-04-loaded", ImageVersion: 1, DefaultSSHUsername: "ubuntu", Distribution: "ubuntu", PricePerHour: 52_000, Key: &machineKey{Name: "ci"}}
}

func testBackendWithAPI(api *fakeAPI) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHKey = "/tmp/test-key"
	b := newBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	b.api = api
	b.sleep = func(context.Context, time.Duration) error { return nil }
	b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error { return nil }
	b.prepareNativeImageSource = func(context.Context, SSHTarget) error { return nil }
	return b
}

func testSize() machineSize {
	return machineSize{Size: "large", VCPU: 2, RAMGB: 4, DiskGB: 80, Regions: []string{"eu", "us-east"}, PricePerHourMicro: 52_000}
}

func TestNewBackendConfiguresClientReadRetryCadenceAndContextSleep(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "canonical default", want: 60 * time.Second},
		{name: "explicit override", configured: 7 * time.Second, want: 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Machine0.PollInterval = tc.configured
			b := newBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
			c, ok := b.api.(*client)
			if !ok || b.cfg.Machine0.PollInterval != tc.want || c.cfg.PollInterval != tc.want || c.sleep == nil {
				t.Fatalf("backend interval=%s client=%#v ok=%v want=%s", b.cfg.Machine0.PollInterval, c, ok, tc.want)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := c.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
				t.Fatalf("context sleep err=%v", err)
			}

			pending := readyMachine("")
			pending.Status = "CREATING"
			ready := readyMachine("203.0.113.10")
			b.api = &fakeAPI{getSequence: []machine{pending, ready}}
			var pollSleeps []time.Duration
			b.sleep = func(_ context.Context, delay time.Duration) error {
				pollSleeps = append(pollSleeps, delay)
				return nil
			}
			got, err := b.waitForRunning(context.Background(), pending.Name, time.Minute)
			if err != nil || got.ID != ready.ID || len(pollSleeps) != 1 || pollSleeps[0] != tc.want {
				t.Fatalf("machine=%#v err=%v poll sleeps=%v want=%s", got, err, pollSleeps, tc.want)
			}
		})
	}
}

func TestEffectiveMachine0WorkRootUsesResolvedSSHUser(t *testing.T) {
	base := core.BaseConfig()
	base.Machine0.WorkRoot = ""
	ubuntu := readyMachine("203.0.113.10")
	nixos := readyMachine("203.0.113.11")
	nixos.DefaultSSHUsername = ""
	nixos.Distribution = "nixos"

	for _, tc := range []struct {
		name     string
		cfg      Config
		item     machine
		wantUser string
		wantRoot string
	}{
		{name: "ubuntu default", cfg: base, item: ubuntu, wantUser: "ubuntu", wantRoot: "/home/ubuntu/crabbox"},
		{name: "nixos default", cfg: base, item: nixos, wantUser: "nix", wantRoot: "/home/nix/crabbox"},
		{name: "explicit override", cfg: func() Config { cfg := base; cfg.Machine0.WorkRoot = "/srv/machine0-work"; return cfg }(), item: nixos, wantUser: "nix", wantRoot: "/srv/machine0-work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveMachine0Config(tc.cfg, tc.item)
			if got.SSHUser != tc.wantUser || got.WorkRoot != tc.wantRoot || got.Machine0.WorkRoot != tc.wantRoot {
				t.Fatalf("user=%q workRoot=%q machine0.workRoot=%q", got.SSHUser, got.WorkRoot, got.Machine0.WorkRoot)
			}
		})
	}
}

func TestPrepareLeaseUsesDeterministicPrivateMachineKnownHosts(t *testing.T) {
	keyRoot := t.TempDir()
	t.Setenv("SSH_KEY_PATH", keyRoot)
	item := readyMachine("203.0.113.10")
	b := testBackendWithAPI(&fakeAPI{machine: item})

	first, err := b.prepareLease(context.Background(), item, Server{CloudID: item.ID}, "cbx_trust", false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := b.prepareLease(context.Background(), item, Server{CloudID: item.ID}, "cbx_trust", false)
	if err != nil {
		t.Fatal(err)
	}
	other := item
	other.ID = "vm-456"
	second, err := b.prepareLease(context.Background(), other, Server{CloudID: other.ID}, "cbx_other", false)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(keyRoot, "crabbox", providerName, "known_hosts.d")
	if first.SSH.KnownHostsFile == "" || first.SSH.KnownHostsFile != again.SSH.KnownHostsFile || filepath.Dir(first.SSH.KnownHostsFile) != wantDir {
		t.Fatalf("first=%q again=%q want dir=%q", first.SSH.KnownHostsFile, again.SSH.KnownHostsFile, wantDir)
	}
	if second.SSH.KnownHostsFile == first.SSH.KnownHostsFile {
		t.Fatalf("different immutable IDs shared host trust: %q", first.SSH.KnownHostsFile)
	}
	if first.SSH.DisableHostKeyChecking || first.SSH.HostKeyAlias != "" {
		t.Fatalf("unexpected host-key relaxation: %#v", first.SSH)
	}
	info, err := os.Stat(wantDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("known-hosts directory info=%#v err=%v", info, err)
	}
}

func TestMachine0HostTrustPathAndResetErrorsAreActionable(t *testing.T) {
	t.Run("create directory", func(t *testing.T) {
		rootFile := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(rootFile, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := machine0KnownHostsFile(rootFile, "vm-123")
		if err == nil || !strings.Contains(err.Error(), "create Machine0 SSH host trust directory") || !strings.Contains(err.Error(), rootFile) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("refuse directory reset", func(t *testing.T) {
		path, err := machine0KnownHostsFile(t.TempDir(), "vm-123")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := resetMachine0HostTrust(path); err == nil || !strings.Contains(err.Error(), "refusing to reset") || !strings.Contains(err.Error(), path) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestPrepareLeaseMachine0FilenameOverridesGenericSSHKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_KEY_PATH", "")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.WriteFile(want, []byte("test private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := readyMachine("203.0.113.10")
	item.Key = &machineKey{Name: "mac-studio-sf", FileName: "id_ed25519"}
	api := &fakeAPI{machine: item}
	b := testBackendWithAPI(api)
	b.cfg.SSHKey = "/tmp/unrelated-crabbox-key"
	var waited SSHTarget
	b.waitSSH = func(_ context.Context, target *SSHTarget, _ time.Duration) error {
		waited = *target
		return nil
	}
	lease, err := b.prepareLease(context.Background(), item, Server{CloudID: item.ID}, "cbx_keytest", true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Key != want || waited.Key != want {
		t.Fatalf("Machine0 key did not override generic key: lease=%q waited=%q want=%q", lease.SSH.Key, waited.Key, want)
	}
	if lease.SSH.HostKeyAlias != "" || waited.HostKeyAlias != "" {
		t.Fatalf("Machine0 must keep direct IP host-key handling rather than use an unseeded alias: lease=%q waited=%q", lease.SSH.HostKeyAlias, waited.HostKeyAlias)
	}
	if len(api.primed) != 0 {
		t.Fatalf("existing Machine0 key must skip PrimeSSH: %v", api.primed)
	}
}

func TestPrepareLeaseValidatesMachine0KeyFilenameBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		wantOK   bool
	}{
		{name: "safe basename", fileName: "id_ed25519", wantOK: true},
		{name: "safe basename with whitespace", fileName: "  id_ed25519  ", wantOK: true},
		{name: "parent traversal", fileName: "../other-key"},
		{name: "subdirectory", fileName: "subdir/key"},
		{name: "absolute POSIX path", fileName: "/tmp/other-key"},
		{name: "Windows separator", fileName: `subdir\key`},
		{name: "absolute Windows path", fileName: `C:\keys\other-key`},
		{name: "current directory", fileName: "."},
		{name: "parent directory", fileName: ".."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keyRoot := t.TempDir()
			t.Setenv("SSH_KEY_PATH", keyRoot)
			if tc.wantOK {
				if err := os.WriteFile(filepath.Join(keyRoot, "id_ed25519"), []byte("test private key"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			item := readyMachine("203.0.113.10")
			item.Key = &machineKey{Name: "ci", FileName: tc.fileName}
			api := &fakeAPI{machine: item}
			b := testBackendWithAPI(api)
			statCalls := 0
			b.stat = func(path string) (os.FileInfo, error) {
				statCalls++
				return os.Stat(path)
			}
			hostTrustCalls := 0
			b.knownHostsFile = func(root, machineID string) (string, error) {
				hostTrustCalls++
				return machine0KnownHostsFile(root, machineID)
			}
			sshCalls := 0
			b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error {
				sshCalls++
				return nil
			}

			lease, err := b.prepareLease(context.Background(), item, Server{CloudID: item.ID}, "cbx_key_validation", true)
			if tc.wantOK {
				if err != nil {
					t.Fatal(err)
				}
				wantKey := filepath.Join(keyRoot, "id_ed25519")
				if lease.SSH.Key != wantKey || statCalls != 1 || hostTrustCalls != 1 || sshCalls != 1 || len(api.primed) != 0 {
					t.Fatalf("key=%q stat=%d hostTrust=%d ssh=%d primed=%v", lease.SSH.Key, statCalls, hostTrustCalls, sshCalls, api.primed)
				}
				return
			}

			var exitErr core.ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "Machine0") || !strings.Contains(err.Error(), "key filename must be a single basename") {
				t.Fatalf("err=%v", err)
			}
			if statCalls != 0 || hostTrustCalls != 0 || sshCalls != 0 || len(api.primed) != 0 {
				t.Fatalf("unsafe filename reached side effects: stat=%d hostTrust=%d ssh=%d primed=%v", statCalls, hostTrustCalls, sshCalls, api.primed)
			}
		})
	}
}

func TestPrepareLeasePrimesMissingMachine0KeyAndVerifiesMaterialization(t *testing.T) {
	keyRoot := t.TempDir()
	t.Setenv("SSH_KEY_PATH", keyRoot)
	item := readyMachine("203.0.113.10")
	item.Key = &machineKey{Name: "mac-studio-sf", FileName: "id_ed25519"}
	keyPath := filepath.Join(keyRoot, "id_ed25519")
	api := &fakeAPI{machine: item}
	api.primeSSH = func(name string) error {
		if name != item.Name {
			t.Fatalf("prime name=%q", name)
		}
		return os.WriteFile(keyPath, []byte("materialized private key"), 0o600)
	}
	b := testBackendWithAPI(api)

	lease, err := b.prepareLease(context.Background(), item, Server{CloudID: item.ID}, "cbx_keymaterialize", true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Key != keyPath || len(api.primed) != 1 || api.primed[0] != item.Name {
		t.Fatalf("lease key=%q primed=%v", lease.SSH.Key, api.primed)
	}
}

func TestPrepareLeaseRejectsPrimeWithoutExpectedMachine0Key(t *testing.T) {
	keyRoot := t.TempDir()
	t.Setenv("SSH_KEY_PATH", keyRoot)
	item := readyMachine("203.0.113.10")
	item.Key = &machineKey{Name: "mac-studio-sf", FileName: "id_ed25519"}
	api := &fakeAPI{machine: item}
	b := testBackendWithAPI(api)

	_, err := b.prepareLease(context.Background(), item, Server{CloudID: item.ID}, "cbx_keymissing", true)
	wantPath := filepath.Join(keyRoot, "id_ed25519")
	if err == nil || !strings.Contains(err.Error(), wantPath) || !strings.Contains(err.Error(), "SSH_KEY_PATH") || !strings.Contains(err.Error(), "materialize") {
		t.Fatalf("err=%v", err)
	}
	if len(api.primed) != 1 || api.primed[0] != item.Name {
		t.Fatalf("PrimeSSH calls=%v", api.primed)
	}
}

func TestPrepareLeaseSelectsMachine0KeyOwnershipWithoutFilename(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      *machineKey
		wantFile string
	}{
		{name: "managed key name", key: &machineKey{Name: "ci-managed", Type: "MANAGED"}, wantFile: "machine0__ci-managed"},
		{name: "sparse managed key name", key: &machineKey{Name: "ci-managed"}, wantFile: "machine0__ci-managed"},
		{name: "public key cannot derive managed filename", key: &machineKey{Name: "ci-public", Type: "PUBLIC"}},
		{name: "missing provider key", key: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyRoot := t.TempDir()
			t.Setenv("SSH_KEY_PATH", keyRoot)
			item := readyMachine("203.0.113.10")
			item.Key = tc.key
			api := &fakeAPI{machine: item}
			b := testBackendWithAPI(api)
			want := "/tmp/fallback-crabbox-key"
			b.cfg.SSHKey = want
			if tc.wantFile != "" {
				want = filepath.Join(keyRoot, tc.wantFile)
				if err := os.WriteFile(want, []byte("fixture private key"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			lease, err := b.prepareLease(context.Background(), item, Server{CloudID: item.ID}, "cbx_keyfallback", true)
			if err != nil {
				t.Fatal(err)
			}
			if lease.SSH.Key != want {
				t.Fatalf("SSH key=%q want=%q", lease.SSH.Key, want)
			}
			if len(api.primed) != 0 {
				t.Fatalf("existing key must skip PrimeSSH: %v", api.primed)
			}
		})
	}
}

func TestAcquirePollsToRunningAndDefaultReleaseDestroys(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{{ID: "vm-123", Name: "crabbox-blue", Status: "CREATING"}, readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}, RequestedSlug: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 1 || api.created[0].Size != "large" || api.created[0].Region != "eu" || api.created[0].Image != "ubuntu-24-04-loaded" {
		t.Fatalf("created=%#v", api.created)
	}
	if lease.Server.CloudID != "vm-123" || lease.SSH.Host != "203.0.113.10" || lease.SSH.User != "ubuntu" {
		t.Fatalf("lease=%#v", lease)
	}
	if lease.Server.Labels["work_root"] != "/home/ubuntu/crabbox" {
		t.Fatalf("lease work_root=%q", lease.Server.Labels["work_root"])
	}
	claim, ok, err := resolveClaim(lease.LeaseID)
	if err != nil || !ok || claim.Labels["work_root"] != "/home/ubuntu/crabbox" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.removed) != 1 || len(api.suspended) != 0 {
		t.Fatalf("removed=%v suspended=%v", api.removed, api.suspended)
	}
}

func TestAcquirePreservesConfiguredNativeSizeAcrossClassChanges(t *testing.T) {
	repo := setupState(t)
	for _, size := range []string{"large", "xl-nvme", "gpu-h100-1", "future-native-size"} {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.Machine0.Size = size
		cfg.Machine0.SizeExplicit = true
		catalogSize := testSize()
		catalogSize.Size = size // Synthetic catalog entry; no static class mapping required.
		catalog, err := json.Marshal([]machineSize{catalogSize})
		if err != nil {
			t.Fatal(err)
		}
		for _, class := range core.CanonicalProviderClasses() {
			t.Run(size+"/"+class, func(t *testing.T) {
				cfg.Class = class
				core.MarkClassExplicit(&cfg)
				if err := (Provider{}).ApplyConfigDefaults(&cfg); err != nil {
					t.Fatal(err)
				}
				// Exercise the real client argv, but stop at the intercepted create command.
				runner := &recordingRunner{sequence: []runnerResponse{
					{result: core.LocalCommandResult{Stdout: `[]`}},
					{result: core.LocalCommandResult{Stdout: string(catalog)}},
					{result: core.LocalCommandResult{Stdout: `[]`}},
					{err: errors.New("create intercepted")},
				}}
				b, err := (Provider{}).Configure(cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
				if err != nil {
					t.Fatal(err)
				}
				_, err = b.(*backend).Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
				if err == nil || !strings.Contains(err.Error(), "create intercepted") {
					t.Fatalf("Acquire error=%v, want intercepted create", err)
				}
				if len(runner.calls) != 4 {
					t.Fatalf("calls=%#v", runner.calls)
				}
				args := runner.calls[3].Args
				if len(args) < 2 || args[0] != "new" {
					t.Fatalf("create args=%q", args)
				}
				want := []string{"--size", size, "--region", "eu", "--image", "ubuntu-24-04-loaded"}
				if !reflect.DeepEqual(args[2:], want) {
					t.Fatalf("create args=%q want selectors=%q", args, want)
				}
			})
		}
	}
}

func TestAcquireTerminalStateRollsBackWithDiagnostic(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{{ID: "vm-123", Name: "crabbox-blue", Status: "ERRORED", LastErrorMessage: "regional capacity unavailable"}}}
	b := testBackendWithAPI(api)
	_, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err == nil || !strings.Contains(err.Error(), "regional capacity unavailable") {
		t.Fatalf("err=%v", err)
	}
	if len(api.removed) != 1 {
		t.Fatalf("rollback removals=%v", api.removed)
	}
}

func TestAcquireRecoveryClaimTracksCreatedMachines(t *testing.T) {
	for _, tc := range []struct {
		name          string
		keep          bool
		removeErr     error
		ready         bool
		bindingFails  bool
		wrongSize     bool
		claimChanged  bool
		sshErr        error
		wantClaim     bool
		wantRecovery  bool
		wantRemovals  int
		stopRecovery  bool
		wantCloudID   string
		wantErrorPart string
	}{
		{name: "kept readiness failure remains discoverable and stoppable", keep: true, wantClaim: true, wantRecovery: true, stopRecovery: true},
		{name: "rollback failure retains claim", removeErr: errors.New("provider unavailable"), wantClaim: true, wantRecovery: true, wantRemovals: 1, wantErrorPart: "recovery claim"},
		{name: "changed pending claim prevents stale rollback", claimChanged: true, wantClaim: true, wantRecovery: true, wantErrorPart: "recovery claim"},
		{name: "successful rollback removes claim", wantRemovals: 1},
		{name: "failed ID binding rolls back pending claim", ready: true, bindingFails: true, wantRemovals: 1},
		{name: "success upgrades claim", keep: true, ready: true, wantClaim: true, wantCloudID: "vm-123"},
		{name: "substituted size rolls back", ready: true, wrongSize: true, wantRemovals: 1, wantErrorPart: "mismatched machine size"},
		{name: "kept substituted size remains stoppable", keep: true, ready: true, wrongSize: true, wantClaim: true, wantRecovery: true, stopRecovery: true, wantErrorPart: "mismatched machine size"},
		{name: "SSH failure retains ID-scoped recovery claim", keep: true, ready: true, sshErr: errors.New("SSH authentication failed"), wantClaim: true, wantRecovery: true, wantCloudID: "vm-123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupState(t)
			api := &fakeAPI{sizes: []machineSize{testSize()}, removeErr: tc.removeErr}
			var observed LeaseClaim
			api.getFn = func(_ context.Context, name string) (machine, error) {
				claims, err := core.ListLeaseClaims()
				if err != nil || len(claims) != 1 {
					t.Fatalf("claim was not durable before first readiness poll: claims=%#v err=%v", claims, err)
				}
				observed = claims[0]
				if observed.Labels["machine0_name"] != name || observed.ProviderScope != machine0NameScope(name) || observed.Labels["recovery"] != "create-pending" {
					t.Fatalf("pending recovery claim=%#v", observed)
				}
				if tc.claimChanged {
					replacement := observed
					replacement.Labels = make(map[string]string, len(observed.Labels)+1)
					for key, value := range observed.Labels {
						replacement.Labels[key] = value
					}
					replacement.Labels["replacement"] = "true"
					if err := core.ReplaceLeaseClaimIfUnchanged(observed.LeaseID, observed, replacement); err != nil {
						t.Fatal(err)
					}
				}
				item := readyMachine("203.0.113.10")
				item.Name = name
				if tc.bindingFails {
					item.ID = ""
				}
				if tc.wrongSize {
					item.Size = "medium"
				}
				api.machine = item
				if tc.ready {
					return item, nil
				}
				return machine{}, context.Canceled
			}
			b := testBackendWithAPI(api)
			if tc.ready && !tc.bindingFails && !tc.wrongSize {
				b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error {
					bound, ok, err := resolveClaim(observed.LeaseID)
					if err != nil || !ok || bound.CloudID != "vm-123" || bound.ProviderScope != machineScope("vm-123") {
						t.Fatalf("claim was not ID-scoped before SSH readiness: claim=%#v ok=%v err=%v", bound, ok, err)
					}
					return tc.sshErr
				}
			}
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}, RequestedSlug: "recovery", Keep: tc.keep})
			if tc.ready && !tc.bindingFails && !tc.wrongSize && tc.sshErr == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || tc.wantErrorPart != "" && !strings.Contains(err.Error(), tc.wantErrorPart) {
				t.Fatalf("err=%v", err)
			}
			if len(api.removed) != tc.wantRemovals {
				t.Fatalf("rollback removals=%v", api.removed)
			}
			claim, ok, claimErr := core.ReadLeaseClaimWithPresence(observed.LeaseID)
			if claimErr != nil || ok != tc.wantClaim {
				t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
			}
			if !ok {
				return
			}
			if (claim.Labels["recovery"] == "create-pending") != tc.wantRecovery || claim.CloudID != tc.wantCloudID {
				t.Fatalf("final claim=%#v lease=%#v", claim, lease)
			}
			if tc.stopRecovery {
				views, listErr := b.List(context.Background(), ListRequest{})
				if listErr != nil || len(views) != 1 {
					t.Fatalf("recovery list=%#v err=%v", views, listErr)
				}
				api.getFn = func(context.Context, string) (machine, error) {
					t.Fatal("pending recovery stop must remove the exact name without adopting an inventory machine")
					return machine{}, nil
				}
				if releaseErr := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: claim.LeaseID}}); releaseErr != nil {
					t.Fatalf("recovery stop: %v", releaseErr)
				}
				if _, remains, resolveErr := core.ReadLeaseClaimWithPresence(claim.LeaseID); resolveErr != nil || remains {
					t.Fatalf("released recovery claim remains=%v err=%v", remains, resolveErr)
				}
			}
		})
	}
}

func TestAcquirePreflightsPublicSSHKeyBeforeCreate(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Fatal("ssh-keygen is required for the private-file identity boundary test")
	}
	makeKey := func() ([]byte, string, ssh.Signer) {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		block, err := ssh.MarshalPrivateKey(private, "")
		if err != nil {
			t.Fatal(err)
		}
		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(block), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), signer
	}
	private, public, signer := makeKey()
	_, otherPublic, _ := makeKey()
	raw, err := ssh.ParseRawPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(raw, "", []byte("synthetic-test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: signer.PublicKey(), CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	publicKey := func(value string) machineKey {
		return machineKey{Name: "selected-key", Type: "PUBLIC", FileName: "local-key", PublicKey: value}
	}
	for _, tc := range []struct {
		name       string
		key        machineKey
		private    []byte
		sidecar    string
		wantError  bool
		mismatch   bool
		extractErr error
		cancel     bool
	}{
		{name: "public key without filename", key: machineKey{Name: "no-filename", Type: "PUBLIC"}, wantError: true},
		{name: "public key with unsafe filename", key: machineKey{Name: "unsafe-filename", Type: "PUBLIC", FileName: "../other-key"}, wantError: true},
		{name: "public key without local private key", key: publicKey(public), sidecar: public, wantError: true},
		{name: "matching private key without sidecar", key: publicKey(public), private: private},
		{name: "mismatching private key without sidecar", key: publicKey(otherPublic), private: private, wantError: true, mismatch: true},
		{name: "matching private key with misleading sidecar", key: publicKey(public), private: private, sidecar: otherPublic},
		{name: "mismatching private key with matching sidecar", key: publicKey(otherPublic), private: private, sidecar: otherPublic, wantError: true, mismatch: true},
		{name: "public key comments are not identity", key: publicKey(public + " registered comment"), private: private},
		{name: "encrypted private key remains unknown", key: publicKey(otherPublic), private: pem.EncodeToMemory(encrypted)},
		{name: "unsupported private key remains unknown", key: publicKey(otherPublic), private: []byte("unsupported private key format")},
		{name: "missing public metadata remains unknown", key: publicKey(""), private: private},
		{name: "invalid public metadata remains unknown", key: publicKey("not a public key"), private: private},
		{name: "certificate metadata is not a bare key", key: publicKey(string(ssh.MarshalAuthorizedKey(cert))), private: private},
		{name: "multiple public keys remain unknown", key: publicKey(otherPublic + "\n" + public), private: private},
		{name: "authorized key options remain unknown", key: publicKey("restrict " + otherPublic), private: private},
		{name: "empty option prefix remains unknown", key: publicKey(", " + otherPublic), private: private},
		{name: "mismatched key type remains unknown", key: publicKey(strings.Replace(otherPublic, "ssh-ed25519", "ssh-rsa", 1)), private: private},
		{name: "extraction unavailable remains unknown", key: publicKey(otherPublic), private: private, extractErr: exec.ErrNotFound},
		{name: "cancellation during extraction prevents create", key: publicKey(public), private: private, cancel: true, wantError: true},
		{name: "managed key can materialize later", key: machineKey{Name: "managed-key", Type: "MANAGED", FileName: "machine0__managed-key"}},
	} {
		for _, leaseID := range []string{"", fixedMachine0TestLeaseID} {
			t.Run(tc.name+"/"+blank(leaseID, "ordinary"), func(t *testing.T) {
				repo := setupState(t)
				keyPath := filepath.Join(os.Getenv("SSH_KEY_PATH"), tc.key.FileName)
				if tc.private != nil {
					if err := os.WriteFile(keyPath, tc.private, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if tc.sidecar != "" {
					if err := os.WriteFile(keyPath+".pub", []byte(tc.sidecar), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("key extraction canceled")
				runner := &recordingRunner{run: func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					if req.Name != "ssh-keygen" || !reflect.DeepEqual(req.Args, []string{"-y", "-P", "", "-f", keyPath}) || req.MaxCapturedOutputBytes <= 0 || req.Stdout != nil || req.Stderr != nil {
						t.Fatal("expected bounded, captured, noninteractive extraction from the exact private file")
					}
					if tc.cancel {
						cancel(cause)
						return core.LocalCommandResult{ExitCode: 1, Stderr: "sensitive diagnostic"}, context.Canceled
					}
					if tc.extractErr != nil {
						return core.LocalCommandResult{ExitCode: 1, Stderr: "sensitive diagnostic"}, tc.extractErr
					}
					output, err := exec.CommandContext(ctx, req.Name, req.Args...).Output()
					result := core.LocalCommandResult{Stdout: string(output)}
					if err != nil {
						result.ExitCode = 1
					}
					return result, err
				}}
				api := &fakeAPI{sizes: []machineSize{testSize()}, selectedKey: &tc.key, getSequence: []machine{readyMachine("203.0.113.10")}}
				b := testBackendWithAPI(api)
				b.rt.Exec = runner
				var diagnostics bytes.Buffer
				b.rt.Stdout, b.rt.Stderr = &diagnostics, &diagnostics
				waited := 0
				b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error { waited++; return nil }
				_, err := b.Acquire(ctx, AcquireRequest{RequestedLeaseID: leaseID, Repo: core.Repo{Root: repo}})
				for _, material := range []string{public, otherPublic, string(private), "sensitive diagnostic"} {
					if strings.Contains(diagnostics.String(), material) {
						t.Fatal("preflight logged key material or raw extraction diagnostics")
					}
				}
				if tc.private != nil {
					got, readErr := os.ReadFile(keyPath)
					if readErr != nil || !bytes.Equal(got, tc.private) {
						t.Fatal("preflight changed the private file")
					}
				}
				if tc.mismatch && len(runner.calls) != 1 {
					t.Fatal("mismatch must be established by private-file extraction")
				}
				if tc.wantError {
					if err == nil || len(api.created) != 0 || len(api.started) != 0 || len(api.primed) != 0 || len(api.removed) != 0 || waited != 0 {
						t.Fatalf("preflight must fail before allocation or readiness: err=%v creates=%d starts=%d primes=%d removals=%d waits=%d", err, len(api.created), len(api.started), len(api.primed), len(api.removed), waited)
					}
					if tc.cancel {
						if !errors.Is(err, cause) {
							t.Fatalf("cancellation cause lost: %v", err)
						}
					} else if tc.mismatch {
						var exitErr core.ExitError
						if !errors.As(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), "--machine0-key") {
							t.Fatalf("expected actionable key mismatch: %v", err)
						}
						for _, secret := range []string{tc.key.Name, keyPath, public, otherPublic, string(private), "sensitive diagnostic"} {
							if strings.Contains(err.Error(), secret) {
								t.Fatal("mismatch error disclosed key identity or material")
							}
						}
					} else if !strings.Contains(err.Error(), tc.key.Name) || !strings.Contains(err.Error(), "--machine0-key <managed-key-name>") {
						t.Fatalf("existing filename/private-file guard lost: %v", err)
					}
					if leaseID != "" {
						claim := readFixedMachine0Claim(t, leaseID)
						if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != fixedMachine0IntentPrepared || len(claim.FixedCreateIntent.Attempt) != 0 || claim.CloudID != "" {
							t.Fatal("preflight failure persisted a create attempt or resource")
						}
					}
					return
				}
				if err != nil || len(api.created) != 1 || waited != 1 {
					t.Fatalf("err=%v creates=%d waits=%d", err, len(api.created), waited)
				}
				if tc.key.Type == "MANAGED" && len(runner.calls) != 0 {
					t.Fatal("managed key reached PUBLIC key extraction")
				}
			})
		}
	}
}

func TestAcquirePublicSSHKeyFileKinds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX special files and symlinks")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"fifo", "symlink fifo", "device", "symlink regular"} {
		for _, leaseID := range []string{"", fixedMachine0TestLeaseID} {
			t.Run(kind+"/"+blank(leaseID, "ordinary"), func(t *testing.T) {
				repo := setupState(t)
				keyPath := filepath.Join(os.Getenv("SSH_KEY_PATH"), "local-key")
				target := keyPath + "-target"
				switch kind {
				case "fifo", "symlink fifo":
					if output, err := exec.Command("mkfifo", "-m", "600", target).CombinedOutput(); err != nil {
						t.Fatalf("mkfifo: %v: %s", err, output)
					}
				case "device":
					target = os.DevNull
				case "symlink regular":
					if err := os.WriteFile(target, pem.EncodeToMemory(block), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "fifo" {
					err = os.Rename(target, keyPath)
				} else {
					err = os.Symlink(target, keyPath)
				}
				if err != nil {
					t.Fatal(err)
				}
				api := &fakeAPI{sizes: []machineSize{testSize()}, selectedKey: &machineKey{
					Name: "selected-key", Type: "PUBLIC", FileName: "local-key",
					PublicKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey())),
				}, getSequence: []machine{readyMachine("203.0.113.10")}}
				b := testBackendWithAPI(api)
				runner := &recordingRunner{run: func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					output, err := exec.CommandContext(ctx, req.Name, req.Args...).Output()
					return core.LocalCommandResult{Stdout: string(output)}, err
				}}
				b.rt.Exec = runner
				// Bound the regression: ssh-keygen blocks opening an unwritten FIFO.
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				_, err := b.Acquire(ctx, AcquireRequest{RequestedLeaseID: leaseID, Repo: core.Repo{Root: repo}})
				wantExtractions := 0
				if kind == "symlink regular" {
					wantExtractions = 1
				}
				if err != nil || len(api.created) != 1 || len(runner.calls) != wantExtractions {
					t.Fatalf("err=%v creates=%d extractions=%d, want nil/1/%d", err, len(api.created), len(runner.calls), wantExtractions)
				}
			})
		}
	}
}

func TestPendingRecoveryReleaseRejectsInvalidOwnershipAndSuspend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*backend, *LeaseClaim)
	}{
		{name: "mismatched name scope", mutate: func(_ *backend, claim *LeaseClaim) { claim.ProviderScope = machine0NameScope("another-machine") }},
		{name: "mismatched machine name", mutate: func(_ *backend, claim *LeaseClaim) { claim.Labels["machine0_name"] = "another-machine" }},
		{name: "pending claim cannot suspend", mutate: func(b *backend, _ *LeaseClaim) { b.cfg.Machine0.ReleasePolicy = "suspend" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupState(t)
			api := &fakeAPI{sizes: []machineSize{testSize()}}
			api.getFn = func(_ context.Context, name string) (machine, error) {
				item := readyMachine("203.0.113.10")
				item.Name = name
				api.machine = item
				return machine{}, context.Canceled
			}
			b := testBackendWithAPI(api)
			if _, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}, RequestedSlug: "pending", Keep: true}); err == nil {
				t.Fatal("expected interrupted acquisition")
			}
			claims, err := core.ListLeaseClaims()
			if err != nil || len(claims) != 1 {
				t.Fatalf("claims=%#v err=%v", claims, err)
			}
			claim, replacement := claims[0], claims[0]
			replacement.Labels = make(map[string]string, len(claim.Labels))
			for key, value := range claim.Labels {
				replacement.Labels[key] = value
			}
			tc.mutate(b, &replacement)
			if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, replacement); err != nil {
				t.Fatal(err)
			}
			err = b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: claim.LeaseID}})
			if err == nil || len(api.removed) != 0 {
				t.Fatalf("pending release err=%v removals=%v", err, api.removed)
			}
			if _, ok, resolveErr := resolveClaim(claim.LeaseID); resolveErr != nil || !ok {
				t.Fatalf("recovery claim not retained: ok=%v err=%v", ok, resolveErr)
			}
		})
	}
}

func TestAcquireDoesNotStartUnexpectedStoppedMachine(t *testing.T) {
	repo := setupState(t)
	stopped := readyMachine("")
	stopped.Status = "STOPPED"
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{stopped}}
	b := testBackendWithAPI(api)
	_, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err == nil || !strings.Contains(err.Error(), "stable state STOPPED") {
		t.Fatalf("err=%v", err)
	}
	if len(api.started) != 0 || len(api.removed) != 1 {
		t.Fatalf("acquire started or failed to roll back stopped machine: started=%v removed=%v", api.started, api.removed)
	}
}

func TestResolveUnclaimedIdentifier(t *testing.T) {
	const leaseID = "cbx_abcdef123456"
	matched := readyMachine("203.0.113.10")
	matched.Name = "crabbox-unknown-slug-c80c2195"
	other := readyMachine("203.0.113.11")
	other.ID, other.Name = "vm-other", "crabbox-other-00000000"
	ambiguous := other
	ambiguous.Name = "crabbox-another-slug-c80c2195"
	missingClaim := "machine0 lease " + leaseID + " has no local claim; candidate \"" + matched.Name + "\" matches only a short name hash: inspect the machine and use its explicit name with --reclaim to adopt it"
	for _, tc := range []struct {
		name      string
		id        string
		machines  []machine
		listErr   error
		wantGet   string
		wantLists int
		wantErr   string
	}{
		{name: "canonical lease ID", id: leaseID, machines: []machine{other, matched}, wantErr: missingClaim, wantLists: 1},
		{name: "trimmed canonical lease ID", id: " " + leaseID + " ", machines: []machine{matched}, wantErr: missingClaim, wantLists: 1},
		{name: "missing lease", id: leaseID, machines: []machine{other}, wantLists: 1, wantErr: "lease/server not found: " + leaseID},
		{name: "empty inventory", id: leaseID, machines: []machine{}, wantLists: 1, wantErr: "lease/server not found: " + leaseID},
		{name: "name must have Crabbox prefix and exact suffix", id: leaseID, machines: []machine{
			{ID: "vm-unmanaged", Name: "other-blue-c80c2195", Status: "RUNNING"},
			{ID: "vm-no-separator", Name: "crabbox-bluec80c2195", Status: "RUNNING"},
			{ID: "vm-extra-suffix", Name: "crabbox-blue-c80c2195-extra", Status: "RUNNING"},
		}, wantLists: 1, wantErr: "lease/server not found: " + leaseID},
		{name: "slug", id: "blue-lobster", wantGet: "blue-lobster"},
		{name: "machine name", id: matched.Name, wantGet: matched.Name},
		{name: "noncanonical ID", id: "cbx_notcanonical", wantGet: "cbx_notcanonical"},
		{name: "ambiguous lease", id: leaseID, machines: []machine{matched, ambiguous}, wantLists: 1, wantErr: "multiple Machine0 machines match lease " + leaseID},
		{name: "list failure", id: leaseID, listErr: context.Canceled, wantLists: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupState(t)
			getCalls := 0
			api := &fakeAPI{machines: tc.machines, getFn: func(_ context.Context, name string) (machine, error) {
				getCalls++
				if tc.wantGet == "" || name != tc.wantGet {
					t.Fatalf("unexpected Get(%q), want %q", name, tc.wantGet)
				}
				return matched, nil
			}}
			if tc.listErr != nil {
				api.listFn = func(context.Context, int) ([]machine, error) { return nil, tc.listErr }
			}
			lease, err := testBackendWithAPI(api).Resolve(context.Background(), ResolveRequest{ID: tc.id, StatusOnly: true})
			switch {
			case tc.listErr != nil:
				if !errors.Is(err, tc.listErr) {
					t.Fatalf("err=%v, want %v", err, tc.listErr)
				}
			case tc.wantErr != "":
				var exitErr core.ExitError
				if !errors.As(err, &exitErr) || exitErr.Code != 4 || err.Error() != tc.wantErr {
					t.Fatalf("err=%v, want exit 4: %s", err, tc.wantErr)
				}
			default:
				if err != nil || lease.Server.CloudID != matched.ID || lease.Server.Name != matched.Name {
					t.Fatalf("server=%#v err=%v", lease.Server, err)
				}
			}
			wantGets := 0
			if tc.wantGet != "" {
				wantGets = 1
			}
			if getCalls != wantGets || api.listCalls != tc.wantLists {
				t.Fatalf("Get calls=%d List calls=%d, want %d and %d", getCalls, api.listCalls, wantGets, tc.wantLists)
			}
		})
	}
}

func TestResolveUnclaimedMachineNameUsesDetail(t *testing.T) {
	for _, mode := range []string{"status", "existing managed key", "materialize managed key", "generic key"} {
		t.Run(mode, func(t *testing.T) {
			setupState(t)
			inventory := readyMachine("203.0.113.10")
			inventory.Name, inventory.Key = "crabbox-blue-c80c2195", nil
			detail := inventory
			detail.IP, detail.DefaultSSHUsername, detail.Distribution = "203.0.113.99", "nix", "nixos"
			keyPath := filepath.Join(os.Getenv("SSH_KEY_PATH"), "machine0__selected")
			if mode != "generic key" {
				detail.Key = &machineKey{Name: "selected", FileName: "machine0__selected"}
			}
			if mode == "existing managed key" {
				if err := os.WriteFile(keyPath, []byte("fixture private key"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			gets, waits := 0, 0
			api := &fakeAPI{machines: []machine{inventory}, getFn: func(_ context.Context, name string) (machine, error) {
				gets++
				if name != inventory.Name {
					t.Fatalf("Get(%q), want inventory name %q", name, inventory.Name)
				}
				return detail, nil
			}}
			api.primeSSH = func(name string) error {
				if mode != "materialize managed key" || name != detail.Name {
					t.Fatalf("unexpected key priming for %q", name)
				}
				return os.WriteFile(keyPath, []byte("fixture private key"), 0o600)
			}
			b := testBackendWithAPI(api)
			if mode == "generic key" {
				keyPath = b.cfg.SSHKey
			}
			b.waitSSH = func(_ context.Context, target *SSHTarget, _ time.Duration) error {
				waits++
				if target.Key != keyPath || target.Host != detail.IP || target.User != "nix" {
					t.Fatalf("readiness target=%#v", target)
				}
				return nil
			}
			ready := mode != "status"
			lease, err := b.Resolve(context.Background(), ResolveRequest{ID: inventory.Name, StatusOnly: true, ReadyProbe: ready})
			if err != nil || gets != 1 || api.listCalls != 0 || lease.Server.PublicNet.IPv4.IP != detail.IP || lease.Server.Labels["work_root"] != "/home/nix/crabbox" {
				t.Fatalf("lease=%#v gets=%d lists=%d err=%v", lease, gets, api.listCalls, err)
			}
			if ready && (waits != 1 || lease.SSH.Key != keyPath) || !ready && waits != 0 {
				t.Fatalf("waits=%d SSH=%#v", waits, lease.SSH)
			}
			wantPrimes := 0
			if mode == "materialize managed key" {
				wantPrimes = 1
			}
			if len(api.primed) != wantPrimes {
				t.Fatalf("key priming=%v", api.primed)
			}
			claims, err := core.ListLeaseClaims()
			if err != nil || len(claims) != 0 {
				t.Fatalf("status lookup published claims=%#v err=%v", claims, err)
			}
		})
	}
}

func TestResolveUnclaimedLeaseHashCollisionFailsClosed(t *testing.T) {
	const firstID, secondID = "cbx_f17568b85ee8", "cbx_f3dbca2ff7ac"
	if machine0LeaseSuffix(firstID) != machine0LeaseSuffix(secondID) {
		t.Fatal("fixture lease IDs must have colliding name hashes")
	}
	for _, id := range []string{firstID, secondID} {
		t.Run(id, func(t *testing.T) {
			repo := setupState(t)
			inventory := readyMachine("203.0.113.10")
			inventory.Name = "crabbox-blue" + machine0LeaseSuffix(firstID)
			gets := 0
			api := &fakeAPI{machines: []machine{inventory}, getFn: func(context.Context, string) (machine, error) {
				gets++
				return inventory, nil
			}}
			b := testBackendWithAPI(api)
			b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error {
				t.Fatal("readiness ran on a hash-only candidate")
				return nil
			}
			_, err := b.Resolve(context.Background(), ResolveRequest{ID: id, Reclaim: true, ReadyProbe: true, Repo: core.Repo{Root: repo}})
			var exitErr core.ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 4 || !strings.Contains(err.Error(), "matches only a short name hash") {
				t.Fatalf("unexpected error=%v", err)
			}
			if gets != 0 || len(api.created)+len(api.started)+len(api.stopped)+len(api.suspended)+len(api.removed)+len(api.primed) != 0 {
				t.Fatalf("gets=%d mutations=%#v", gets, api)
			}
			claims, err := core.ListLeaseClaims()
			if err != nil || len(claims) != 0 {
				t.Fatalf("unverified detail published claims=%#v err=%v", claims, err)
			}
		})
	}
}

func TestResolveRefreshesChangedIPAndPrefersReturnedUsername(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	api.machine.IP = "203.0.113.99"
	api.machine.DefaultSSHUsername = "nix"
	api.machine.Distribution = "nixos"
	b.cfg.Class = "beast"
	core.MarkClassExplicit(&b.cfg)
	b.cfg.Machine0.Size, b.cfg.Machine0.SizeExplicit = "gpu-h100-1", true
	b.cfg.Machine0.Region, b.cfg.Machine0.Image, b.cfg.Machine0.Key = "us-east", "other-image", "other-key"
	api.listCalls = 0
	api.getFn = func(_ context.Context, name string) (machine, error) {
		if name != lease.Server.Name {
			t.Fatalf("claimed Get(%q), want %q", name, lease.Server.Name)
		}
		return api.machine, nil
	}
	resolved, err := b.Resolve(context.Background(), ResolveRequest{Repo: core.Repo{Root: repo}, ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if api.listCalls != 0 {
		t.Fatalf("claimed resolve called List %d times", api.listCalls)
	}
	if resolved.SSH.Host != "203.0.113.99" || resolved.SSH.User != "nix" || resolved.Server.CloudID != "vm-123" {
		t.Fatalf("resolved=%#v", resolved)
	}
	if resolved.Server.Labels["work_root"] != "/home/nix/crabbox" {
		t.Fatalf("resolved work_root=%q", resolved.Server.Labels["work_root"])
	}
	if resolved.Server.ServerType.Name != "large" || resolved.Server.Labels["region"] != "eu" || resolved.SSH.Key != lease.SSH.Key {
		t.Fatalf("creation selectors changed the observed machine: %#v", resolved)
	}
	if len(api.created) != 1 || len(api.removed)+len(api.started)+len(api.stopped)+len(api.suspended) != 0 {
		t.Fatalf("reuse mutated lifecycle: created=%v removed=%v started=%v stopped=%v suspended=%v", api.created, api.removed, api.started, api.stopped, api.suspended)
	}
	claim, ok, err := resolveClaim(lease.LeaseID)
	if err != nil || !ok || claim.Labels["work_root"] != "/home/nix/crabbox" {
		t.Fatalf("resolved claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolveRunningMachinePreservesIsolatedHostTrust(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	const staleTrust = "203.0.113.10 ssh-ed25519 stale-but-continuous\n"
	if err := os.WriteFile(lease.SSH.KnownHostsFile, []byte(staleTrust), 0o600); err != nil {
		t.Fatal(err)
	}
	api.machine = readyMachine("203.0.113.10")

	resolved, err := b.Resolve(context.Background(), ResolveRequest{Repo: core.Repo{Root: repo}, ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(resolved.SSH.KnownHostsFile)
	if err != nil || string(got) != staleTrust || resolved.SSH.KnownHostsFile != lease.SSH.KnownHostsFile {
		t.Fatalf("known hosts=%q path=%q original=%q err=%v", got, resolved.SSH.KnownHostsFile, lease.SSH.KnownHostsFile, err)
	}
}

func TestAcquirePreservesExplicitMachine0WorkRoot(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	b.cfg.Machine0.WorkRoot = "/srv/explicit-machine0"
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["work_root"] != "/srv/explicit-machine0" {
		t.Fatalf("lease work_root=%q", lease.Server.Labels["work_root"])
	}
	claim, ok, err := resolveClaim(lease.LeaseID)
	if err != nil || !ok || claim.Labels["work_root"] != "/srv/explicit-machine0" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestSuspendResumePreservesMachineIDAndRefreshesChangedIP(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	b.cfg.Machine0.ReleasePolicy = "suspend"
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "vm-123" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("initial identity/endpoint=%#v", lease)
	}
	suspending := readyMachine("203.0.113.10")
	suspending.Status = "SUSPENDING"
	suspended := readyMachine("")
	suspended.Status = "SUSPENDED"
	api.getSequence = []machine{readyMachine("203.0.113.10"), suspending, suspended}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.suspended) != 1 || len(api.removed) != 0 || !b.RetainLeaseClaimAfterRelease(lease) {
		t.Fatalf("suspended=%v removed=%v", api.suspended, api.removed)
	}
	if err := os.WriteFile(lease.SSH.KnownHostsFile, []byte("stale suspended host key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api.getSequence = []machine{func() machine { m := readyMachine("203.0.113.77"); m.Status = "STARTING"; return m }(), readyMachine("203.0.113.77")}
	if err := b.Resume(context.Background(), ResumeRequest{ID: lease.LeaseID}); err != nil {
		t.Fatal(err)
	}
	if len(api.started) != 1 {
		t.Fatalf("started=%v", api.started)
	}
	if _, err := os.Stat(lease.SSH.KnownHostsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resume did not reset isolated host trust: %v", err)
	}
	resolved, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, StatusOnly: true, ReadyProbe: true})
	if err != nil || resolved.Server.CloudID != "vm-123" || resolved.SSH.Host != "203.0.113.77" || resolved.SSH.Host == lease.SSH.Host {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
}

func TestPauseAndCleanupWaitForExactSuspendedState(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		repo := setupState(t)
		api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
		b := testBackendWithAPI(api)
		lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
		if err != nil {
			t.Fatal(err)
		}
		suspending := readyMachine("203.0.113.10")
		suspending.Status = "SUSPENDING"
		suspended := readyMachine("")
		suspended.Status = "SUSPENDED"
		api.getSequence = []machine{readyMachine("203.0.113.10"), suspending, suspended}
		if err := b.Pause(context.Background(), PauseRequest{ID: lease.LeaseID}); err != nil {
			t.Fatal(err)
		}
		claim, ok, err := resolveClaim(lease.LeaseID)
		if err != nil || !ok {
			t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
		}
		if claim.Labels["machine0_status"] != "SUSPENDED" || claim.SSHHost != "" || len(api.suspended) != 1 {
			t.Fatalf("pause persisted before exact suspension: claim=%#v suspended=%v", claim, api.suspended)
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		repo := setupState(t)
		api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
		b := testBackendWithAPI(api)
		b.cfg.Machine0.ReleasePolicy = "suspend"
		lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
		if err != nil {
			t.Fatal(err)
		}
		api.machine.Status = "STOPPED"
		suspending := readyMachine("203.0.113.10")
		suspending.Status = "SUSPENDING"
		suspended := readyMachine("")
		suspended.Status = "SUSPENDED"
		api.getSequence = []machine{suspending, suspended}
		if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
			t.Fatal(err)
		}
		claim, ok, err := resolveClaim(lease.LeaseID)
		if err != nil || !ok || claim.Labels["machine0_status"] != "SUSPENDED" || claim.SSHHost != "" {
			t.Fatalf("cleanup claim=%#v ok=%v err=%v", claim, ok, err)
		}
	})
}

func TestSuspendFailureDoesNotClearLiveClaimEndpoint(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	failed := readyMachine("203.0.113.10")
	failed.Status = "ERRORED"
	failed.LastErrorMessage = "snapshot failed"
	api.getSequence = []machine{readyMachine("203.0.113.10"), failed}
	if err := b.Pause(context.Background(), PauseRequest{ID: lease.LeaseID}); err == nil || !strings.Contains(err.Error(), "snapshot failed") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, err := resolveClaim(lease.LeaseID)
	if err != nil || !ok || claim.SSHHost != "203.0.113.10" || claim.Labels["machine0_status"] != "RUNNING" {
		t.Fatalf("failed suspend changed claim: claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestWaitForSuspendedHonorsContextAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ctx     func() context.Context
		timeout time.Duration
		want    string
	}{
		{name: "canceled", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }, timeout: time.Minute, want: "context canceled"},
		{name: "timeout", ctx: context.Background, timeout: 0, want: "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := readyMachine("203.0.113.10")
			item.Status = "SUSPENDING"
			b := testBackendWithAPI(&fakeAPI{machine: item})
			b.sleep = sleepContext
			_, err := b.waitForSuspended(tc.ctx(), item.Name, tc.timeout)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestMachineWaitPreservesTerminalStateAtDeadline(t *testing.T) {
	failed := readyMachine("")
	failed.Status = "ERRORED"
	failed.LastErrorMessage = "late provider failure"
	api := &fakeAPI{getFn: func(ctx context.Context, _ string) (machine, error) {
		<-ctx.Done()
		return failed, nil
	}}
	b := testBackendWithAPI(api)
	_, err := b.waitForRunning(context.Background(), failed.Name, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "late provider failure") || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
}

func TestMachineWaitPreservesClientDeadline(t *testing.T) {
	api := &fakeAPI{getFn: func(context.Context, string) (machine, error) {
		return machine{}, context.DeadlineExceeded
	}}
	b := testBackendWithAPI(api)
	_, err := b.waitForRunning(context.Background(), "late-machine", time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveStartsOnceAfterStoppingTransition(t *testing.T) {
	for _, transition := range []struct {
		name   string
		states []string
	}{
		{name: "suspending", states: []string{"SUSPENDING", "SUSPENDED", "STARTING", "RUNNING"}},
		{name: "stopping", states: []string{"STOPPING", "STOPPED", "STARTING", "RUNNING"}},
	} {
		t.Run(transition.name, func(t *testing.T) {
			repo := setupState(t)
			api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
			b := testBackendWithAPI(api)
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			if err != nil {
				t.Fatal(err)
			}
			api.getSequence = nil
			for _, state := range transition.states {
				item := readyMachine("203.0.113.77")
				item.Status = state
				if state != "RUNNING" {
					item.IP = ""
				}
				api.getSequence = append(api.getSequence, item)
			}
			resolved, err := b.Resolve(context.Background(), ResolveRequest{Repo: core.Repo{Root: repo}, ID: lease.LeaseID})
			if err != nil {
				t.Fatal(err)
			}
			if len(api.started) != 1 || resolved.SSH.Host != "203.0.113.77" {
				t.Fatalf("started=%v resolved=%#v", api.started, resolved)
			}
		})
	}
}

func TestMachine0MachineNameIsDeterministicUniqueAndWithinProviderLimit(t *testing.T) {
	const leaseID = "cbx_abcdef123456"
	if got := machine0MachineName(leaseID, "blue-lobster"); got != "crabbox-blue-lobster-c80c2195" {
		t.Fatalf("short name=%q", got)
	}
	long := machine0MachineName(leaseID, "this-is-a-very-long-requested-slug")
	if long != "crabbox-this-is-a-very-c80c2195" || len(long) != machine0MachineNameMaxLength {
		t.Fatalf("long name=%q len=%d", long, len(long))
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`).MatchString(long) {
		t.Fatalf("name violates Machine0 contract: %q", long)
	}
	if again := machine0MachineName(leaseID, "this-is-a-very-long-requested-slug"); again != long {
		t.Fatalf("name is not deterministic: first=%q second=%q", long, again)
	}
	if other := machine0MachineName("cbx_abcdef123457", "this-is-a-very-long-requested-slug"); other == long {
		t.Fatalf("different leases collided: %q", other)
	}
}

func TestCatalogValidationUsesLiveRegions(t *testing.T) {
	api := &fakeAPI{sizes: []machineSize{testSize()}}
	b := testBackendWithAPI(api)
	if err := b.validateCatalogSelection(context.Background(), "large", "asia"); err == nil || !strings.Contains(err.Error(), "available regions: eu,us-east") {
		t.Fatalf("err=%v", err)
	}
	if err := b.validateCatalogSelection(context.Background(), "missing", "eu"); err == nil || !strings.Contains(err.Error(), "live catalog") {
		t.Fatalf("err=%v", err)
	}
}

func TestDoctorChecksCLIAuthInventoryAndCatalogWithoutMutation(t *testing.T) {
	api := &fakeAPI{machine: readyMachine("203.0.113.10"), sizes: []machineSize{testSize()}}
	b := testBackendWithAPI(api)
	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cli=ready", "auth=ready", "inventory=ready", "mutation=false", "leases=1", "sizes=1", "1.0.155"} {
		if !strings.Contains(result.Message, want) {
			t.Fatalf("doctor message %q missing %q", result.Message, want)
		}
	}
	if len(api.created) != 0 || len(api.removed) != 0 || len(api.suspended) != 0 {
		t.Fatalf("doctor mutated provider: %#v", api)
	}
}

func TestDoctorRunsSlowProviderProbesWithinSharedBudget(t *testing.T) {
	api := &fakeAPI{
		machine:     readyMachine("203.0.113.10"),
		sizes:       []machineSize{testSize()},
		doctorDelay: 4 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	result, err := testBackendWithAPI(api).Doctor(ctx, DoctorRequest{})
	if err != nil {
		t.Fatalf("doctor failed within shared budget: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 8*time.Second {
		t.Fatalf("doctor probes ran sequentially: elapsed=%s", elapsed)
	}
	if !strings.Contains(result.Message, "leases=1") || !strings.Contains(result.Message, "sizes=1") {
		t.Fatalf("doctor result=%#v", result)
	}
}

func TestDoctorProbeFailureCancelsSiblingProbes(t *testing.T) {
	wantErr := errors.New("machine0 version probe failed")
	api := &fakeAPI{
		versionErr:  wantErr,
		doctorDelay: time.Hour,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := testBackendWithAPI(api).Doctor(ctx, DoctorRequest{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("doctor error=%v, want original probe error %v", err, wantErr)
	}
	if ctx.Err() != nil {
		t.Fatalf("sibling probes were not canceled before the parent context expired: %v", ctx.Err())
	}
}

func TestCleanupDestroysStoppedClaimByDefault(t *testing.T) {
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	if _, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}}); err != nil {
		t.Fatal(err)
	}
	api.machine.Status = "STOPPED"
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.removed) != 1 || len(api.suspended) != 0 {
		t.Fatalf("removed=%v suspended=%v", api.removed, api.suspended)
	}
}

func TestCleanupRejectsPartialOrMismatchedOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*LeaseClaim)
	}{
		{name: "missing cloud id with mutable label fallback", mutate: func(claim *LeaseClaim) { claim.CloudID = "" }},
		{name: "mismatched provider scope", mutate: func(claim *LeaseClaim) { claim.ProviderScope = machineScope("vm-other") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupState(t)
			api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
			var stderr bytes.Buffer
			b := testBackendWithAPI(api)
			b.rt.Stderr = &stderr
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			if err != nil {
				t.Fatal(err)
			}
			claim, ok, err := resolveClaim(lease.LeaseID)
			if err != nil || !ok {
				t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
			}
			replacement := claim
			tc.mutate(&replacement)
			if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, replacement); err != nil {
				t.Fatal(err)
			}
			api.machine.Status = "STOPPED"
			if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if len(api.removed) != 0 || len(api.suspended) != 0 {
				t.Fatalf("unsafe cleanup mutation: removed=%v suspended=%v", api.removed, api.suspended)
			}
			if !strings.Contains(stderr.String(), "reason=ownership") {
				t.Fatalf("stderr=%q", stderr.String())
			}
			if _, ok, err := resolveClaim(lease.LeaseID); err != nil || !ok {
				t.Fatalf("partial claim should remain for manual repair: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestProviderFlagsAndValidation(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--machine0-cli", "/opt/m0", "--machine0-size", "gpu-h100-1", "--machine0-region", "us-east", "--machine0-release-policy", "suspend", "--machine0-image-version", "4", "--machine0-poll-interval", "9s"}); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = providerName
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Machine0.CLIPath != "/opt/m0" || cfg.Machine0.Size != "gpu-h100-1" || !cfg.Machine0.SizeExplicit || cfg.Machine0.Region != "us-east" || cfg.Machine0.ReleasePolicy != "suspend" || cfg.Machine0.ImageVersion != 4 || cfg.Machine0.PollInterval != 9*time.Second {
		t.Fatalf("cfg=%#v", cfg.Machine0)
	}
	cfg.Machine0.ReleasePolicy = "stop"
	if err := (Provider{}).ValidateConfig(cfg); err == nil {
		t.Fatal("expected release policy validation error")
	}
}

func TestProviderCapabilities(t *testing.T) {
	spec := (Provider{}).Spec()
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeaturePauseResume, core.FeatureCheckpoint, core.FeatureSnapshot} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %s", feature)
		}
	}
	capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: core.Config{Provider: providerName}, Server: core.Server{Provider: providerName, CloudID: "vm-1"}})
	if !ok || capability.Kind != core.CheckpointKindMachine0 || !capability.Direct {
		t.Fatalf("capability=%#v ok=%v", capability, ok)
	}
}

func TestProviderClassCatalogAndSizeSelection(t *testing.T) {
	provider := Provider{}
	wantSizes := []string{"large", "xl", "xxl", "xxxl", "4xl", "5xl"}
	classes := core.CanonicalProviderClasses()
	profiles := provider.ClassProfiles()
	if provider.Spec().ClassDisposition != core.ProviderClassDispositionMapped || len(profiles) != len(classes) {
		t.Fatalf("disposition=%s profiles=%#v", provider.Spec().ClassDisposition, profiles)
	}
	if _, ok := any(provider).(core.ProviderClassSpecProvider); ok {
		t.Fatal("Machine0 unexpectedly exposes the historical compatibility class summary")
	}
	for index, class := range classes {
		t.Run(class, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.Class = class
			core.MarkClassExplicit(&cfg)
			if got := provider.ServerTypeForClass(class); got != wantSizes[index] {
				t.Fatalf("class size=%q want=%q", got, wantSizes[index])
			}
			if err := provider.ApplyConfigDefaults(&cfg); err != nil || cfg.Machine0.Size != wantSizes[index] || cfg.ServerType != wantSizes[index] {
				t.Fatalf("size=%q serverType=%q err=%v want=%q", cfg.Machine0.Size, cfg.ServerType, err, wantSizes[index])
			}
			profile := profiles[index]
			if profile.Class != class || profile.Primary.Type != wantSizes[index] || profile.Primary.VCPU == nil || *profile.Primary.VCPU <= 0 || profile.Primary.Memory == nil || profile.Primary.Memory.Value <= 0 {
				t.Fatalf("class profile=%#v", profile)
			}
		})
	}

	for _, tc := range []struct {
		name           string
		nativeSize     string
		nativeExplicit bool
		serverType     string
		wantSize       string
	}{
		{name: "legacy default without provenance maps the class", nativeSize: "large", wantSize: "5xl"},
		{name: "legacy stored size without provenance maps the class", nativeSize: "xxxl", wantSize: "5xl"},
		{name: "explicit default-valued native size overrides class", nativeSize: "large", nativeExplicit: true, wantSize: "large"},
		{name: "explicit arbitrary native size overrides class", nativeSize: "gpu-h100-1", nativeExplicit: true, wantSize: "gpu-h100-1"},
		{name: "explicit generic type overrides native selection", nativeSize: "large", nativeExplicit: true, serverType: "gpu-h200-1", wantSize: "gpu-h200-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.Class = "beast"
			core.MarkClassExplicit(&cfg)
			cfg.Machine0.Size = tc.nativeSize
			cfg.Machine0.SizeExplicit = tc.nativeExplicit
			if tc.serverType != "" {
				cfg.ServerType = tc.serverType
				cfg.ServerTypeExplicit = true
			}
			if got := provider.ServerTypeForConfig(cfg); got != tc.wantSize {
				t.Fatalf("resolved size=%q want=%q", got, tc.wantSize)
			}
			if got, selected := provider.ServerTypeOverrideForConfig(cfg); selected != tc.nativeExplicit || selected && got != tc.nativeSize {
				t.Fatalf("native override=%q selected=%t want=%q selected=%t", got, selected, tc.nativeSize, tc.nativeExplicit)
			}
			if err := provider.ApplyConfigDefaults(&cfg); err != nil || cfg.Machine0.Size != tc.wantSize || cfg.ServerType != tc.wantSize {
				t.Fatalf("native size=%q serverType=%q err=%v want=%q", cfg.Machine0.Size, cfg.ServerType, err, tc.wantSize)
			}
		})
	}

	for _, size := range []string{"large", "gpu-h100-1"} {
		t.Run("CLI native size overrides inherited selection "+size, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.Class = "beast"
			core.MarkClassExplicit(&cfg)
			cfg.Machine0.Size = "xl-nvme"
			cfg.Machine0.SizeExplicit = true
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := registerFlags(fs, cfg)
			if err := fs.Parse([]string{"--machine0-size", size}); err != nil {
				t.Fatal(err)
			}
			if err := applyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.Machine0.Size != size || !cfg.Machine0.SizeExplicit || provider.ServerTypeForConfig(cfg) != size {
				t.Fatalf("native size=%q explicit=%t resolved=%q", cfg.Machine0.Size, cfg.Machine0.SizeExplicit, provider.ServerTypeForConfig(cfg))
			}
		})
	}

	t.Run("changing an implicit class remaps a previously resolved size", func(t *testing.T) {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.Class = "fast"
		core.MarkClassExplicit(&cfg)
		if err := provider.ApplyConfigDefaults(&cfg); err != nil || cfg.Machine0.Size != "xxxl" {
			t.Fatalf("first size=%q err=%v", cfg.Machine0.Size, err)
		}
		cfg.Class = "beast"
		core.MarkClassExplicit(&cfg)
		if err := provider.ApplyConfigDefaults(&cfg); err != nil || cfg.Machine0.Size != "5xl" {
			t.Fatalf("updated size=%q err=%v", cfg.Machine0.Size, err)
		}
	})

	t.Run("implicit class preserves existing default", func(t *testing.T) {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		if err := provider.ApplyConfigDefaults(&cfg); err != nil || cfg.Machine0.Size != "large" {
			t.Fatalf("default size=%q err=%v", cfg.Machine0.Size, err)
		}
	})
}

func TestNativeCheckpointWorkdirUsesResolvedMachine0Root(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.WorkRoot = ""
	got := (Provider{}).NativeCheckpointWorkdir(core.NativeCheckpointWorkdirRequest{
		Config:   cfg,
		Server:   core.Server{Labels: map[string]string{"work_root": "/home/nix/crabbox"}},
		LeaseID:  "cbx_123",
		RepoName: "my-app",
	})
	if got != "/home/nix/crabbox/cbx_123/my-app" {
		t.Fatalf("checkpoint workdir=%q", got)
	}
}

func checkpointCreateFixture(t *testing.T) (*backend, *fakeAPI, LeaseTarget, LeaseClaim, core.NativeCheckpointCreateRequest) {
	t.Helper()
	repo := setupState(t)
	api := &fakeAPI{sizes: []machineSize{testSize()}, getSequence: []machine{readyMachine("203.0.113.10")}}
	b := testBackendWithAPI(api)
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveClaim(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	req := core.NativeCheckpointCreateRequest{
		Config:       b.cfg,
		Server:       lease.Server,
		Target:       lease.SSH,
		CheckpointID: "chk_0123456789abcdef",
		LeaseID:      lease.LeaseID,
		Name:         "baseline",
		RepoName:     "my-app",
		Strategy:     core.CheckpointStrategyImage,
		Wait:         true,
		WaitTimeout:  time.Minute,
		Stderr:       io.Discard,
	}
	api.imageDetail = readyCheckpointImage(req, claim, 1)
	api.actions = nil
	return b, api, lease, claim, req
}

func checkpointSource(req core.NativeCheckpointCreateRequest, ip string) machine {
	item := readyMachine(ip)
	item.Name = req.Server.Name
	return item
}

func readyCheckpointImage(req core.NativeCheckpointCreateRequest, claim LeaseClaim, version int) machineImageDetail {
	return machineImageDetail{
		Image: machineImage{ID: "img-1", Name: req.Name, Status: "READY"},
		Versions: []machineImageVersion{{
			ID: "iv-1", Version: version, Status: "DRAFT", DisplayStatus: "DRAFT", SnapshotStatus: "READY",
			Metadata: map[string]any{"crabbox_checkpoint": req.CheckpointID, "crabbox_lease": claim.LeaseID, "crabbox_source": req.Server.CloudID},
		}},
	}
}

func checkpointImageSnapshotState(req core.NativeCheckpointCreateRequest, claim LeaseClaim, version int, snapshotStatus string) machineImageDetail {
	detail := readyCheckpointImage(req, claim, version)
	detail.Versions[0].SnapshotStatus = snapshotStatus
	return detail
}

func TestCreateNativeCheckpointStopsSavesRestartsAndRefreshesClaimEndpoint(t *testing.T) {
	b, api, lease, claim, req := checkpointCreateFixture(t)
	req.Wait = false
	api.imageDetails = []machineImageDetail{
		checkpointImageSnapshotState(req, claim, 1, "CREATING"),
		readyCheckpointImage(req, claim, 1),
	}
	api.rejectStartBeforeReady = true
	api.getSequence = []machine{
		checkpointSource(req, "203.0.113.10"),
		checkpointSource(req, "203.0.113.10"),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPING"; return item }(),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPED"; return item }(),
		func() machine { item := checkpointSource(req, "203.0.113.77"); item.Status = "STARTING"; return item }(),
		checkpointSource(req, "203.0.113.77"),
	}
	b.prepareNativeImageSource = func(context.Context, SSHTarget) error {
		api.actions = append(api.actions, "prepare")
		return nil
	}
	if err := os.WriteFile(lease.SSH.KnownHostsFile, []byte("stale checkpoint host key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	globalKnownHosts := filepath.Join(home, ".ssh", "known_hosts")
	if err := os.MkdirAll(filepath.Dir(globalKnownHosts), 0o700); err != nil {
		t.Fatal(err)
	}
	const globalTrust = "shared global host key must survive\n"
	if err := os.WriteFile(globalKnownHosts, []byte(globalTrust), 0o600); err != nil {
		t.Fatal(err)
	}
	b.waitSSH = func(_ context.Context, target *SSHTarget, _ time.Duration) error {
		if target.KnownHostsFile != lease.SSH.KnownHostsFile {
			t.Fatalf("restart known hosts=%q want=%q", target.KnownHostsFile, lease.SSH.KnownHostsFile)
		}
		if _, err := os.Stat(target.KnownHostsFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("checkpoint restart did not reset stale isolated trust before readiness: %v", err)
		}
		return nil
	}

	result, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.ID != "img-1@v1" || result.Metadata[metadataImageVersion] != "1" {
		t.Fatalf("result=%#v", result)
	}
	if got, want := strings.Join(api.actions, ","), "prepare,stop,save,image:CREATING,image:READY,start"; got != want {
		t.Fatalf("actions=%q want=%q", got, want)
	}
	if len(api.stopped) != 1 || len(api.started) != 1 || len(api.savedImages) != 1 {
		t.Fatalf("stopped=%v started=%v saved=%v", api.stopped, api.started, api.savedImages)
	}
	globalAfter, err := os.ReadFile(globalKnownHosts)
	if err != nil || string(globalAfter) != globalTrust {
		t.Fatalf("global known_hosts changed: %q err=%v", globalAfter, err)
	}
	updated, ok, err := resolveClaim(claim.LeaseID)
	if err != nil || !ok || updated.CloudID != claim.CloudID || updated.SSHHost != "203.0.113.77" || updated.Labels["machine0_status"] != "RUNNING" {
		t.Fatalf("updated claim=%#v ok=%v err=%v", updated, ok, err)
	}
}

func TestCreateNativeCheckpointSaveFailureRestartsSource(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	api.saveErr = errors.New("image save rejected")
	api.getSequence = []machine{
		checkpointSource(req, "203.0.113.10"),
		checkpointSource(req, "203.0.113.10"),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPED"; return item }(),
		checkpointSource(req, "203.0.113.10"),
	}

	result, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err == nil || !strings.Contains(err.Error(), "image save rejected") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Image.ID != "" || strings.Join(api.actions, ",") != "stop,save,start" {
		t.Fatalf("result=%#v actions=%v", result, api.actions)
	}
	if len(api.started) != 1 {
		t.Fatalf("save failure left source stopped: started=%v", api.started)
	}
}

func TestCreateNativeCheckpointJoinsSaveAndRestartFailures(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	api.saveErr = errors.New("image save rejected")
	api.startErr = errors.New("start rejected")
	api.getSequence = []machine{
		checkpointSource(req, "203.0.113.10"),
		checkpointSource(req, "203.0.113.10"),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPED"; return item }(),
	}

	_, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err == nil || !strings.Contains(err.Error(), "image save rejected") || !strings.Contains(err.Error(), "start rejected") {
		t.Fatalf("joined err=%v", err)
	}
	if strings.Join(api.actions, ",") != "stop,save,start" {
		t.Fatalf("actions=%v", api.actions)
	}
}

func TestCreateNativeCheckpointRestartFailureKeepsObservedImageIdentity(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	api.startErr = errors.New("start rejected")
	api.getSequence = []machine{
		checkpointSource(req, "203.0.113.10"),
		checkpointSource(req, "203.0.113.10"),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPED"; return item }(),
	}

	result, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err == nil || !strings.Contains(err.Error(), "start rejected") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Image.ID != "img-1@v1" || result.Metadata[metadataImageVersion] != "1" {
		t.Fatalf("restart failure lost cleanup identity: %#v", result)
	}
}

func TestCreateNativeCheckpointRestartsAfterTerminalSnapshotState(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	api.imageDetail = checkpointImageSnapshotState(req, claim, 1, "FAILED")
	api.getSequence = []machine{
		checkpointSource(req, "203.0.113.10"),
		checkpointSource(req, "203.0.113.10"),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPED"; return item }(),
		checkpointSource(req, "203.0.113.10"),
	}

	result, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err == nil || !strings.Contains(err.Error(), "terminal state") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Image.ID != "img-1@v1" || strings.Join(api.actions, ",") != "stop,save,image:FAILED,start" {
		t.Fatalf("result=%#v actions=%v", result, api.actions)
	}
	if len(api.started) != 1 {
		t.Fatalf("terminal snapshot left source stopped: started=%v", api.started)
	}
}

func TestCreateNativeCheckpointJoinsSnapshotAndRestartFailures(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	api.imageDetail = checkpointImageSnapshotState(req, claim, 1, "FAILED")
	api.startErr = errors.New("start rejected")
	api.getSequence = []machine{
		checkpointSource(req, "203.0.113.10"),
		checkpointSource(req, "203.0.113.10"),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPED"; return item }(),
	}

	result, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err == nil || !strings.Contains(err.Error(), "terminal state") || !strings.Contains(err.Error(), "start rejected") {
		t.Fatalf("result=%#v joined err=%v", result, err)
	}
	if result.Image.ID != "img-1@v1" || strings.Join(api.actions, ",") != "stop,save,image:FAILED,start" {
		t.Fatalf("result=%#v actions=%v", result, api.actions)
	}
}

func TestCreateNativeCheckpointRestartsAfterSnapshotTimeout(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	req.Wait = false
	req.WaitTimeout = 10 * time.Millisecond
	b.sleep = sleepContext
	api.imageDetail = checkpointImageSnapshotState(req, claim, 1, "CREATING")
	api.getSequence = []machine{
		checkpointSource(req, "203.0.113.10"),
		checkpointSource(req, "203.0.113.10"),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPED"; return item }(),
		checkpointSource(req, "203.0.113.10"),
	}

	result, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Image.ID != "img-1@v1" || strings.Join(api.actions, ",") != "stop,save,image:CREATING,start" {
		t.Fatalf("result=%#v actions=%v", result, api.actions)
	}
	if len(api.started) != 1 {
		t.Fatalf("snapshot timeout left source stopped: started=%v", api.started)
	}
}

func TestCreateNativeCheckpointRestartsAfterCallerCancellation(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	b.sleep = sleepContext
	api.imageDetail = checkpointImageSnapshotState(req, claim, 1, "CREATING")
	api.getSequence = []machine{
		checkpointSource(req, "203.0.113.10"),
		checkpointSource(req, "203.0.113.10"),
		func() machine { item := checkpointSource(req, "203.0.113.10"); item.Status = "STOPPED"; return item }(),
		checkpointSource(req, "203.0.113.10"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := b.createNativeCheckpoint(ctx, req, claim)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Image.ID != "" || strings.Join(api.actions, ",") != "stop,start" {
		t.Fatalf("result=%#v actions=%v", result, api.actions)
	}
	if len(api.started) != 1 {
		t.Fatalf("caller cancellation left source stopped: started=%v", api.started)
	}
}

func TestMachine0CheckpointSnapshotTimeoutPrecedence(t *testing.T) {
	if got := machine0CheckpointSnapshotTimeout(7*time.Minute, 15*time.Minute); got != 7*time.Minute {
		t.Fatalf("requested timeout=%s", got)
	}
	if got := machine0CheckpointSnapshotTimeout(0, 15*time.Minute); got != 15*time.Minute {
		t.Fatalf("fallback timeout=%s", got)
	}
}

func TestCreateNativeCheckpointStopTimeoutRestartsWithoutSaving(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	b.cfg.Machine0.CreateTimeout = time.Nanosecond
	b.sleep = sleepContext
	stopping := checkpointSource(req, "203.0.113.10")
	stopping.Status = "STOPPING"
	api.getSequence = []machine{checkpointSource(req, "203.0.113.10"), checkpointSource(req, "203.0.113.10"), stopping, checkpointSource(req, "203.0.113.10")}

	result, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(api.stopped) != 1 || len(api.started) != 1 || len(api.savedImages) != 0 {
		t.Fatalf("stopped=%v started=%v saved=%v", api.stopped, api.started, api.savedImages)
	}
}

func TestCreateNativeCheckpointPreservesAlreadyStoppedSource(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	req.Wait = false
	api.imageDetails = []machineImageDetail{
		checkpointImageSnapshotState(req, claim, 1, "CREATING"),
		readyCheckpointImage(req, claim, 1),
	}
	stopped := checkpointSource(req, "203.0.113.10")
	stopped.Status = "STOPPED"
	api.getSequence = []machine{stopped}
	b.prepareNativeImageSource = func(context.Context, SSHTarget) error {
		t.Fatal("pre-stopped source must not require SSH preparation")
		return nil
	}

	result, err := b.createNativeCheckpoint(context.Background(), req, claim)
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.ID != "img-1@v1" || strings.Join(api.actions, ",") != "save,image:CREATING,image:READY" || len(api.stopped) != 0 || len(api.started) != 0 {
		t.Fatalf("result=%#v actions=%v stopped=%v started=%v", result, api.actions, api.stopped, api.started)
	}
}

func TestCreateNativeCheckpointRejectsMismatchedClaimScopeBeforeMutation(t *testing.T) {
	b, api, _, claim, req := checkpointCreateFixture(t)
	original := claim
	claim.ProviderScope = machineScope("vm-other")
	if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, original, claim); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveClaim(claim.LeaseID)
	if err != nil || !ok || claim.ProviderScope != machineScope("vm-other") {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	api.getSequence = []machine{checkpointSource(req, "203.0.113.10")}

	_, err = b.createNativeCheckpoint(context.Background(), req, claim)
	if err == nil || !strings.Contains(err.Error(), "provider scope mismatch") {
		t.Fatalf("err=%v", err)
	}
	if len(api.stopped) != 0 || len(api.savedImages) != 0 || len(api.started) != 0 {
		t.Fatalf("mismatched ownership mutated provider: stopped=%v saved=%v started=%v", api.stopped, api.savedImages, api.started)
	}
}

func TestWaitForStoppedRequiresExactStoppedState(t *testing.T) {
	t.Run("stopping to stopped", func(t *testing.T) {
		stopping := readyMachine("203.0.113.10")
		stopping.Status = "STOPPING"
		stopped := readyMachine("203.0.113.10")
		stopped.Status = "STOPPED"
		b := testBackendWithAPI(&fakeAPI{getSequence: []machine{stopping, stopped}})
		got, err := b.waitForStopped(context.Background(), stopped.Name, time.Minute)
		if err != nil || got.Status != "STOPPED" {
			t.Fatalf("got=%#v err=%v", got, err)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		failed := readyMachine("203.0.113.10")
		failed.Status = "ERRORED"
		failed.LastErrorMessage = "stop failed"
		b := testBackendWithAPI(&fakeAPI{machine: failed})
		if _, err := b.waitForStopped(context.Background(), failed.Name, time.Minute); err == nil || !strings.Contains(err.Error(), "stop failed") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("unexpected stable state", func(t *testing.T) {
		suspended := readyMachine("")
		suspended.Status = "SUSPENDED"
		b := testBackendWithAPI(&fakeAPI{machine: suspended})
		if _, err := b.waitForStopped(context.Background(), suspended.Name, time.Minute); err == nil || !strings.Contains(err.Error(), "unexpected state SUSPENDED") {
			t.Fatalf("err=%v", err)
		}
	})

	for _, tc := range []struct {
		name    string
		ctx     func() context.Context
		timeout time.Duration
		want    string
	}{
		{name: "canceled", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }, timeout: time.Minute, want: "context canceled"},
		{name: "timeout", ctx: context.Background, timeout: 0, want: "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopping := readyMachine("203.0.113.10")
			stopping.Status = "STOPPING"
			b := testBackendWithAPI(&fakeAPI{machine: stopping})
			b.sleep = sleepContext
			_, err := b.waitForStopped(tc.ctx(), stopping.Name, tc.timeout)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCheckpointImageRequiresExactVersionAndRemoteOwnershipMetadata(t *testing.T) {
	api := &fakeAPI{imageDetail: machineImageDetail{
		Image: machineImage{ID: "img-1", Name: "baseline", Status: "READY"},
		Versions: []machineImageVersion{{Version: 2, DisplayStatus: "ACTIVE", Metadata: map[string]any{
			"crabbox_checkpoint": "chk_123",
			"crabbox_lease":      "cbx_123",
			"crabbox_source":     "vm-123",
		}}},
	}}
	b := testBackendWithAPI(api)
	api.images = []machineImage{api.imageDetail.Image}
	req := core.NativeCheckpointResourceRequest{
		Image:    core.NativeCheckpointImage{Provider: providerName, Kind: core.CheckpointKindMachine0, Direct: true, ResourceID: "img-1"},
		Metadata: map[string]string{metadataImageName: "baseline", metadataImageID: "img-1", metadataImageVersion: "2", metadataSourceMachine: "vm-123", "crabbox_checkpoint": "chk_123", "crabbox_lease": "cbx_123"},
	}
	if _, version, err := b.loadCheckpointImage(context.Background(), req); err != nil || version.Version != 2 {
		t.Fatalf("version=%#v err=%v", version, err)
	}
	api.imageDetail.Versions[0].Metadata["crabbox_source"] = "vm-other"
	if _, _, err := b.loadCheckpointImage(context.Background(), req); err == nil || !strings.Contains(err.Error(), "mismatched crabbox_source") {
		t.Fatalf("err=%v", err)
	}
}

func TestWholeImageCheckpointDeleteRefusesUnrelatedLaterVersions(t *testing.T) {
	owned := machineImageVersion{Version: 1, Status: "ACTIVE", DisplayStatus: "ACTIVE", SnapshotStatus: "READY", Metadata: map[string]any{
		"crabbox_checkpoint": "chk_123",
		"crabbox_lease":      "cbx_123",
		"crabbox_source":     "vm-123",
	}}
	api := &fakeAPI{imageDetail: machineImageDetail{
		Image:    machineImage{ID: "img-1", Name: "baseline", Status: "READY"},
		Versions: []machineImageVersion{owned, {Version: 2, Status: "DRAFT", DisplayStatus: "DRAFT", SnapshotStatus: "READY"}},
	}}
	b := testBackendWithAPI(api)
	api.images = []machineImage{api.imageDetail.Image}
	req := core.NativeCheckpointResourceRequest{
		Image: core.NativeCheckpointImage{Provider: providerName, Kind: core.CheckpointKindMachine0, Direct: true, ResourceID: "img-1"},
		Metadata: map[string]string{
			metadataImageName: "baseline", metadataImageID: "img-1", metadataImageVersion: "1", metadataCreatedImage: "true", metadataSourceMachine: "vm-123", "crabbox_checkpoint": "chk_123", "crabbox_lease": "cbx_123",
		},
	}
	if err := b.deleteNativeCheckpoint(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no longer the only version") || !strings.Contains(err.Error(), "machine0 images versions rm baseline 1 --yes") {
		t.Fatalf("unsafe whole-image deletion err=%v", err)
	}
	if len(api.removedImage) != 0 {
		t.Fatalf("unsafe deletion removed provider data: %v", api.removedImage)
	}
	api.imageDetail.Versions = []machineImageVersion{owned}
	if err := b.deleteNativeCheckpoint(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(api.removedImage) != 1 || api.removedImage[0] != "baseline" {
		t.Fatalf("single owned version did not remove image: %v", api.removedImage)
	}
}

func TestDraftImageVersionRequiresReadySnapshot(t *testing.T) {
	if !imageVersionReady(machineImageVersion{Status: "DRAFT", DisplayStatus: "DRAFT", SnapshotStatus: "READY"}) {
		t.Fatal("ready draft image version should be usable with --image-version")
	}
	if imageVersionReady(machineImageVersion{Status: "DRAFT", DisplayStatus: "DRAFT", SnapshotStatus: "CREATING"}) {
		t.Fatal("creating draft image version must not be treated as ready")
	}
	if !imageVersionReady(machineImageVersion{Status: "ACTIVE", DisplayStatus: "ACTIVE", SnapshotStatus: "READY"}) {
		t.Fatal("active version with ready snapshot should be usable")
	}
	if imageVersionReady(machineImageVersion{Status: "ERRORED", DisplayStatus: "DRAFT", SnapshotStatus: "READY"}) {
		t.Fatal("terminal version must not become usable from a ready snapshot")
	}
}

func TestImageWaitKeepsObservedVersionOnTimeoutAndError(t *testing.T) {
	expected := map[string]string{"crabbox_checkpoint": "chk_123", "crabbox_lease": "cbx_123", "crabbox_source": "vm-123"}
	pending := machineImageDetail{Image: machineImage{ID: "img-1", Name: "baseline"}, Versions: []machineImageVersion{{
		Version: 2, Status: "DRAFT", DisplayStatus: "DRAFT", SnapshotStatus: "CREATING", Metadata: map[string]any{"crabbox_checkpoint": "chk_123", "crabbox_lease": "cbx_123", "crabbox_source": "vm-123"},
	}}}

	t.Run("immediate timeout does not fetch", func(t *testing.T) {
		b := testBackendWithAPI(&fakeAPI{imageDetail: pending})
		detail, version, err := b.waitForImageVersion(context.Background(), "baseline", 1, expected, true, 0, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err=%v", err)
		}
		if detail.Image.ID != "" || version.Version != 0 {
			t.Fatalf("immediate timeout fetched identity: detail=%#v version=%#v", detail, version)
		}
	})

	t.Run("timeout retains observed identity", func(t *testing.T) {
		b := testBackendWithAPI(&fakeAPI{imageDetail: pending})
		b.sleep = sleepContext
		detail, version, err := b.waitForImageVersion(context.Background(), "baseline", 1, expected, true, 10*time.Millisecond, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err=%v", err)
		}
		if detail.Image.ID != "img-1" || version.Version != 2 {
			t.Fatalf("timeout lost observed identity: detail=%#v version=%#v", detail, version)
		}
	})

	t.Run("later get error", func(t *testing.T) {
		api := &fakeAPI{imageDetails: []machineImageDetail{pending}, imageErrors: []error{nil, errors.New("get failed")}}
		b := testBackendWithAPI(api)
		b.sleep = func(context.Context, time.Duration) error { return nil }
		detail, version, err := b.waitForImageVersion(context.Background(), "baseline", 1, expected, true, time.Minute, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "get failed") {
			t.Fatalf("err=%v", err)
		}
		if detail.Image.ID != "img-1" || version.Version != 2 {
			t.Fatalf("error lost observed identity: detail=%#v version=%#v", detail, version)
		}
	})

	t.Run("terminal state at deadline", func(t *testing.T) {
		terminal := pending
		terminal.Versions = append([]machineImageVersion(nil), pending.Versions...)
		terminal.Versions[0].SnapshotStatus = "FAILED"
		api := &fakeAPI{imageFn: func(ctx context.Context, _ string) (machineImageDetail, error) {
			<-ctx.Done()
			return terminal, nil
		}}
		b := testBackendWithAPI(api)
		_, version, err := b.waitForImageVersion(context.Background(), "baseline", 1, expected, true, 10*time.Millisecond, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "terminal state") || strings.Contains(err.Error(), "timed out") || version.Version != 2 {
			t.Fatalf("version=%#v err=%v", version, err)
		}
	})

	t.Run("client deadline", func(t *testing.T) {
		api := &fakeAPI{imageFn: func(context.Context, string) (machineImageDetail, error) {
			return machineImageDetail{}, context.DeadlineExceeded
		}}
		b := testBackendWithAPI(api)
		_, _, err := b.waitForImageVersion(context.Background(), "baseline", 1, expected, true, time.Minute, io.Discard)
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out waiting") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("later response omits version", func(t *testing.T) {
		api := &fakeAPI{imageDetails: []machineImageDetail{pending, {Image: machineImage{ID: "img-1", Name: "baseline"}}}}
		b := testBackendWithAPI(api)
		sleeps := 0
		b.sleep = func(ctx context.Context, _ time.Duration) error {
			sleeps++
			if sleeps == 1 {
				return nil
			}
			<-ctx.Done()
			return context.Cause(ctx)
		}
		detail, version, err := b.waitForImageVersion(context.Background(), "baseline", 1, expected, true, 10*time.Millisecond, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err=%v", err)
		}
		if detail.Image.ID != "img-1" || version.Version != 2 {
			t.Fatalf("later response lost observed identity: detail=%#v version=%#v", detail, version)
		}
	})
}

func TestImageWaitReportsProgressOnlyWhileContinuing(t *testing.T) {
	expected := map[string]string{"crabbox_checkpoint": "chk_123", "crabbox_lease": "cbx_123", "crabbox_source": "vm-123"}
	pending := machineImageDetail{Image: machineImage{ID: "img-1", Name: "baseline"}, Versions: []machineImageVersion{{
		Version: 2, Status: "DRAFT", DisplayStatus: "DRAFT", SnapshotStatus: "CREATING", Metadata: map[string]any{"crabbox_checkpoint": "chk_123", "crabbox_lease": "cbx_123", "crabbox_source": "vm-123"},
	}}}
	ready := pending
	ready.Versions = append([]machineImageVersion(nil), pending.Versions...)
	ready.Versions[0].SnapshotStatus = "READY"
	b := testBackendWithAPI(&fakeAPI{imageDetails: []machineImageDetail{pending, ready}})
	var progress bytes.Buffer
	_, _, err := b.waitForImageVersion(context.Background(), "baseline", 1, expected, true, time.Minute, &progress)
	if err != nil {
		t.Fatal(err)
	}
	if got := progress.String(); got != "waiting image=baseline version=2 state=DRAFT\n" {
		t.Fatalf("progress=%q", got)
	}
}

func TestImageWaitSelectsMatchingConcurrentSaveVersion(t *testing.T) {
	expected := map[string]string{"crabbox_checkpoint": "chk_ours", "crabbox_lease": "cbx_ours", "crabbox_source": "vm-ours"}
	detail := machineImageDetail{Image: machineImage{ID: "img-1", Name: "baseline"}, Versions: []machineImageVersion{
		{Version: 3, Status: "DRAFT", DisplayStatus: "DRAFT", SnapshotStatus: "READY", Metadata: map[string]any{"crabbox_checkpoint": "chk_other", "crabbox_lease": "cbx_other", "crabbox_source": "vm-other"}},
		{Version: 2, Status: "DRAFT", DisplayStatus: "DRAFT", SnapshotStatus: "READY", Metadata: map[string]any{"crabbox_checkpoint": "chk_ours", "crabbox_lease": "cbx_ours", "crabbox_source": "vm-ours"}},
	}}
	b := testBackendWithAPI(&fakeAPI{imageDetail: detail})
	_, version, err := b.waitForImageVersion(context.Background(), "baseline", 1, expected, true, time.Minute, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if version.Version != 2 {
		t.Fatalf("selected concurrent version v%d, want owned v2", version.Version)
	}
}

package utm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"golang.org/x/crypto/ssh"
)

type macOSFixture struct {
	base           *controlFixture
	actions        []string
	fail           string
	cancelCreate   func()
	cancelInstall  func()
	installQueries int
	address        string
}

func (f *macOSFixture) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	out := func(s string) (core.LocalCommandResult, error) { return core.LocalCommandResult{Stdout: s}, nil }
	if req.Name == "/usr/bin/osascript" && len(req.Args) == 1 && req.Args[0] == "-" {
		data, _ := io.ReadAll(req.Stdin)
		script := string(data)
		switch {
		case strings.Contains(script, "make new virtual machine"):
			f.actions = append(f.actions, "create")
			field := func(pattern string) string {
				m := regexp.MustCompile(pattern).FindStringSubmatch(script)
				if len(m) != 2 {
					panic("missing nonsecret create field")
				}
				return m[1]
			}
			v := vm{ID: "aaaaaaaa-aaaa-4aaa-8aaa-000000000099", Name: field(`name:"([^"]+)"`), Backend: "apple", Status: "stopped", Configuration: configuration{Notes: field(`notes:"([^"]+)"`), Networks: []network{{Mode: "shared", Address: "02:00:00:00:00:99"}}}}
			f.base.vms = append(f.base.vms, v)
			path := f.base.bundlePath(v.ID)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return core.LocalCommandResult{}, err
			}
			doc := map[string]any{"Backend": "Apple", "Information": bundleInformation{UUID: v.ID, Name: v.Name, Notes: v.Configuration.Notes}, "System": map[string]any{"Architecture": "aarch64", "Boot": map[string]any{"OperatingSystem": "macOS"}}, "Network": []bundleNetwork{{Mode: "Shared", MacAddress: v.Configuration.Networks[0].Address}}}
			encoded, _ := json.Marshal(doc)
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				return core.LocalCommandResult{}, err
			}
			if f.cancelCreate != nil {
				f.cancelCreate()
			}
			if f.fail == "create" {
				return core.LocalCommandResult{}, errors.New("creation interrupted after new VM")
			}
			return out(v.ID)
		case strings.Contains(script, "set p to query install"):
			f.actions = append(f.actions, "query-install")
			f.installQueries++
			if f.fail == "install" {
				return core.LocalCommandResult{}, errors.New("restore failed")
			}
			if f.fail == "lost-query" {
				return core.LocalCommandResult{}, errors.New("No installation was started")
			}
			if f.fail == "pending" {
				if f.cancelInstall != nil {
					f.cancelInstall()
				}
				return out("true\t0.5")
			}
			return out("false\t1.0")
		case strings.Contains(script, "install targetVM"):
			f.actions = append(f.actions, "install")
			return out("")
		case strings.Contains(script, "provisioning {"):
			f.actions = append(f.actions, "firstboot")
			if f.fail == "firstboot" {
				return core.LocalCommandResult{Stdout: script, Stderr: script, ExitCode: 1}, fmt.Errorf("echoed event: %s", script)
			}
			f.base.vms[len(f.base.vms)-1].Status = "started"
			return out("submitted")
		case strings.Contains(script, "query ip targetVM"):
			f.actions = append(f.actions, "query-ip")
			return out(f.address)
		}
		req.Stdin = strings.NewReader(script)
	}
	return f.base.Run(ctx, req)
}
func setupMacOS(t *testing.T) (*backend, *macOSFixture) {
	t.Helper()
	b, base := setup(t)
	b.cfg.UTM.Template = ""
	b.cfg.UTM.Acquisition = "ipsw"
	b.cfg.TargetOS = core.TargetMacOS
	b.cfg.UTM.SSHKey = ""
	b.cfg.UTM.InstallTimeout = time.Second
	b.cfg.UTM.IPSW = filepath.Join(t.TempDir(), "fixture.ipsw")
	if err := os.WriteFile(b.cfg.UTM.IPSW, []byte("immutable fixture restore image"), 0600); err != nil {
		t.Fatal(err)
	}
	f := &macOSFixture{base: base, address: "192.168.64.9"}
	b.rt.Exec = f
	inspect, bootstrap, ready := inspectMacOSImage, bootstrapMacOSGuest, waitForSSHReady
	inspectMacOSImage = func(_ *backend, _ context.Context, password string) (macOSImage, error) {
		if password == "" {
			return macOSImage{}, errors.New("missing random credential")
		}
		return macOSImage{GuestVersion: "27.0.1", GuestBuild: "26A434", HostVersion: "27.0.1", CPUCount: 2, MemoryMiB: 8192, Supported: true, AccountValidated: true}, nil
	}
	bootstrapMacOSGuest = func(_ *backend, _ context.Context, r *record, password, public string) error {
		if !r.InstallComplete || !r.FirstBootSubmitted || r.AccountComplete || password == "" || public == "" {
			return errors.New("bootstrap phase violation")
		}
		r.SSHHostKey = public
		return nil
	}
	waitForSSHReady = func(ctx context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if target.TargetOS != core.TargetMacOS || target.Host != f.address || target.Port != "22" || target.SSHHostKey == "" || !target.AuthoritativeKnownHosts || target.DisableHostKeyChecking || !strings.Contains(target.ReadyCheck, "sw_vers") || !strings.Contains(target.ReadyCheck, "ifconfig") {
			return errors.New("missing authenticated Darwin identity/trust")
		}
		if target.PrepareConnection == nil {
			return errors.New("missing pin preparation")
		}
		return target.PrepareConnection(ctx)
	}
	t.Cleanup(func() { inspectMacOSImage, bootstrapMacOSGuest, waitForSSHReady = inspect, bootstrap, ready })
	return b, f
}
func TestMacOSAcquisitionReuseAndRemovalPreserveSource(t *testing.T) {
	b, f := setupMacOS(t)
	source := b.cfg.UTM.IPSW
	original, _ := os.ReadFile(source)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.isMacOS() || r.SourceID != "" || !r.InstallComplete || !r.FirstBootSubmitted || !r.AccountComplete || !r.SSHReady || r.State != "ready" || r.MAC != "02:00:00:00:00:99" {
		t.Fatal("missing separate successful macOS phases")
	}
	private, err := os.ReadFile(r.SSHKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ssh.ParsePrivateKey(private); err != nil {
		t.Fatal(err)
	}
	before := len(f.actions)
	// An absent input image and renamed retained display must not trigger install.
	b.cfg.UTM.IPSW = filepath.Join(t.TempDir(), "absent.ipsw")
	f.base.vms[len(f.base.vms)-1].Name = "retained operator display name"
	path := f.base.bundlePath(r.VMID)
	data, _ := os.ReadFile(path)
	var doc map[string]any
	json.Unmarshal(data, &doc)
	doc["Information"].(map[string]any)["Name"] = f.base.vms[len(f.base.vms)-1].Name
	data, _ = json.Marshal(doc)
	os.WriteFile(path, data, 0600)
	f.address = "192.168.64.10"
	retained, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if retained.SSH.Host != f.address || retained.Server.CloudID != r.VMID || retained.SSH.Key != r.SSHKey {
		t.Fatal("retained endpoint identity changed")
	}
	for _, action := range f.actions[before:] {
		if action != "query-ip" {
			t.Fatalf("retained lease replayed %s", action)
		}
	}
	if err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: retained}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(r.SSHKey); !os.IsNotExist(err) {
		t.Fatal("owned generated key remains")
	}
	if _, ok := findVM(f.base.vms, r.VMID); ok {
		t.Fatal("owned VM remains")
	}
	current, err := os.ReadFile(source)
	if err != nil || string(current) != string(original) {
		t.Fatal("operator IPSW changed")
	}
	if records, err := b.records(); err != nil || len(records) != 0 {
		t.Fatal("removed VM journal remains")
	}
}
func TestMacOSFailuresKeepDurablePhasesAndNeverReplay(t *testing.T) {
	for _, failure := range []string{"create", "install", "lost-query", "firstboot", "pending"} {
		t.Run(failure, func(t *testing.T) {
			b, f := setupMacOS(t)
			f.fail = failure
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "pending" {
				f.cancelInstall = cancel
			}
			_, err := b.Acquire(ctx, core.AcquireRequest{})
			if err == nil {
				t.Fatal("accepted failed provisioning")
			}
			if strings.Contains(err.Error(), "password:") || strings.Contains(err.Error(), "provisioning {") {
				t.Fatal("credential-bearing source reached error")
			}
			records, e := b.records()
			if e != nil || len(records) != 1 {
				t.Fatal("ownership record missing")
			}
			r := records[0]
			if r.VMID == "" || r.State != "recovery" || r.SSHReady || r.AccountComplete {
				t.Fatal("failed phase identity lost")
			}
			if failure == "firstboot" {
				if !r.InstallComplete || !r.FirstBootSubmitted {
					t.Fatal("uncertain first boot not recorded")
				}
			} else if r.FirstBootSubmitted {
				t.Fatal("firstboot before completed install")
			}
			actions := len(f.actions)
			if _, e = b.Resolve(context.Background(), core.ResolveRequest{ID: r.LeaseID}); e == nil {
				t.Fatal("incomplete acquisition reused")
			}
			if len(f.actions) != actions {
				t.Fatal("incomplete reuse replayed lifecycle operation")
			}
			f.fail = ""
			if e = b.Cleanup(context.Background(), core.CleanupRequest{}); e != nil {
				t.Fatal(e)
			}
			if _, e = os.Stat(b.cfg.UTM.IPSW); e != nil {
				t.Fatal("cleanup touched IPSW")
			}
		})
	}
}
func TestMacOSCanceledCreationRecordsVMWithoutInstalling(t *testing.T) {
	b, f := setupMacOS(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.cancelCreate = cancel
	_, err := b.Acquire(ctx, core.AcquireRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	records, e := b.records()
	if e != nil || len(records) != 1 || records[0].VMID == "" {
		t.Fatal("canceled creation identity missing")
	}
	for _, action := range f.actions {
		if action != "create" {
			t.Fatal("canceled creation performed additional mutation")
		}
	}
	if err = b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
}
func TestMacOSUnsupportedImagesFailBeforeVMCreation(t *testing.T) {
	for _, image := range []macOSImage{
		{Error: "guest-version"}, {Error: "host-version"}, {Error: "image-incompatible"}, {Error: "account-invalid"},
		{GuestVersion: "28.0.0", HostVersion: "27.0.1", GuestBuild: "fixture", Supported: true, AccountValidated: true, CPUCount: 2, MemoryMiB: 8192},
	} {
		t.Run(image.Error+image.GuestVersion, func(t *testing.T) {
			b, f := setupMacOS(t)
			inspectMacOSImage = func(*backend, context.Context, string) (macOSImage, error) { return image, nil }
			if _, err := b.Acquire(context.Background(), core.AcquireRequest{}); err == nil {
				t.Fatal("admitted unsupported image")
			}
			if len(f.actions) != 0 {
				t.Fatal("VM operation preceded preflight")
			}
			records, err := b.records()
			if err != nil || len(records) != 0 {
				t.Fatal("unsupported image allocated ownership")
			}
		})
	}
}
func TestMacOSFirstBootEligibilityIsSingleSubmission(t *testing.T) {
	b, f := setupMacOS(t)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := b.lookup(lease.LeaseID)
	before := len(f.actions)
	if err = b.firstBootMacOS(context.Background(), &r, "fixture-unused"); err == nil {
		t.Fatal("replayed firstboot on retained VM")
	}
	if len(f.actions) != before {
		t.Fatal("ineligible firstboot sent event")
	}
}

type secretEchoRunner struct{}

func (secretEchoRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	input, _ := io.ReadAll(req.Stdin)
	if strings.Contains(strings.Join(req.Args, " "), string(input)) {
		return core.LocalCommandResult{}, errors.New("credential on argv")
	}
	return core.LocalCommandResult{Stdout: string(input), Stderr: fmt.Sprintf("escaped=%q", input), ExitCode: 1}, fmt.Errorf("wrapped %q", input)
}
func TestPrivateCommandSuppressesAllCredentialBearingResults(t *testing.T) {
	b := &backend{rt: core.Runtime{Exec: secretEchoRunner{}}}
	secret := "fixture-only\"\\secret\n"
	result, err := b.privateCommand(context.Background(), "first boot", core.LocalCommandRequest{Name: "fixture", Args: []string{"-"}, Stdin: strings.NewReader(secret)})
	if err == nil || result.Stdout != "" || result.Stderr != "" || strings.Contains(err.Error(), "fixture-only") || strings.Contains(err.Error(), "wrapped") {
		t.Fatal("private command leaked credential diagnostics")
	}
}
func TestMacOSDoctorDoesNotProvisionOrCreateJournal(t *testing.T) {
	b, f := setupMacOS(t)
	result, err := b.Doctor(context.Background(), core.DoctorRequest{ProbeSSH: true})
	if err != nil || core.DoctorChecksStatus(result.Checks) != "ok" {
		t.Fatalf("doctor failed: %v", err)
	}
	if len(f.actions) != 0 {
		t.Fatal("doctor provisioned a VM")
	}
	if records, err := b.records(); err != nil || len(records) != 0 {
		t.Fatal("doctor created journal")
	}
}

func TestMacOSConfigureDefersInputChecksForRetainedOperations(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.UTM.Acquisition = "ipsw"
	cfg.UTM.IPSW = "/absent/original.ipsw"
	cfg.TargetOS = core.TargetMacOS
	core.MarkTargetExplicit(&cfg)
	configured, err := (Provider{}).Configure(cfg, core.Runtime{Exec: secretEchoRunner{}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal("missing IPSW blocked retained configuration")
	}
	b := configured.(*backend)
	if b.cfg.TargetOS != core.TargetMacOS || b.cfg.UTM.WorkRoot != "/tmp/crabbox" {
		t.Fatal("incorrect macOS platform defaults")
	}
	if err = b.macOSInput(); err == nil {
		t.Fatal("acquisition accepted missing IPSW")
	}
	cfg.UTM.Acquisition = "other"
	if _, err = (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("unknown acquisition mode accepted")
	}
	cfg.UTM.Acquisition = "ipsw"
	cfg.TargetOS = core.TargetLinux
	if _, err = (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("explicit Linux target admitted for IPSW")
	}
}
func TestMacOSDefaultsRespectExplicitPlatformAndWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.UTM.Acquisition = "ipsw"
	cfg.UTM.WorkRoot = "/Users/fixture/work"
	if err := (Provider{}).ApplyConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != core.TargetMacOS || cfg.WorkRoot != "/Users/fixture/work" {
		t.Fatal("macOS mode defaults did not route target/work root")
	}
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)
	(Provider{}).ApplyConfigDefaults(&cfg)
	if cfg.TargetOS != core.TargetLinux {
		t.Fatal("explicit platform overwritten")
	}
}
func TestMacOSJournalModeAndPhaseValidationPreservesLegacy(t *testing.T) {
	b, _ := setupMacOS(t)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := b.lookup(lease.LeaseID)
	for _, change := range []func(*record){func(r *record) { r.SourceID = sourceUUID }, func(r *record) { r.Backend = "qemu" }, func(r *record) { r.Port = 2222 }, func(r *record) { r.InstallComplete = false }, func(r *record) { r.FirstBootSubmitted = false }, func(r *record) { r.SSHHostKey = "" }} {
		broken := r
		change(&broken)
		if broken.validate() == nil {
			t.Fatal("invalid mode/phase journal accepted")
		}
	}
	legacy := record{Version: 1, LeaseID: "fixture-legacy", AppPath: "/Applications/UTM.app", SourceID: sourceUUID, OwnedName: "crabbox-utm-fixture", Token: "fixture", Port: 2222}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var restored record
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if err = restored.validate(); err != nil || restored.isMacOS() {
		t.Fatal("legacy template journal rejected")
	}
}

func TestMacOSClaimFencedExecRefreshesOnlyLocalTarget(t *testing.T) {
	b, f := setupMacOS(t)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := recordPath(r.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.address = "192.168.64.11"
	target, err := b.macOSExecLeaseTarget(context.Background(), r, f.base.vms)
	if err != nil {
		t.Fatal(err)
	}
	if target.SSH.Host != f.address || target.SSH.SSHHostKey != r.SSHHostKey || target.LeaseID != r.LeaseID {
		t.Fatal("exec lost refreshed guest identity")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(original) {
		t.Fatal("claim-fenced exec rewrote ownership journal")
	}
	f.address = ""
	if _, err = b.macOSExecLeaseTarget(context.Background(), r, f.base.vms); err == nil {
		t.Fatal("exec reused stale address after ARP discovery failed")
	}
	f.address = "192.168.64.11"
	f.base.vms[len(f.base.vms)-1].Configuration.Notes = "changed"
	if _, err = b.macOSExecLeaseTarget(context.Background(), r, f.base.vms); err == nil {
		t.Fatal("exec accepted changed guest ownership")
	}
}

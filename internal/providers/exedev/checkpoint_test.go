package exedev

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Exercise the real provider and archive transport through an exe.dev-shaped
// SSH executable. Remote archive commands run against isolated local directories.
func TestExeDevArchiveCheckpointRestoreAndFork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake SSH executable requires a Unix host")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake SSH surface")
	}
	// Git resolves macOS's /var alias; claims must use the same repository path.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	ssh := `#!/usr/bin/env python3
import json, os, shlex, subprocess, sys
args=sys.argv[1:]
i=0
while i<len(args) and args[i].startswith('-'):
    i += 2 if args[i] in ('-o','-p','-i','-F','-S','-J','-l','-E') else 1
host=args[i]; command=' '.join(args[i+1:])
if host != 'exe.dev':
    if os.environ.get('EXE_TEST_FAIL_RESTORE') and 'tar -C' in command and '-xzf' in command: sys.exit(19)
    sys.exit(subprocess.run(command, shell=True).returncode)
p=os.environ['EXE_TEST_INVENTORY']
with open(p) as f: vms=json.load(f)
a=shlex.split(command)
if a[0]=='whoami': print(json.dumps({'email':'test@example.com'})); sys.exit(0)
if a[0]=='ls': print(json.dumps({'vms':list(vms.values())})); sys.exit(0)
if a[0]=='cp':
    assert a[1] in vms and '--copy-tags=false' in a
    name=a[2]; assert name not in vms
    vm={'vm_name':name,'ssh_dest':'builder@'+name+'.example','status':'running','tags':[]}
    vms[name]=vm
elif a[0]=='new':
    name=a[a.index('--name')+1]; assert name not in vms
    tags=[a[j+1] for j,x in enumerate(a) if x=='--tag']
    vm={'vm_name':name,'ssh_dest':'builder@'+name+'.example','status':'running','tags':tags}
    vms[name]=vm
elif a[0]=='tag':
    name=a[2]; assert name!='base'
    vms[name]['tags']=a[3:]; vm=vms[name]
elif a[0]=='rm':
    assert a[1]!='base'
    del vms[a[1]]; vm={}
else: raise Exception('unexpected command '+command)
with open(p,'w') as f: json.dump(vms,f)
print(json.dumps(vm))
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(root, "config.yaml"))
	inventory := filepath.Join(root, "inventory.json")
	t.Setenv("EXE_TEST_INVENTORY", inventory)
	if err := os.WriteFile(inventory, []byte(`{"base":{"vm_name":"base","tags":["source-private-tag"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(root, "work")
	config := "provider: exe-dev\nexeDev:\n  workRoot: " + workRoot + "\n"
	if err := os.WriteFile(os.Getenv("CRABBOX_CONFIG"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(repo)
	leaseID, slug := "cbx_abcdef123456", "source"
	vm := ownedExeDevVM(leaseID, slug)
	vm.SSHDest = "builder@" + vm.Name() + ".example"
	var state map[string]exeDevVM
	data, _ := os.ReadFile(inventory)
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state[vm.Name()] = vm
	data, _ = json.Marshal(state)
	if err := os.WriteFile(inventory, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := cloneConfig(t)
	cfg.WorkRoot, cfg.ExeDev.WorkRoot = workRoot, workRoot
	persistExeDevClaim(t, cfg, vm, leaseID, slug, repo)
	workdir := filepath.Join(workRoot, leaseID, filepath.Base(repo))
	if err := os.MkdirAll(workdir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workdir, "checkpoint-marker")
	if err := os.WriteFile(marker, []byte("saved workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: &stderr}
	run := func(args ...string) {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if err := app.Run(t.Context(), args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stderr.String())
		}
	}
	run("checkpoint", "create", "--provider", "exe-dev", "--id", leaseID, "--json")
	var record struct{ ID, Kind string }
	if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Kind != "workspace-archive" {
		t.Fatalf("record=%+v", record)
	}
	if err := os.WriteFile(marker, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	run("checkpoint", "restore", record.ID, "--provider", "exe-dev", "--id", leaseID)
	if got, err := os.ReadFile(marker); err != nil || string(got) != "saved workspace" {
		t.Fatalf("restored=%q err=%v", got, err)
	}
	for _, fromBase := range []bool{false, true} {
		args := []string{"checkpoint", "fork", record.ID, "--provider", "exe-dev", "--json"}
		if fromBase {
			args = append(args, "--exe-dev-from", "base")
		}
		run(args...)
		var fork struct {
			LeaseID string `json:"leaseId"`
			Workdir string `json:"workdir"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &fork); err != nil {
			t.Fatalf("fork=%s err=%v", stdout.String(), err)
		}
		if got, err := os.ReadFile(filepath.Join(fork.Workdir, "checkpoint-marker")); err != nil || string(got) != "saved workspace" {
			t.Fatalf("fork workspace=%q err=%v", got, err)
		}
		run("stop", "--provider", "exe-dev", fork.LeaseID)
	}
	t.Setenv("EXE_TEST_FAIL_RESTORE", "1")
	if err := app.Run(t.Context(), []string{"checkpoint", "fork", record.ID, "--provider", "exe-dev", "--exe-dev-from", "base"}); err == nil {
		t.Fatal("expected failed restore")
	}
	data, _ = os.ReadFile(inventory)
	// Decode into a fresh map: Unmarshal retains keys omitted by later JSON.
	state = nil
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state) != 2 || len(state["base"].Tags) != 1 || state["base"].Tags[0] != "source-private-tag" {
		t.Fatalf("leaked fork or mutated base: %s", data)
	}
	err = app.Run(context.Background(), []string{"checkpoint", "create", "--provider", "exe-dev", "--id", leaseID, "--mode", "native"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("native err=%v", err)
	}
}

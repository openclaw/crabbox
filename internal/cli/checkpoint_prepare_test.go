//go:build !windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeImagePreparationPreservesCurrentBootStatus(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("preparation fixture requires Python 3")
	}
	for _, tt := range []struct {
		name    string
		failure string
		cleaned bool
	}{
		{name: "clean succeeds", cleaned: true},
		{name: "clean fails after deleting cache", failure: "clean", cleaned: true},
		{name: "initialization still running", failure: "running"},
		{name: "cloud-init disabled", failure: "disabled"},
		{name: "persistent runtime filesystem", failure: "disk"},
		{name: "runtime inside cleaned cache", failure: "overlap"},
		{name: "runtime symlink inside cleaned cache", failure: "overlap-link"},
		{name: "cloud-init absent", failure: "absent"},
		{name: "second copy fails", failure: "copy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			runDir, dataDir, binDir := filepath.Join(root, "run", "cloud-init"), filepath.Join(root, "cloud", "data"), filepath.Join(root, "bin")
			if tt.failure == "overlap" {
				runDir = filepath.Join(root, "cloud", "runtime")
			}
			for _, dir := range []string{runDir, dataDir, binDir} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			configuredRun := runDir
			if tt.failure == "overlap-link" {
				configuredRun = filepath.Join(root, "cloud", "runtime-link")
				if err := os.Symlink(runDir, configuredRun); err != nil {
					t.Fatal(err)
				}
			}
			// cloud-init links current-boot status into the disk cache that clean removes.
			facts := map[string]string{"status.json": `{"v1":{"modules-final":{"finished":123}}}`, "result.json": `{"v1":{"errors":[]}}`}
			for file, contents := range facts {
				if err := os.WriteFile(filepath.Join(dataDir, file), []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dataDir, file), filepath.Join(runDir, file)); err != nil {
					t.Fatal(err)
				}
			}
			clean := `#!/bin/sh
set -eu
if [ "$1" = status ]; then
  case "$FIXTURE_FAILURE" in
    running|disabled) printf '{"status":"%s"}\n' "$FIXTURE_FAILURE";;
    *) printf '{"status":"done"}\n';;
  esac
  exit 0
fi
test "$1" = clean && test "$2" = --logs
: > "$FIXTURE_CLEANED"
rm -rf "$FIXTURE_DATA"
if [ "$FIXTURE_FAILURE" = clean ]; then exit 7; fi
`

			fixture := `import os, shutil, subprocess, sys, types
paths = types.SimpleNamespace(run_dir=os.environ["FIXTURE_RUN"], cloud_dir=os.path.dirname(os.environ["FIXTURE_DATA"]))
module = types.ModuleType("cloudinit.cmd.devel")
module.read_cfg_paths = lambda: paths
sys.modules["cloudinit.cmd.devel"] = module
run = subprocess.run
def fixture_run(args, **kwargs):
    if args[:4] == [sys.executable, "-I", "-m", "cloudinit.cmd.main"]:
        args = [os.environ["FIXTURE_CLI"]] + args[4:]
    return run(args, **kwargs)
subprocess.run = fixture_run
copy = shutil.copy2
def fixture_copy(source, target):
    if os.environ["FIXTURE_FAILURE"] == "copy" and source.name == "result.json":
        raise OSError("injected second-copy failure")
    return copy(source, target)
shutil.copy2 = fixture_copy
assert sys.argv[1:3] == ["-I", "-c"]
exec(compile(sys.argv[3], "remote-image-preparation", "exec"))
`
			if err := os.WriteFile(filepath.Join(root, "fixture.py"), []byte(fixture), 0o600); err != nil {
				t.Fatal(err)
			}
			for file, contents := range map[string]string{"python-wrapper": fmt.Sprintf("#!/bin/sh\nexec %s %s \"$@\"\n", shellQuote(python), shellQuote(filepath.Join(root, "fixture.py"))), "stat": "#!/bin/sh\nif [ \"$FIXTURE_FAILURE\" = disk ]; then printf 'ext4\\n'; else printf 'tmpfs\\n'; fi\n", "sudo": "#!/bin/sh\nexec \"$@\"\n", "cloud-init": clean, "sync": "#!/bin/sh\n: > \"$FIXTURE_SYNC\"\n"} {
				if tt.failure == "absent" && file == "cloud-init" {
					continue
				}
				if err := os.WriteFile(filepath.Join(binDir, file), []byte(contents), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			command := strings.ReplaceAll(remotePrepareNativeImageCommand(), "/usr/bin/python3", filepath.Join(binDir, "python-wrapper"))
			for attempt := 0; attempt < 2; attempt++ {
				cmd := exec.Command("sh", "-c", command)
				path := binDir + ":/usr/bin:/bin"
				if tt.failure == "absent" {
					path = binDir
				}
				cmd.Env = []string{"PATH=" + path, "FIXTURE_FAILURE=" + tt.failure, "FIXTURE_CLEANED=" + filepath.Join(root, "cleaned"), "FIXTURE_DATA=" + dataDir, "FIXTURE_RUN=" + configuredRun, "FIXTURE_CLI=" + filepath.Join(binDir, "cloud-init"), "FIXTURE_SYNC=" + filepath.Join(root, "synced")}
				out, err := cmd.CombinedOutput()
				if (err != nil) != (tt.failure != "" && tt.failure != "absent") {
					t.Fatalf("preparation error=%v, injected failure=%s: %s", err, tt.failure, out)
				}
				for file, want := range facts {
					path := filepath.Join(runDir, file)
					got, err := os.ReadFile(path)
					if err != nil || string(got) != want {
						t.Fatalf("current-boot %s=%q err=%v, want original facts %q", file, got, err, want)
					}
					info, err := os.Lstat(path)
					if err != nil || (tt.cleaned && !info.Mode().IsRegular()) {
						t.Fatalf("current-boot %s must survive removal of its old disk target: %v", file, err)
					}
					if tt.cleaned && info.Mode().Perm() != 0o644 {
						t.Fatalf("completion file mode=%v, want readable original mode", info.Mode())
					}
				}
				if _, err := os.Stat(dataDir); os.IsNotExist(err) != tt.cleaned {
					t.Fatalf("clone disk still contains cloud-init cache: %v", err)
				}
			}
			_, cleanErr := os.Stat(filepath.Join(root, "cleaned"))
			if os.IsNotExist(cleanErr) == tt.cleaned {
				t.Fatalf("clean invoked=%v, want=%v", cleanErr == nil, tt.cleaned)
			}
			_, syncErr := os.Stat(filepath.Join(root, "synced"))
			if (tt.failure != "" && tt.failure != "absent") != os.IsNotExist(syncErr) {
				t.Fatalf("sync must follow successful preparation only: failure=%s sync err=%v", tt.failure, syncErr)
			}
		})
	}
}

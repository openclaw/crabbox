package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativeCheckpointMachine0ConfiguredCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell executable fixture requires POSIX")
	}
	binary := filepath.Join(t.TempDir(), "crabbox")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/crabbox")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	for _, tc := range []struct {
		name          string
		envOverride   bool
		createdImage  bool
		imageID       string
		version       int
		metadataKey   string
		extraVersion  bool
		readFailure   bool
		invalidConfig bool
		wantError     string
	}{
		{name: "yaml_owned_image", createdImage: true},
		{name: "environment_overrides_yaml_owned_version", envOverride: true},
		{name: "mismatched_image", imageID: "img-other", wantError: "mismatched image identity"},
		{name: "missing_version", version: 3, wantError: "version 2 was not found"},
		{name: "mismatched_checkpoint", metadataKey: "crabbox_checkpoint", wantError: "mismatched crabbox_checkpoint metadata"},
		{name: "mismatched_lease", metadataKey: "crabbox_lease", wantError: "mismatched crabbox_lease metadata"},
		{name: "mismatched_source", metadataKey: "crabbox_source", wantError: "mismatched crabbox_source metadata"},
		{name: "later_version_protects_whole_image", createdImage: true, extraVersion: true, wantError: "no longer the only version"},
		{name: "unknown_resource", readFailure: true, wantError: "fixture image lookup failed"},
		{name: "invalid_config", invalidConfig: true, wantError: "parse config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home, state, configDir := filepath.Join(root, "home"), filepath.Join(root, "state"), filepath.Join(root, "config")
			emptyPath := filepath.Join(root, "empty-path")
			for _, dir := range []string{home, state, configDir, emptyPath} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			configPath := filepath.Join(configDir, "config.yaml")
			cliPath, logPath := filepath.Join(root, "custom-machine0"), filepath.Join(root, "commands.log")
			write := func(path, contents string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(contents), mode); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{
				"HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + configDir, "LOCALAPPDATA=" + configDir,
				"XDG_CONFIG_HOME=" + configDir, "XDG_STATE_HOME=" + state, "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
				"PATH=" + emptyPath, "CRABBOX_CONFIG=" + configPath,
			}
			yamlCLI := cliPath
			if tc.envOverride {
				yamlCLI = filepath.Join(root, "superseded-machine0")
				write(yamlCLI, "#!/bin/sh\nprintf '%s\\n' 'wrong executable selected' >&2\nexit 99\n", 0o700)
				env = append(env, "CRABBOX_MACHINE0_CLI="+cliPath)
			}
			config := fmt.Sprintf("provider: machine0\nmachine0:\n  cliPath: %q\n  pollInterval: 7s\n  createTimeout: 23m\n", yamlCLI)
			if tc.invalidConfig {
				config = "machine0: [\n"
			}
			write(configPath, config, 0o600)

			record := checkpointRecord{ID: "chk_config", Kind: checkpointKindMachine0, Provider: "machine0", LeaseID: "cbx_abcdef123456", CreatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)}
			record.Native.Provider, record.Native.Kind, record.Native.Direct = "machine0", checkpointKindMachine0, true
			record.Native.ImageID, record.Native.Resource, record.Native.Name = "img-owned@v2", "img-owned", "baseline"
			record.Native.Metadata = map[string]string{
				"machine0_image_name": "baseline", "machine0_image_id": "img-owned", "machine0_image_version": "2",
				"machine0_created_image": fmt.Sprint(tc.createdImage), "machine0_source_machine": "vm-owned",
				"crabbox_checkpoint": record.ID, "crabbox_lease": record.LeaseID,
			}
			metadata := map[string]string{"crabbox_checkpoint": record.ID, "crabbox_lease": record.LeaseID, "crabbox_source": "vm-owned"}
			if tc.metadataKey != "" {
				metadata[tc.metadataKey] = "other-owner"
			}
			imageID, version := "img-owned", 2
			if tc.imageID != "" {
				imageID = tc.imageID
			}
			if tc.version != 0 {
				version = tc.version
			}
			versions := []any{map[string]any{"version": version, "status": "DRAFT", "snapshotStatus": "READY", "metadata": metadata}}
			if tc.extraVersion {
				versions = append(versions, map[string]any{"version": 3, "status": "DRAFT", "snapshotStatus": "READY"})
			}
			response, err := json.Marshal(map[string]any{"image": map[string]string{"id": imageID, "name": "baseline"}, "versions": versions})
			if err != nil {
				t.Fatal(err)
			}
			readCommand := "printf '%s\\n' " + shellQuote(string(response))
			if tc.readFailure {
				readCommand = "printf '%s\\n' 'fixture image lookup failed' >&2; exit 5"
			}
			deleteCommand := "images versions rm baseline 2 --yes"
			if tc.createdImage {
				deleteCommand = "images rm baseline --yes"
			}
			// Only shell builtins run; no provider CLI, network tool, or credentials are available.
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\ncase \"$*\" in\n  'images get baseline --json') %s ;;\n  %s) exit 0 ;;\n  *) printf 'unexpected command: %%s\\n' \"$*\" >&2; exit 97 ;;\nesac\n", shellQuote(logPath), readCommand, shellQuote(deleteCommand))
			write(cliPath, script, 0o700)
			store := checkpointStore{root: filepath.Join(state, "crabbox", "checkpoints")}

			for _, operation := range []string{"delete", "prune"} {
				t.Run(operation, func(t *testing.T) {
					_, err := store.Create(record)
					if err != nil {
						t.Fatal(err)
					}
					paths, err := store.Paths(record.ID)
					if err != nil {
						t.Fatal(err)
					}
					before, err := os.ReadFile(paths.Meta)
					if err != nil {
						t.Fatal(err)
					}
					write(logPath, "", 0o600)
					for _, args := range [][]string{{"checkpoint", "inspect", record.ID, "--verify", "--json"}, {"checkpoint", "list", "--verify", "--json"}} {
						stdout, stderr, code := runDescribeTestBinary(binary, root, env, args...)
						if code != 0 {
							t.Fatalf("%v exit=%d stderr=%s", args, code, stderr)
						}
						var audits []checkpointAudit
						if args[1] == "inspect" {
							audits = make([]checkpointAudit, 1)
							err = json.Unmarshal(stdout, &audits[0])
						} else {
							err = json.Unmarshal(stdout, &audits)
						}
						if err != nil || len(audits) != 1 {
							t.Fatalf("%v audit JSON: %v: %s", args, err, stdout)
						}
						wantState, wantNext, wantError := "DRAFT", "fork_or_delete", ""
						if tc.wantError != "" && !tc.extraVersion {
							wantState, wantNext, wantError = "unknown", "check_runtime", tc.wantError
						}
						if got := audits[0]; got.LocalState != "metadata_available" || got.ProviderState != wantState || got.NextAction != wantNext || !strings.Contains(got.Error, wantError) || (wantError == "" && got.Error != "") {
							t.Errorf("%s audit state=%s next=%s error=%q; want state=%s next=%s error containing %q", args[1], got.ProviderState, got.NextAction, got.Error, wantState, wantNext, wantError)
						}
					}
					args := []string{"checkpoint", "delete", record.ID}
					if operation == "prune" {
						args = []string{"checkpoint", "prune", "--older-than", "24h", "--kind", "native"}
					}
					stdout, stderr, code := runDescribeTestBinary(binary, root, env, args...)
					if tc.wantError == "" {
						if code != 0 {
							t.Errorf("%s exit=%d stdout=%s stderr=%s", operation, code, stdout, stderr)
						}
						if _, _, err := store.Read(record.ID); !isCheckpointNotFound(err) {
							t.Errorf("owned resource deletion retained metadata: %v", err)
						}
					} else {
						if code == 0 || !strings.Contains(string(stderr), tc.wantError) {
							t.Errorf("%s exit=%d stderr=%s; want refusal containing %q", operation, code, stderr, tc.wantError)
						}
						after, err := os.ReadFile(paths.Meta)
						if err != nil || !bytes.Equal(before, after) {
							t.Errorf("uncertain deletion changed checkpoint metadata: %v", err)
						}
					}
					commands, err := os.ReadFile(logPath)
					if err != nil {
						t.Fatal(err)
					}
					wantCommands := strings.Repeat("images get baseline --json\n", 3)
					if tc.invalidConfig {
						wantCommands = ""
					} else if tc.wantError == "" {
						wantCommands += deleteCommand + "\n"
					}
					if string(commands) != wantCommands {
						t.Errorf("executable commands=%q, want %q", commands, wantCommands)
					}
					if err := store.Delete(record.ID); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

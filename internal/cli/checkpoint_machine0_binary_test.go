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
		name             string
		envOverride      bool
		createdImage     bool
		imageID          string
		imageName        string
		inventoryID      string
		version          int
		metadataKey      string
		extraVersion     bool
		readFailure      bool
		inventoryFailure bool
		authFailure      bool
		absentImage      bool
		postRemove       string
		removeFailure    bool
		invalidConfig    bool
		wantError        string
		deleteError      string
	}{
		{name: "yaml_owned_image", createdImage: true},
		{name: "environment_overrides_yaml_owned_version", envOverride: true, extraVersion: true},
		{name: "lost_version_remove_response", extraVersion: true, removeFailure: true},
		{name: "lost_image_remove_response", createdImage: true, removeFailure: true},
		{name: "mismatched_image", imageID: "img-other", wantError: "mismatched image identity"},
		{name: "mismatched_detail_name", imageName: "other-name", wantError: "mismatched image identity"},
		{name: "initial_inventory_replacement", inventoryID: "img-other", wantError: "image identity changed or is ambiguous"},
		{name: "missing_version", version: 3, wantError: "version 2 was not found"},
		{name: "whole_image_missing_version", createdImage: true, version: 3, wantError: "version 2 was not found"},
		{name: "mismatched_checkpoint", metadataKey: "crabbox_checkpoint", wantError: "mismatched crabbox_checkpoint metadata"},
		{name: "mismatched_lease", metadataKey: "crabbox_lease", wantError: "mismatched crabbox_lease metadata"},
		{name: "mismatched_source", metadataKey: "crabbox_source", wantError: "mismatched crabbox_source metadata"},
		{name: "later_version_protects_whole_image", createdImage: true, extraVersion: true, wantError: "no longer the only version"},
		{name: "unknown_resource", readFailure: true, wantError: "fixture image lookup failed"},
		{name: "unknown_inventory", inventoryFailure: true, wantError: "fixture inventory lookup failed"},
		{name: "bad_credentials", authFailure: true, wantError: "Machine0 authentication is required"},
		{name: "invalid_config", invalidConfig: true, wantError: "parse config"},
		{name: "whole_image_absent", createdImage: true, absentImage: true},
		{name: "whole_image_remove_leaves_empty_image", createdImage: true, postRemove: "empty", deleteError: "version 2 was not found"},
		{name: "whole_image_remove_omits_versions", createdImage: true, postRemove: "omitted", deleteError: "version 2 was not found"},
		{name: "version_remove_not_confirmed", postRemove: "unchanged", deleteError: "checkpoint source transition is pending"},
		{name: "failed_version_remove", postRemove: "unchanged", removeFailure: true, deleteError: "fixture remove failed"},
		{name: "post_remove_detail_unknown", postRemove: "lookup_failure", deleteError: "fixture image lookup failed"},
		{name: "post_remove_inventory_unknown", postRemove: "inventory_failure", deleteError: "fixture inventory lookup failed"},
		{name: "post_remove_inventory_replacement", postRemove: "inventory_replacement", deleteError: "image identity changed or is ambiguous"},
		{name: "post_remove_detail_replacement", postRemove: "detail_replacement", deleteError: "mismatched image identity"},
		{name: "post_remove_detail_renamed", postRemove: "detail_renamed", deleteError: "mismatched image identity"},
		{name: "post_remove_version_replaced", postRemove: "version_replacement", deleteError: "mismatched crabbox_source metadata"},
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
			removedPath := filepath.Join(root, "removed")
			inventoryPath, detailPath := filepath.Join(root, "inventory.json"), filepath.Join(root, "detail.json")
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
			imageID, imageName, version := "img-owned", "baseline", 2
			if tc.imageID != "" {
				imageID = tc.imageID
			}
			if tc.version != 0 {
				version = tc.version
			}
			if tc.imageName != "" {
				imageName = tc.imageName
			}
			versions := []any{map[string]any{"version": version, "status": "DRAFT", "snapshotStatus": "READY", "metadata": metadata}}
			if tc.extraVersion {
				versions = append(versions, map[string]any{"version": 3, "status": "DRAFT", "snapshotStatus": "READY"})
			}
			response, err := json.Marshal(map[string]any{"image": map[string]string{"id": imageID, "name": imageName}, "versions": versions})
			if err != nil {
				t.Fatal(err)
			}
			readCommand := "IFS= read -r response < " + shellQuote(detailPath) + "; printf '%s\\n' \"$response\""
			if tc.readFailure {
				readCommand = "printf '%s\\n' 'fixture image lookup failed' >&2; exit 5"
			}
			deleteCommand := "images versions rm baseline 2 --yes"
			if tc.createdImage {
				deleteCommand = "images rm baseline --yes"
			}
			inventory := `[{"id":"img-owned","name":"baseline","status":"READY"}]`
			if tc.inventoryID != "" {
				inventory = strings.ReplaceAll(inventory, "img-owned", tc.inventoryID)
			}
			if tc.absentImage {
				inventory = `[]`
			}
			postInventory := inventory
			if tc.createdImage && tc.postRemove == "" {
				postInventory = `[]`
			}
			survivors := []any{}
			for _, v := range versions {
				if v.(map[string]any)["version"] != 2 {
					survivors = append(survivors, v)
				}
			}
			postDetail := map[string]any{"image": map[string]string{"id": "img-owned", "name": "baseline"}, "versions": survivors}
			switch tc.postRemove {
			case "unchanged":
				postDetail["versions"] = versions
			case "omitted":
				delete(postDetail, "versions")
			case "inventory_replacement":
				postInventory = strings.ReplaceAll(postInventory, "img-owned", "img-other")
			case "detail_replacement":
				postDetail["image"].(map[string]string)["id"] = "img-other"
			case "detail_renamed":
				postDetail["image"].(map[string]string)["name"] = "other-name"
			case "version_replacement":
				postDetail["versions"] = []any{map[string]any{"version": 2, "status": "DRAFT", "snapshotStatus": "READY", "metadata": map[string]string{"crabbox_checkpoint": record.ID, "crabbox_lease": record.LeaseID, "crabbox_source": "other-owner"}}}
			}
			postResponse, err := json.Marshal(postDetail)
			if err != nil {
				t.Fatal(err)
			}
			inventoryCommand := "IFS= read -r response < " + shellQuote(inventoryPath) + "; printf '%s\\n' \"$response\""
			if tc.inventoryFailure {
				inventoryCommand = "printf '%s\\n' 'fixture inventory lookup failed' >&2; exit 5"
			}
			if tc.authFailure {
				inventoryCommand = "printf '%s\\n' 'unauthorized fixture' >&2; exit 3"
			}
			postReadCommand, postInventoryCommand := readCommand, inventoryCommand
			if tc.postRemove == "lookup_failure" {
				postReadCommand = "printf '%s\\n' 'fixture image lookup failed' >&2; exit 5"
			}
			if tc.postRemove == "inventory_failure" {
				postInventoryCommand = "printf '%s\\n' 'fixture inventory lookup failed' >&2; exit 5"
			}
			removeResult := "exit 0"
			if tc.removeFailure {
				removeResult = "printf '%s\\n' 'fixture remove failed' >&2; exit 5"
			}
			// Only shell builtins run; no provider CLI, network tool, or credentials are available.
			script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
case "$*" in
  'images ls --json')
    if [ -f %s ]; then %s; else %s; fi ;;
  'images get baseline --json')
    if [ -f %s ]; then %s; else %s; fi ;;
  %s)
    printf '%%s\n' %s > %s
    printf '%%s\n' %s > %s
    : > %s
    %s ;;
  *) printf 'unexpected command: %%s\n' "$*" >&2; exit 97 ;;
esac
`, shellQuote(logPath), shellQuote(removedPath), postInventoryCommand, inventoryCommand, shellQuote(removedPath), postReadCommand, readCommand, shellQuote(deleteCommand), shellQuote(postInventory), shellQuote(inventoryPath), shellQuote(string(postResponse)), shellQuote(detailPath), shellQuote(removedPath), removeResult)
			write(cliPath, script, 0o700)
			store := checkpointStore{root: filepath.Join(state, "crabbox", "checkpoints")}

			for _, operation := range []string{"delete", "prune"} {
				t.Run(operation, func(t *testing.T) {
					if err := os.Remove(removedPath); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					write(inventoryPath, inventory+"\n", 0o600)
					write(detailPath, string(response)+"\n", 0o600)
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
						if tc.absentImage {
							wantState, wantNext = "missing", "delete_local"
						}
						if tc.wantError != "" && !tc.extraVersion {
							wantState, wantNext, wantError = "unknown", "check_runtime", tc.wantError
						}
						if got := audits[0]; got.LocalState != "metadata_available" || got.ProviderState != wantState || got.NextAction != wantNext || !strings.Contains(got.Error, wantError) || (wantError == "" && got.Error != "") {
							t.Errorf("%s audit state=%s next=%s error=%q; want state=%s next=%s error containing %q", args[1], got.ProviderState, got.NextAction, got.Error, wantState, wantNext, wantError)
						}
					}
					if after, err := os.ReadFile(paths.Meta); err != nil || !bytes.Equal(before, after) {
						t.Fatalf("inspection changed checkpoint metadata: %v", err)
					}
					args := []string{"checkpoint", "delete", record.ID}
					if operation == "prune" {
						args = []string{"checkpoint", "prune", "--older-than", "24h", "--kind", "native"}
					}
					stdout, stderr, code := runDescribeTestBinary(binary, root, env, args...)
					wantError := tc.wantError
					if tc.deleteError != "" {
						wantError = tc.deleteError
					}
					if wantError == "" {
						if code != 0 {
							t.Errorf("%s exit=%d stdout=%s stderr=%s", operation, code, stdout, stderr)
						}
						if _, _, err := store.Read(record.ID); !isCheckpointNotFound(err) {
							t.Errorf("owned resource deletion retained metadata: %v", err)
						}
					} else {
						if code == 0 || !strings.Contains(string(stderr), wantError) {
							t.Errorf("%s exit=%d stderr=%s; want refusal containing %q", operation, code, stderr, wantError)
						}
						if wantError == "version 2 was not found" && (code != 4 || !strings.Contains(string(stderr), "Machine0 image baseline version 2 was not found")) {
							t.Errorf("missing version lost exit/message contract: exit=%d stderr=%s", code, stderr)
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
					wantCommands := strings.Repeat("images ls --json\nimages get baseline --json\n", 3)
					if tc.inventoryID != "" || tc.inventoryFailure || tc.authFailure || tc.absentImage {
						wantCommands = strings.Repeat("images ls --json\n", 3)
					}
					if tc.invalidConfig {
						wantCommands = ""
					} else if tc.wantError == "" && !tc.absentImage {
						wantCommands += deleteCommand + "\nimages ls --json\n"
						if (!tc.createdImage || tc.postRemove != "") && tc.postRemove != "inventory_failure" && tc.postRemove != "inventory_replacement" {
							wantCommands += "images get baseline --json\n"
						}
					}
					if string(commands) != wantCommands {
						t.Errorf("executable commands=%q, want %q", commands, wantCommands)
					}
					wantInventory, wantDetail := inventory, string(response)
					if tc.wantError == "" && !tc.absentImage {
						wantInventory, wantDetail = postInventory, string(postResponse)
					}
					for path, want := range map[string]string{inventoryPath: wantInventory, detailPath: wantDetail} {
						if got, err := os.ReadFile(path); err != nil || string(got) != want+"\n" {
							t.Errorf("provider state or sibling versions changed: %s=%s want=%s err=%v", path, got, want, err)
						}
					}
					if err := store.Delete(record.ID); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

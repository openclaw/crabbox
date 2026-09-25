package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestWarmupGCPKeptIdleExpiryGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux guest shell fixture requires POSIX paths")
	}
	for _, executable := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(executable); err != nil {
			t.Skipf("%s unavailable", executable)
		}
	}
	var document struct {
		Files []struct{ Path, Content string } `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte("write_files:\n"+cloudInitGCPExpiryGuardFiles()), &document); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tc := range []struct {
		name, state, expiry, owner string
		wantDelete                 bool
	}{
		{"kept idle", "ready", LeaseLabelTime(now.Add(-time.Minute)), "true", true},
		{"kept future", "ready", LeaseLabelTime(now.Add(time.Hour)), "true", false},
		{"released future", "released", LeaseLabelTime(now.Add(time.Hour)), "true", false},
		{"missing expiry", "released", "", "true", false},
		{"invalid expiry", "ready", "invalid", "true", false},
		{"overflow expiry", "ready", "999999999999999999999", "true", false},
		{"running grace", "running", LeaseLabelTime(now.Add(-time.Hour)), "true", false},
		{"stale running", "running", LeaseLabelTime(now.Add(-13 * time.Hour)), "true", true},
		{"foreign instance", "ready", LeaseLabelTime(now.Add(-time.Hour)), "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			deleted := filepath.Join(dir, "deleted")
			instance, err := json.Marshal(map[string]any{"labels": map[string]string{
				"crabbox": tc.owner, "provider": "gcp", "lease": "cbx_123456abcdef", "keep": "true", "state": tc.state, "expires_at": tc.expiry,
			}})
			if err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string]string{
				"guard":  document.Files[0].Content,
				"logger": "#!/bin/sh\nexit 0\n",
				"curl": `#!/usr/bin/env bash
if [[ "$*" == *"-X DELETE"* ]]; then
  touch "$GUARD_DELETED"
  exit 0
fi
case "${!#}" in
  */project/project-id) echo fixture-project ;;
  */instance/name) echo fixture-instance ;;
  */instance/zone) echo zones/fixture-zone ;;
  */instance/service-accounts/default/token) echo '{"access_token":"synthetic"}' ;;
  */instances/fixture-instance) printf '%s' "$GUARD_INSTANCE" ;;
  *) exit 1 ;;
esac
`,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", filepath.Join(dir, "guard"))
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "GUARD_INSTANCE="+string(instance), "GUARD_DELETED="+deleted)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("guard: %v\n%s", err, output)
			}
			_, err = os.Stat(deleted)
			if got := err == nil; got != tc.wantDelete {
				t.Fatalf("deleted=%t, want %t", got, tc.wantDelete)
			}
		})
	}
}

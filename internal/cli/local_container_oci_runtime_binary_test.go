package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalContainerOCIRuntimeConfigBuiltBinary(t *testing.T) {
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, repo, env, want string
	}{
		{name: "user", want: "user-runtime"},
		{name: "repository", repo: "repo-runtime", want: "repo-runtime"},
		{name: "environment", repo: "repo-runtime", env: "env-runtime", want: "env-runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
				t.Setenv(key, root)
			}
			configDir, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(configDir, "crabbox", "config.yaml")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte("provider: local-container\nlocalContainer:\n  ociRuntime: user-runtime\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.repo != "" {
				if err := os.WriteFile(filepath.Join(root, "crabbox.yaml"), []byte("localContainer:\n  ociRuntime: "+tc.repo+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{"HOME=" + root, "USERPROFILE=" + root, "APPDATA=" + root, "XDG_CONFIG_HOME=" + root, "XDG_STATE_HOME=" + root, "CRABBOX_LOCAL_CONTAINER_OCI_RUNTIME=" + tc.env}
			stdout, stderr, code := runDescribeTestBinary(binary, root, env, "config", "show", "--json")
			if code != 0 {
				t.Fatalf("config show exit=%d: %s", code, stderr)
			}
			var view struct {
				LocalContainer struct {
					OCIRuntime string `json:"ociRuntime"`
				} `json:"localContainer"`
			}
			if err := json.Unmarshal(stdout, &view); err != nil {
				t.Fatal(err)
			}
			if view.LocalContainer.OCIRuntime != tc.want {
				t.Fatalf("OCI runtime=%q, want %q", view.LocalContainer.OCIRuntime, tc.want)
			}
		})
	}
	root := t.TempDir()
	stdout, stderr, code := runDescribeTestBinary(binary, root, []string{"HOME=" + root}, "run", "--help")
	if code != 0 || !strings.Contains(string(stdout)+string(stderr), "-local-container-oci-runtime") {
		t.Fatalf("run does not advertise runtime selection: exit=%d stderr=%s", code, stderr)
	}
}

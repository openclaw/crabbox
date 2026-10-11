package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOSSelectorLeasePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, config, envOS, envTarget string
		flags                          []string
		wantOS, wantTarget, wantMode   string
		wantError                      string
	}{
		{name: "env Windows with target flag", envOS: "windows-server:2025", flags: []string{"--target", "windows"}, wantOS: "windows-server:2025", wantTarget: targetWindows},
		{name: "config Windows with target flag", config: "os: windows-server:2022\n", flags: []string{"--target", "windows"}, wantOS: "windows-server:2022", wantTarget: targetWindows},
		{name: "provider and target flags override config", config: "provider: azure\nos: windows-server:2025\n", flags: []string{"--target", "windows"}, wantOS: "windows-server:2025", wantTarget: targetWindows},
		{name: "env target overrides config", config: "target: linux\nos: windows-server:2025\n", envTarget: "windows", wantOS: "windows-server:2025", wantTarget: targetWindows},
		{name: "OS flag overrides incompatible env", envOS: "windows-server:2025", flags: []string{"--os", "ubuntu:26.04"}, wantOS: "ubuntu:26.04", wantTarget: targetLinux},
		{name: "OS env overrides Windows config", config: "target: windows\nos: windows-server:2025\n", envOS: "ubuntu:26.04", wantOS: "ubuntu:26.04", wantTarget: targetWindows},
		{name: "OS flag overrides Windows config", config: "target: windows\nos: windows-server:2025\n", flags: []string{"--os", "ubuntu:26.04"}, wantOS: "ubuntu:26.04", wantTarget: targetWindows},
		{name: "WSL2 flag", envOS: "windows-server:2025", flags: []string{"--target", "windows", "--windows-mode", "wsl2", "--type", "m8i.large"}, wantOS: "windows-server:2025", wantTarget: targetWindows, wantMode: windowsModeWSL2},
		{name: "native mode overrides config", config: "target: windows\nos: windows-server:2025\nwindows:\n  mode: wsl2\n", flags: []string{"--windows-mode", "normal"}, wantOS: "windows-server:2025", wantTarget: targetWindows},
		{name: "reject Windows OS flag on Linux", flags: []string{"--os", "windows-server:2025", "--target", "linux"}, wantError: "os windows-server:2025 requires provider=aws target=windows"},
		{name: "reject final Linux override", config: "target: windows\nos: windows-server:2025\n", flags: []string{"--target", "linux"}, wantError: "os windows-server:2025 requires provider=aws target=windows"},
		{name: "reject final provider override", config: "target: windows\nos: windows-server:2025\n", flags: []string{"--provider", "azure"}, wantError: "os windows-server:2025 requires provider=aws target=windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			config := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(config, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", config)
			t.Setenv("CRABBOX_OS", tc.envOS)
			t.Setenv("CRABBOX_TARGET", tc.envTarget)
			fs := newFlagSet("warmup", io.Discard)
			flags := registerLeaseCreateFlags(fs, defaultConfig())
			args := append([]string{"--provider", "aws", "--class", "standard", "--type", "m7i.large"}, tc.flags...)
			if err := parseFlags(fs, args); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig()
			if err != nil {
				t.Fatalf("config loading rejected values before flags: %v", err)
			}
			err = applyLeaseCreateFlags(&cfg, fs, flags)
			if tc.wantError != "" {
				if err == nil || err.Error() != tc.wantError {
					t.Fatalf("error=%v, want %s", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.OSImage != tc.wantOS || cfg.TargetOS != tc.wantTarget || cfg.WindowsMode != blank(tc.wantMode, windowsModeNormal) || !cfg.osImageExplicit {
				t.Fatalf("resolved os=%s target=%s mode=%s explicit=%t", cfg.OSImage, cfg.TargetOS, cfg.WindowsMode, cfg.osImageExplicit)
			}
		})
	}
}

func TestOSSelectorMintBuiltBinary(t *testing.T) {
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"warmup", "prewarm", "run"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			requests := make(chan map[string]any, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if command == "run" && strings.HasPrefix(r.URL.Path, "/v1/runs/") &&
					(r.Method == http.MethodPut || r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events")) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if strings.HasSuffix(r.URL.Path, "/events") {
						_ = json.NewEncoder(w).Encode(map[string]any{"event": body})
					} else {
						_ = json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{
							"id": strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "command": body["command"], "state": "running", "phase": "starting",
						}})
					}
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/leases" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body
				// Refuse allocation after recording the final wire values.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"fixture stopped before provisioning"}`)
			}))
			defer server.Close()
			args := []string{command, "--provider", "aws", "--target", "windows", "--class", "standard", "--type", "m7i.large", "--windows-mode", "normal", "--market", "on-demand", "--ttl", "20m", "--idle-timeout", "10m", "--timing-json"}
			if command == "prewarm" {
				args = append(args, "--no-hydrate")
			}
			if command == "run" {
				args = append(args, "--no-sync", "--no-hydrate", "--", "cmd", "/c", "ver")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Dir = root
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + root, "USER=fixture",
				"USERPROFILE=" + root, "APPDATA=" + root, "LOCALAPPDATA=" + root,
				"XDG_CONFIG_HOME=" + root, "XDG_STATE_HOME=" + root,
				"CRABBOX_CONFIG=" + filepath.Join(root, "missing.yaml"),
				"CRABBOX_COORDINATOR=" + server.URL, "CRABBOX_COORDINATOR_TOKEN=fixture-token",
				"CRABBOX_AWS_REGION=eu-west-1", "CRABBOX_AWS_STOCK_IMAGE=1", "CRABBOX_AWS_ROOT_GB=0",
				"CRABBOX_OS=windows-server:2025", "CRABBOX_AWS_SSH_CIDRS=192.0.2.1/32",
			}
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() == 2 || !strings.Contains(string(output), "fixture stopped before provisioning") {
				t.Fatalf("exit=%v output=%s", err, output)
			}
			select {
			case body := <-requests:
				for key, want := range map[string]any{"provider": "aws", "target": "windows", "windowsMode": "normal", "os": "windows-server:2025", "awsRegion": "eu-west-1", "awsUseStockImage": true} {
					if body[key] != want {
						t.Errorf("request %s=%v, want %v", key, body[key], want)
					}
				}
			default:
				t.Fatal("CLI never reached fake coordinator")
			}
		})
	}
}

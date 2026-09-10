package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestWindowsNativeBaselineBootstrap(t *testing.T) {
	for name, render := range map[string]func(Config, string) string{
		"aws":            windowsBootstrapPowerShell,
		"azure":          azureWindowsBootstrapPowerShell,
		"azure-snapshot": azureWindowsSnapshotRehydratePowerShell,
	} {
		for _, mode := range []string{"", windowsModeNormal, windowsModeWSL2} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				cfg := baseConfig()
				cfg.TargetOS, cfg.WindowsMode = targetWindows, mode
				script := render(cfg, "ssh-ed25519 fixture")
				for _, marker := range []string{"\nEnsure-CrabboxNode\n", "Start-CrabboxDetachedProcess.ps1"} {
					if mode == windowsModeWSL2 {
						if strings.Contains(script, marker) {
							t.Fatalf("native baseline leaked into WSL2: %s", marker)
						}
					} else if strings.Count(script, marker) != 1 {
						t.Fatalf("expected exactly one %s", marker)
					}
				}
				if mode == windowsModeWSL2 {
					return
				}
				for _, want := range []string{
					`$nodeVersion = "24.19.0"`, `$nodeArch = "x64"`, `$nodeArch = "arm64"`,
					"57f71ab3652e797d84acddc79c81cc9ff1c6ddb2a1974cdb83f00fee9bff4c73",
					"8502f4a50b458d4cc38ed8f2001556c2cd239d464920f74017926ccb1e1c157f",
					`if (Test-CrabboxNode) { return }`, `SetEnvironmentVariable("Path", $machinePath, "Machine")`,
					`if ($LASTEXITCODE -ne 0)`, `CREATE_BREAKAWAY_FROM_JOB | CREATE_NEW_CONSOLE`,
					`IntPtr.Zero, IntPtr.Zero, false`, `CloseHandle(process.thread)`, `CloseHandle(process.process)`,
				} {
					if !strings.Contains(script, want) {
						t.Fatalf("missing native baseline contract: %s", want)
					}
				}
				verify := strings.Index(script, "Assert-CrabboxFileSHA256 $zip $nodeSHA256")
				expand := strings.Index(script, "Expand-Archive -LiteralPath $zip -DestinationPath $stage")
				ready := strings.Index(script, "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath")
				install := strings.Index(script, "\nEnsure-CrabboxNode\n")
				if verify < 0 || expand < verify || (ready >= 0 && ready < install) {
					t.Fatal("verification, install and readiness are out of order")
				}
			})
		}
	}
}

func TestWindowsNativeReadinessRequiresNodeAndNpm(t *testing.T) {
	script := decodePowerShellCommand(t, sshReadyCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}))
	for _, want := range []string{"node --version", "npm.cmd --version", `throw "node readiness failed"`, `throw "npm readiness failed"`} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing readiness requirement %s", want)
		}
	}
}

func TestWindowsDevtoolsNodeBaseline(t *testing.T) {
	script, err := os.ReadFile("../../scripts/install-windows-developer-tools.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`$DefaultNodeVersion = "24.19.0"`, "f0f66c2a80c08a30a5ab5179ee9ea9e45f9b46289436a8cc87ff833b852db351", "CRABBOX_WINDOWS_NODE_VERSION", "CRABBOX_WINDOWS_NODE_SHA256"} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("missing devtools baseline %s", want)
		}
	}
}

func TestStatusWorkroot(t *testing.T) {
	for _, tc := range []struct{ name, target, mode, root, want string }{
		{"native", targetWindows, windowsModeNormal, "", `C:\crabbox`},
		{"native-custom", targetWindows, windowsModeNormal, `D:\build`, `D:\build`},
		{"wsl", targetWindows, windowsModeWSL2, "", defaultPOSIXWorkRoot},
		{"linux", targetLinux, "", "", defaultPOSIXWorkRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = "aws"
			cfg.Network = NetworkPublic
			view, err := statusViewFromLeaseTarget(t.Context(), cfg, LeaseTarget{Server: Server{Status: "released", Labels: map[string]string{"target": tc.target, "windows_mode": tc.mode, "work_root": tc.root}}, SSH: SSHTarget{TargetOS: tc.target, WindowsMode: tc.mode}})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["workroot"] != tc.want {
				t.Fatalf("workroot = %v, want %s", fields["workroot"], tc.want)
			}
		})
	}
}

func TestCoordinatorStatusWorkroot(t *testing.T) {
	for _, tc := range []struct{ name, mode, root, want string }{
		{"native-default", windowsModeNormal, "", `C:\crabbox`},
		{"native-recorded", windowsModeNormal, `D:\build`, `D:\build`},
		{"wsl-default", windowsModeWSL2, "", defaultPOSIXWorkRoot},
		{"wsl-recorded", windowsModeWSL2, "/workspace", "/workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_0123456789ab" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_0123456789ab", Provider: "aws", TargetOS: targetWindows, WindowsMode: tc.mode, WorkRoot: tc.root, State: "released"}})
			}))
			defer server.Close()
			cfg := baseConfig()
			cfg.Provider, cfg.Coordinator, cfg.CoordToken = "aws", server.URL, "fixture-token"
			backend := &coordinatorLeaseBackend{cfg: cfg, coord: mustNewCoordinatorClient(t, cfg)}
			view, err := backend.Status(t.Context(), StatusRequest{ID: "cbx_0123456789ab", AuthoritativeProviderMetadata: true})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["workroot"] != tc.want {
				t.Fatalf("workroot = %v, want %s", fields["workroot"], tc.want)
			}
		})
	}
}

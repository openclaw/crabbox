//go:build darwin && parallels_live

package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The operator must supply an exclusively owned, running Windows guest with
// Tools. This test never starts, stops, clones, or provisions a VM/account.
func TestParallelsWindowsUploadLive(t *testing.T) {
	if os.Getenv("CRABBOX_PARALLELS_WINDOWS_UPLOAD_LIVE") != "1" {
		t.Skip("requires parallels_live tag and CRABBOX_PARALLELS_WINDOWS_UPLOAD_LIVE=1")
	}
	vm := os.Getenv("CRABBOX_PARALLELS_WINDOWS_UPLOAD_VM")
	if !regexp.MustCompile(`^\{?[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\}?$`).MatchString(vm) {
		t.Fatal("live proof requires an explicit guest UUID")
	}
	cfg := BaseConfig()
	cfg.TargetOS = targetWindows
	cfg.Parallels.StartupTimeout = 2 * time.Minute
	for _, mode := range []string{"applied-write-result-loss", "execution-result-loss"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if err := NewParallelsClient(cfg, nil).WaitForGuestExec(ctx, vm, cfg, time.Minute); err != nil {
				t.Fatal(err)
			}
			name := "upload-live-" + rand.Text()
			base := `C:\ProgramData\crabbox\` + name
			realPowerShell := func(ctx context.Context, script string) (LocalCommandResult, error) {
				args := append([]string{"exec", vm}, strings.Fields(PowershellCommand(script))...)
				return (execCommandRunner{}).Run(ctx, LocalCommandRequest{Name: "prlctl", Args: args})
			}
			defer func() {
				cleanupCtx, stop := context.WithTimeout(context.Background(), time.Minute)
				defer stop()
				result, err := realPowerShell(cleanupCtx, fmt.Sprintf(`$ErrorActionPreference='Stop'
$files=@(%s,%s,%s,%s,%s)
foreach($f in $files){if(Test-Path -LiteralPath $f){Remove-Item -LiteralPath $f -Force}}
@($files | Where-Object {Test-Path -LiteralPath $_}).Count`, psQuote(base+".ps1"), psQuote(base+".ps1.b64"), psQuote(base+".ps1.result"), psQuote(base+".counter"), psQuote(base+".marker")))
				if err != nil || strings.TrimSpace(result.Stdout) != "0" {
					t.Errorf("cleanup unique probe files: %v %s", err, result.Stdout)
				}
			}()
			marker := "UTF-8 marker: π 雪 🦀\n"
			payload := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$counter = %s
$n = 0
if (Test-Path -LiteralPath $counter) { $n = [int][IO.File]::ReadAllText($counter) }
[IO.File]::WriteAllText($counter, [string]($n + 1), [Text.UTF8Encoding]::new($false))
[IO.File]::WriteAllBytes(%s, [Convert]::FromBase64String('%s'))
`, psQuote(base+".counter"), psQuote(base+".marker"), base64.StdEncoding.EncodeToString([]byte(marker)))
			payload += strings.Repeat("# UTF-8 payload π 雪 🦀\n", 40)
			encodedLen := len(base64.StdEncoding.EncodeToString([]byte(payload)))
			if _, err := realPowerShell(ctx, fmt.Sprintf(`New-Item -ItemType Directory -Force -Path 'C:\ProgramData\crabbox' | Out-Null
[IO.File]::WriteAllText(%s, ('Z' * %d), [Text.Encoding]::ASCII)`, psQuote(base+".ps1.b64"), encodedLen+800)); err != nil {
				t.Fatal(err)
			}
			writeLost, probeLost, executionLost := false, false, false
			dispatches := 0
			runner := parallelsExecReadyRunner(func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				command := decodePowerShellCommand(t, strings.Join(req.Args[2:], " "))
				final := strings.Contains(command, "& powershell.exe")
				if final {
					dispatches++
				}
				result, err := (execCommandRunner{}).Run(ctx, req)
				if err != nil {
					return result, err
				}
				lose := false
				if mode == "applied-write-result-loss" {
					if strings.Contains(command, "$file.Position = 400") && !writeLost {
						chunk := windowsUploadBytes.FindStringSubmatch(command)[1]
						verified, verifyErr := realPowerShell(ctx, fmt.Sprintf(`[IO.File]::ReadAllText(%s).Substring(400, %d) -ceq %s`, psQuote(base+".ps1.b64"), len(chunk), psQuote(chunk)))
						if verifyErr != nil || !strings.EqualFold(strings.TrimSpace(verified.Stdout), "True") {
							t.Fatalf("guest write verification failed: %v %s", verifyErr, verified.Stdout)
						}
						writeLost, lose = true, true
					}
					if strings.Contains(command, `"ok" | Out-Null`) && !probeLost {
						probeLost, lose = true, true
					}
				} else if final {
					executionLost, lose = true, true
				}
				if lose {
					return LocalCommandResult{ExitCode: 255, Stderr: "PrlJob_GetResult: Invalid argument"}, errors.New("controlled result loss after real prlctl success")
				}
				return result, nil
			})
			if err := NewParallelsClient(cfg, runner).runWindowsPowerShellFile(ctx, vm, name, payload); err != nil {
				t.Fatal(err)
			}
			result, err := realPowerShell(ctx, fmt.Sprintf(`$ErrorActionPreference='Stop'
$p=%s
@{script=(Get-FileHash -Algorithm SHA256 -LiteralPath ($p+'.ps1')).Hash.ToLowerInvariant();marker=(Get-FileHash -Algorithm SHA256 -LiteralPath ($p+'.marker')).Hash.ToLowerInvariant();counter=[int][IO.File]::ReadAllText($p+'.counter')} | ConvertTo-Json -Compress`, psQuote(base)))
			var got struct {
				Script, Marker string
				Counter        int
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(result.Stdout), &got); err != nil {
				t.Fatal(err)
			}
			if got.Script != fmt.Sprintf("%x", sha256.Sum256([]byte(payload))) || got.Marker != fmt.Sprintf("%x", sha256.Sum256([]byte(marker))) || got.Counter != 1 || dispatches != 1 {
				t.Fatalf("payload mismatch or replay: %+v dispatches=%d", got, dispatches)
			}
			if mode == "applied-write-result-loss" && (!writeLost || !probeLost) || mode == "execution-result-loss" && !executionLost {
				t.Fatal("required controlled fault was not injected")
			}
		})
	}
}

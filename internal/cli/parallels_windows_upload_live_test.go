package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This opt-in test never starts, stops, clones, or bootstraps an account. The
// operator supplies an exclusively owned, running Windows guest with Tools.
func TestParallelsWindowsUploadLive(t *testing.T) {
	if os.Getenv("CRABBOX_PARALLELS_WINDOWS_UPLOAD_LIVE") != "1" {
		t.Skip("set CRABBOX_PARALLELS_WINDOWS_UPLOAD_LIVE=1 for real prlctl/PowerShell proof")
	}
	vm := os.Getenv("CRABBOX_PARALLELS_WINDOWS_UPLOAD_VM")
	out := os.Getenv("CRABBOX_PARALLELS_WINDOWS_UPLOAD_PROOF_DIR")
	if runtime.GOOS != "darwin" || !regexp.MustCompile(`^\{?[0-9a-fA-F-]{36}\}?$`).MatchString(vm) || !filepath.IsAbs(out) {
		t.Fatal("live proof requires macOS, an explicit guest UUID, and an absolute proof directory")
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	readyCfg := BaseConfig()
	readyCfg.TargetOS = targetWindows
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := NewParallelsClient(readyCfg, nil).WaitForGuestExec(readyCtx, vm, readyCfg, 2*time.Minute)
	readyCancel()
	if err != nil {
		t.Fatalf("real guest readiness preflight: %v", err)
	}
	for _, mode := range []string{"applied-write-result-loss", "execution-result-loss"} {
		t.Run(mode, func(t *testing.T) {
			var nonce [8]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			name := "upload-live-" + hex.EncodeToString(nonce[:])
			base := `C:\ProgramData\crabbox\` + name
			marker := "UTF-8 marker: π 雪 🦀\n"
			payload := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$counter = %s
$n = 0
if (Test-Path -LiteralPath $counter) { $n = [int][IO.File]::ReadAllText($counter) }
[IO.File]::WriteAllText($counter, [string]($n + 1), [Text.UTF8Encoding]::new($false))
$bytes = [Convert]::FromBase64String('%s')
[IO.File]::WriteAllBytes(%s, $bytes)
`, psQuote(base+".counter"), base64.StdEncoding.EncodeToString([]byte(marker)), psQuote(base+".marker"))
			// Non-ASCII comments exercise the staged UTF-8 bytes without relying on
			// Windows PowerShell's interpretation of UTF-8 files without a BOM.
			payload += strings.Repeat("# UTF-8 payload π 雪 🦀\n", 40)
			cfg := BaseConfig()
			cfg.TargetOS = targetWindows
			cfg.Parallels.StartupTimeout = 2 * time.Minute
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			runner := &windowsUploadLiveRunner{t: t, vm: vm, base: base, mode: mode}
			report := windowsUploadLiveReport{
				Mode: mode, FaultModel: "controlled result loss after successful real prlctl execution",
				Source:         os.Getenv("CRABBOX_PARALLELS_WINDOWS_UPLOAD_SOURCE"),
				ExpectedSHA256: liveUploadHash(payload), ExpectedMarkerSHA256: liveUploadHash(marker),
				EncodedBytes: len(base64.StdEncoding.EncodeToString([]byte(payload))),
			}
			defer func() {
				inspectCtx, stop := context.WithTimeout(context.Background(), time.Minute)
				defer stop()
				snapshot, err := runner.inspect(inspectCtx)
				if err != nil {
					t.Errorf("inspect guest proof: %v", err)
				}
				report.Guest = snapshot
				remaining, err := runner.cleanup(inspectCtx)
				if err != nil || remaining != 0 {
					t.Errorf("cleanup unique probe files: remaining=%d err=%v", remaining, err)
				}
				report.CleanupRemaining = remaining
				report.CleanupComplete = err == nil && remaining == 0
				report.Events = runner.events
				report.Passed = !t.Failed()
				data, err := json.MarshalIndent(report, "", "  ")
				if err == nil {
					err = os.WriteFile(filepath.Join(out, mode+".json"), append(data, '\n'), 0o600)
				}
				if err != nil {
					t.Errorf("retain proof: %v", err)
				}
				t.Logf("real-transport proof mode=%s passed=%t cleanup=%t", mode, report.Passed, report.CleanupComplete)
			}()
			// A longer stale tail must not alter the decoded prefix.
			_, err := runner.realPowerShell(ctx, fmt.Sprintf(`$ErrorActionPreference='Stop'
New-Item -ItemType Directory -Force -Path 'C:\ProgramData\crabbox' | Out-Null
[IO.File]::WriteAllText(%s, ('Z' * %d), [Text.Encoding]::ASCII)`, psQuote(base+".ps1.b64"), report.EncodedBytes+800))
			if err != nil {
				t.Fatal(err)
			}
			err = NewParallelsClient(cfg, runner).runWindowsPowerShellFile(ctx, vm, name, payload)
			report.UploaderReturnedError = err != nil
			if mode == "applied-write-result-loss" {
				if err != nil {
					t.Errorf("uploader did not recover an applied staging write: %v", err)
					return
				}
				if !runner.writeLost || !runner.probeLost || runner.probes < 2 || runner.offset400 < 2 {
					t.Errorf("missing recovery path: writeLost=%t probeLost=%t probes=%d offset400=%d", runner.writeLost, runner.probeLost, runner.probes, runner.offset400)
				}
			} else if err == nil || !runner.executionLost {
				t.Errorf("ambiguous execution result must return an error without replay: %v", err)
			}
			got, err := runner.inspect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.ScriptSHA256 != report.ExpectedSHA256 || got.MarkerSHA256 != report.ExpectedMarkerSHA256 || got.Counter != 1 || runner.executions != 1 {
				t.Errorf("payload mismatch or replay: script=%s marker=%s counter=%d dispatches=%d", got.ScriptSHA256, got.MarkerSHA256, got.Counter, runner.executions)
			}
			if got.Base64Bytes <= report.EncodedBytes {
				t.Error("stale-tail precondition was not preserved")
			}
		})
	}
}

type windowsUploadLiveEvent struct {
	Operation     string `json:"operation"`
	Offset        int    `json:"offset"`
	RealExitCode  int    `json:"realExitCode"`
	RealSucceeded bool   `json:"realSucceeded"`
	Applied       bool   `json:"guestWriteVerified,omitempty"`
	LostResult    bool   `json:"injectedLostResult,omitempty"`
}

type windowsUploadLiveSnapshot struct {
	ScriptSHA256 string `json:"scriptSHA256"`
	MarkerSHA256 string `json:"markerSHA256"`
	Counter      int    `json:"executionCounter"`
	Base64Bytes  int    `json:"base64Bytes"`
}

type windowsUploadLiveReport struct {
	Mode                  string                    `json:"mode"`
	FaultModel            string                    `json:"faultModel"`
	Source                string                    `json:"source"`
	ExpectedSHA256        string                    `json:"expectedScriptSHA256"`
	ExpectedMarkerSHA256  string                    `json:"expectedMarkerSHA256"`
	EncodedBytes          int                       `json:"encodedBytes"`
	UploaderReturnedError bool                      `json:"uploaderReturnedError"`
	Passed                bool                      `json:"passed"`
	CleanupComplete       bool                      `json:"cleanupComplete"`
	CleanupRemaining      int                       `json:"cleanupRemaining"`
	Guest                 windowsUploadLiveSnapshot `json:"guest"`
	Events                []windowsUploadLiveEvent  `json:"events"`
}

type windowsUploadLiveRunner struct {
	t                     *testing.T
	vm, base, mode        string
	writes, probes        int
	executions, offset400 int
	appendedBytes         int
	writeLost, probeLost  bool
	executionLost         bool
	events                []windowsUploadLiveEvent
}

func (r *windowsUploadLiveRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.t.Helper()
	command := decodePowerShellCommand(r.t, strings.Join(req.Args[2:], " "))
	event := windowsUploadLiveEvent{Operation: "initialize", Offset: -1}
	chunk := ""
	if match := windowsUploadOffset.FindStringSubmatch(command); match != nil {
		event.Operation = "offset-write"
		event.Offset, _ = strconv.Atoi(match[1])
		chunk = windowsUploadBytes.FindStringSubmatch(command)[1]
	} else if strings.Contains(command, "Add-Content") {
		event.Operation = "append-write"
		event.Offset = r.appendedBytes
		chunk = windowsUploadAppend.FindStringSubmatch(command)[1]
	} else if strings.Contains(command, `"ok" | Out-Null`) {
		event.Operation = "readiness-probe"
		r.probes++
	} else if strings.Contains(command, "& powershell.exe") {
		event.Operation = "execute-payload"
		r.executions++
	}
	result, err := (execCommandRunner{}).Run(ctx, req)
	event.RealExitCode = result.ExitCode
	event.RealSucceeded = err == nil
	lose := false
	if err == nil && chunk != "" {
		r.writes++
		if event.Offset == 400 {
			r.offset400++
		}
		verified, verifyErr := r.realPowerShell(ctx, fmt.Sprintf(`[IO.File]::ReadAllText(%s).Substring(%d, %d) -ceq %s`, psQuote(r.base+".ps1.b64"), event.Offset, len(chunk), psQuote(chunk)))
		if verifyErr != nil || !strings.EqualFold(strings.TrimSpace(verified.Stdout), "True") {
			return result, fmt.Errorf("guest write inspection failed: match=%q error=%v details=%s", strings.TrimSpace(verified.Stdout), verifyErr, strings.TrimSpace(verified.Stderr))
		}
		if event.Operation == "append-write" {
			r.appendedBytes += len(chunk) + 2
		}
		event.Applied = true
		if r.mode == "applied-write-result-loss" && r.writes == 2 && !r.writeLost {
			r.writeLost, lose = true, true
		}
	}
	if err == nil && r.mode == "applied-write-result-loss" && event.Operation == "readiness-probe" && !r.probeLost {
		r.probeLost, lose = true, true
	}
	if err == nil && r.mode == "execution-result-loss" && event.Operation == "execute-payload" && !r.executionLost {
		r.executionLost, lose = true, true
	}
	event.LostResult = lose
	r.events = append(r.events, event)
	if lose {
		return LocalCommandResult{ExitCode: 255, Stderr: "PrlJob_GetResult: Invalid argument"}, errors.New("controlled result loss after real prlctl success")
	}
	return result, err
}

func (r *windowsUploadLiveRunner) realPowerShell(ctx context.Context, script string) (LocalCommandResult, error) {
	args := append([]string{"exec", r.vm}, strings.Fields(PowershellCommand(script))...)
	return (execCommandRunner{}).Run(ctx, LocalCommandRequest{Name: "prlctl", Args: args})
}

func (r *windowsUploadLiveRunner) inspect(ctx context.Context) (windowsUploadLiveSnapshot, error) {
	result, err := r.realPowerShell(ctx, fmt.Sprintf(`$ErrorActionPreference='Stop'
$p=%s
$v=[ordered]@{scriptSHA256='';markerSHA256='';executionCounter=0;base64Bytes=0}
if(Test-Path -LiteralPath ($p+'.ps1')){$v.scriptSHA256=(Get-FileHash -Algorithm SHA256 -LiteralPath ($p+'.ps1')).Hash.ToLowerInvariant()}
if(Test-Path -LiteralPath ($p+'.marker')){$v.markerSHA256=(Get-FileHash -Algorithm SHA256 -LiteralPath ($p+'.marker')).Hash.ToLowerInvariant()}
if(Test-Path -LiteralPath ($p+'.counter')){$v.executionCounter=[int][IO.File]::ReadAllText($p+'.counter')}
if(Test-Path -LiteralPath ($p+'.ps1.b64')){$v.base64Bytes=(Get-Item -LiteralPath ($p+'.ps1.b64')).Length}
$v | ConvertTo-Json -Compress`, psQuote(r.base)))
	var snapshot windowsUploadLiveSnapshot
	if err == nil {
		err = json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &snapshot)
	}
	return snapshot, err
}

func (r *windowsUploadLiveRunner) cleanup(ctx context.Context) (int, error) {
	result, err := r.realPowerShell(ctx, fmt.Sprintf(`$ErrorActionPreference='Stop'
$files=@(%s,%s,%s,%s)
foreach($f in $files){if(Test-Path -LiteralPath $f){Remove-Item -LiteralPath $f -Force}}
@($files | Where-Object {Test-Path -LiteralPath $_}).Count`, psQuote(r.base+".ps1"), psQuote(r.base+".ps1.b64"), psQuote(r.base+".counter"), psQuote(r.base+".marker")))
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(strings.TrimSpace(result.Stdout))
}

func liveUploadHash(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

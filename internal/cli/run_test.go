package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFormatRunSummary(t *testing.T) {
	got := formatRunSummary(runTimings{
		sync:    1200 * time.Millisecond,
		command: 3400 * time.Millisecond,
		syncSteps: syncStepTimings{
			manifest: 20 * time.Millisecond,
			rsync:    900 * time.Millisecond,
		},
		syncSkipped: true,
	}, 5*time.Second, 7)
	for _, want := range []string{
		"run summary",
		"sync=1.2s",
		"command=3.4s",
		"total=5s",
		"sync_skipped=true",
		"exit=7",
		"sync_steps=manifest:20ms,rsync:900ms",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestFormatRunSummaryIncludesGitHydrateSkipReason(t *testing.T) {
	got := formatRunSummary(runTimings{
		sync: 2 * time.Second,
		syncSteps: syncStepTimings{
			gitHydrateSkipped:    true,
			gitHydrateSkipReason: "remote base current",
		},
	}, 3*time.Second, 0)
	if !strings.Contains(got, "git_hydrate:skipped_remote_base_current") {
		t.Fatalf("summary missing git hydrate skip reason: %q", got)
	}
}

func TestTimingJSONShape(t *testing.T) {
	var buf bytes.Buffer
	err := writeTimingJSON(&buf, timingReportFromRun("aws", "cbx_123", "blue-crab", runTimings{
		sync:    1200 * time.Millisecond,
		command: 3400 * time.Millisecond,
		syncSteps: syncStepTimings{
			rsync:                900 * time.Millisecond,
			gitHydrateSkipped:    true,
			gitHydrateSkipReason: "marker base current",
		},
		syncSkipped: true,
	}, 5*time.Second, 7))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Provider    string `json:"provider"`
		LeaseID     string `json:"leaseId"`
		SyncMs      int64  `json:"syncMs"`
		CommandMs   int64  `json:"commandMs"`
		TotalMs     int64  `json:"totalMs"`
		ExitCode    int    `json:"exitCode"`
		SyncSkipped bool   `json:"syncSkipped"`
		SyncPhases  []struct {
			Name    string `json:"name"`
			Ms      int64  `json:"ms"`
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		} `json:"syncPhases"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "aws" || got.LeaseID != "cbx_123" || got.SyncMs != 1200 || got.CommandMs != 3400 || got.TotalMs != 5000 || got.ExitCode != 7 || !got.SyncSkipped {
		t.Fatalf("unexpected report: %#v", got)
	}
	if len(got.SyncPhases) != 2 || got.SyncPhases[1].Name != "git_hydrate" || !got.SyncPhases[1].Skipped || got.SyncPhases[1].Reason != "marker base current" {
		t.Fatalf("unexpected phases: %#v", got.SyncPhases)
	}
}

func TestTimingJSONIncludesActionsRunURLWhenAvailable(t *testing.T) {
	var buf bytes.Buffer
	err := writeTimingJSON(&buf, timingReportFromRunWithActionsURL("aws", "cbx_123", "blue-crab", runTimings{
		sync:    1200 * time.Millisecond,
		command: 3400 * time.Millisecond,
	}, 5*time.Second, 0, "https://github.com/openclaw/openclaw/actions/runs/123"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ActionsRunURL string `json:"actionsRunUrl"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ActionsRunURL != "https://github.com/openclaw/openclaw/actions/runs/123" {
		t.Fatalf("actionsRunUrl=%q", got.ActionsRunURL)
	}
}

func TestApplyCapacityMarketFlag(t *testing.T) {
	fs := newFlagSet("test", io.Discard)
	market := fs.String("market", "spot", "")
	if err := parseFlags(fs, []string{"--market", "on-demand"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	if err := applyCapacityMarketFlag(&cfg, fs, *market); err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity.Market != "on-demand" {
		t.Fatalf("market=%s want on-demand", cfg.Capacity.Market)
	}

	fs = newFlagSet("test", io.Discard)
	market = fs.String("market", "spot", "")
	if err := parseFlags(fs, []string{"--market", "reserved"}); err != nil {
		t.Fatal(err)
	}
	if err := applyCapacityMarketFlag(&cfg, fs, *market); err == nil {
		t.Fatal("expected invalid market failure")
	}
}

func TestApplyServerTypeFlagOverridesUsesTargetAwareAWSDefaults(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "macos",
			args: []string{"--provider", "aws", "--target", "macos", "--class", "standard"},
			want: "mac2.metal",
		},
		{
			name: "windows",
			args: []string{"--provider", "aws", "--target", "windows", "--class", "standard"},
			want: "m7i.large",
		},
		{
			name: "windows wsl2",
			args: []string{"--provider", "aws", "--target", "windows", "--windows-mode", "wsl2", "--class", "standard"},
			want: "m8i.large",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Provider:    "aws",
				TargetOS:    targetLinux,
				WindowsMode: windowsModeNormal,
				Class:       "beast",
				ServerType:  "c7a.48xlarge",
			}
			fs := newFlagSet("test", io.Discard)
			provider := fs.String("provider", cfg.Provider, "")
			class := fs.String("class", cfg.Class, "")
			serverType := fs.String("type", "", "")
			targetFlags := registerTargetFlags(fs, cfg)
			if err := parseFlags(fs, tt.args); err != nil {
				t.Fatal(err)
			}
			cfg.Provider = *provider
			cfg.Class = *class
			if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
				t.Fatal(err)
			}
			applyServerTypeFlagOverrides(&cfg, fs, *serverType)
			if cfg.ServerType != tt.want {
				t.Fatalf("serverType=%q want %q", cfg.ServerType, tt.want)
			}
			if cfg.ServerTypeExplicit {
				t.Fatal("ServerTypeExplicit=true, want false")
			}
		})
	}
}

func TestApplyServerTypeFlagOverridesPreservesExplicitType(t *testing.T) {
	cfg := Config{
		Provider:    "aws",
		TargetOS:    targetLinux,
		WindowsMode: windowsModeNormal,
		Class:       "beast",
		ServerType:  "c7a.48xlarge",
	}
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", cfg.Provider, "")
	class := fs.String("class", cfg.Class, "")
	serverType := fs.String("type", "", "")
	targetFlags := registerTargetFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--provider", "aws", "--target", "macos", "--class", "standard", "--type", "mac1.metal"}); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = *provider
	cfg.Class = *class
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		t.Fatal(err)
	}
	applyServerTypeFlagOverrides(&cfg, fs, *serverType)
	if cfg.ServerType != "mac1.metal" {
		t.Fatalf("serverType=%q want mac1.metal", cfg.ServerType)
	}
	if !cfg.ServerTypeExplicit {
		t.Fatal("ServerTypeExplicit=false, want true")
	}
}

func TestCommandNeedsHydrationHint(t *testing.T) {
	if !commandNeedsHydrationHint([]string{"env NODE_OPTIONS=--max-old-space-size=4096 pnpm test"}, true) {
		t.Fatal("expected shell pnpm command to need hydration hint")
	}
	if commandNeedsHydrationHint([]string{"go", "test", "./..."}, false) {
		t.Fatal("go test should not need hydration hint")
	}
}

func TestRecordRunFailureCapturesShadowedReturnErrors(t *testing.T) {
	var recorded error
	func() {
		if err := errors.New("sync failed"); err != nil {
			_ = recordRunFailure(&recorded, err)
			return
		}
	}()
	if recorded == nil || recorded.Error() != "sync failed" {
		t.Fatalf("recorded=%v", recorded)
	}
	_ = recordRunFailure(&recorded, nil)
	if recorded == nil || recorded.Error() != "sync failed" {
		t.Fatalf("nil failure should not clear recorded error, got %v", recorded)
	}
}

package cli

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestCheckpointPruneRejectsOverflowBeforeDeleting(t *testing.T) {
	for _, flag := range []string{"--older-than", "--unused-for"} {
		t.Run(flag, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			store, err := defaultCheckpointStore()
			if err != nil {
				t.Fatal(err)
			}
			record, _, err := store.Reserve(checkpointRecord{
				ID: "chk_overflow", Kind: checkpointKindArchive,
				CreatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
			})
			if err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: io.Discard}
			// This value used to wrap to a positive duration of about 25 minutes.
			if err := app.checkpointPrune(context.Background(), []string{flag, "213504d"}); err == nil {
				t.Errorf("overflow accepted: %s", stdout.String())
			}
			if _, _, err := store.Read(record.ID); err != nil {
				t.Errorf("overflow removed checkpoint: %v", err)
			}
		})
	}
}

func TestDayDurationBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{"106752d", "213504d", "9223372036854775807d", "9223372036854775808d"} {
		t.Run(value, func(t *testing.T) {
			for _, flag := range []string{"--older-than", "--unused-for", "--expire-unused-after"} {
				if got, err := parseCheckpointPruneDurationFlag(flag, value); err == nil {
					t.Errorf("%s accepted overflow as %v", flag, got)
				}
			}
			if got, _, err := parseBenchmarkSince(value, now); err == nil {
				t.Errorf("--since accepted overflow as %v", got)
			}
		})
	}
	for _, value := range []string{"1d", "106751d", "24h"} {
		duration, err := parseCheckpointPruneDuration(value)
		if err != nil {
			t.Fatal(err)
		}
		want := 24 * time.Hour
		if value == "106751d" {
			want *= 106751
		}
		if duration != want {
			t.Errorf("%s: duration=%v, want %v", value, duration, want)
		}
		since, label, err := parseBenchmarkSince(value, now)
		if err != nil || label != value || !since.Equal(now.Add(-want)) {
			t.Errorf("%s: since=%v label=%q error=%v", value, since, label, err)
		}
	}
	if since, _, err := parseBenchmarkSince("0d", now); err != nil || !since.Equal(now) {
		t.Errorf("zero benchmark lookback: since=%v error=%v", since, err)
	}
}

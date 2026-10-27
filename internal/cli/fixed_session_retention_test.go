package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSessionDiskJournalFenceAndClone(t *testing.T) {
	at := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	c := LeaseClaim{LeaseID: "cbx_123456abcdef", CloudID: "owned-resource", FixedCreateIntent: &FixedCreateIntent{State: "acquired", Retention: &SessionDiskRetentionPolicy{Mode: "session-disk", OwnerBinding: strings.Repeat("a", 64), ComputeWindowSeconds: 60}, Journal: &FixedLeaseJournal{Version: 1, Phase: "acquired", Revision: 1}}}
	if err := initializeSessionDiskRetention(c.FixedCreateIntent, c.LeaseID, at); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeLeaseClaim("fixture", data)
	if err != nil {
		t.Fatal(err)
	}
	// The previous decoder's supported journal version is exactly 1. New
	// retained claims always use 2; adding fields to version 1 is forbidden.
	if decoded.FixedCreateIntent.Journal.Version != 2 {
		t.Fatal("retained journal is readable as a legacy allocation")
	}
	cloned := CloneLeaseClaim(c)
	cloned.FixedCreateIntent.Journal.Retention.DesiredState = "retired"
	cloned.FixedCreateIntent.Retention.OwnerBinding = strings.Repeat("b", 64)
	if c.FixedCreateIntent.Retention.OwnerBinding == cloned.FixedCreateIntent.Retention.OwnerBinding || c.FixedCreateIntent.Journal.Retention.DesiredState != "running" {
		t.Fatal("claim clone aliases retained authority")
	}
	for _, mutate := range []func(*FixedCreateIntent){
		func(i *FixedCreateIntent) { i.Journal.Version = 1 },
		func(i *FixedCreateIntent) { i.Retention = nil },
		func(i *FixedCreateIntent) { i.Journal.Retention = nil },
		func(i *FixedCreateIntent) {
			i.Journal.Retention.ComputeWindow.ExpiresAt = at.Add(2 * time.Minute).Format(time.RFC3339Nano)
		},
	} {
		bad := CloneLeaseClaim(c)
		mutate(bad.FixedCreateIntent)
		data, _ := json.Marshal(bad)
		if _, err := decodeLeaseClaim("fixture", data); err == nil {
			t.Fatal("malformed retention admitted by claim decoder")
		}
	}
	if _, err := SessionDiskExecutionDeadline(c, at.Add(59*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := SessionDiskExecutionDeadline(c, at.Add(time.Minute)); err == nil {
		t.Fatal("deadline boundary admitted work")
	}
}

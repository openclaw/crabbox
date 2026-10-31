package cli

import "testing"

func TestUnresolvedCheckpointTreatsOnlyTheJournalStateAsPending(t *testing.T) {
	hetzner := checkpointRecord{Kind: checkpointKindHetzner}
	hetzner.Native.ImageID = "123"
	hetzner.Native.State = "creating"
	if unresolvedCheckpoint(hetzner) {
		t.Fatal("a Hetzner --wait=false record in creating state must stay forkable/deletable")
	}

	for _, kind := range []string{checkpointKindHyperV, checkpointKindHetzner} {
		journal := checkpointRecord{Kind: kind}
		journal.Native.ImageID = "journal-name"
		journal.Native.State = NativeCheckpointJournalState
		if !unresolvedCheckpoint(journal) {
			t.Fatalf("%s: a pending provider journal must block capture, fork and restore", kind)
		}
		if unresolvedCheckpointJournal(journal) {
			t.Fatalf("%s: a pending journal with a delete identity must reach guarded delete cleanup", kind)
		}
		journal.Native.State = "available"
		if unresolvedCheckpoint(journal) {
			t.Fatalf("%s: an available checkpoint with a delete identity is resolved", kind)
		}
	}
}

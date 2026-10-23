package cli

import "testing"

func TestUnresolvedCheckpointCreatingStateScopedToHyperVJournal(t *testing.T) {
	hetzner := checkpointRecord{Kind: checkpointKindHetzner}
	hetzner.Native.ImageID = "123"
	hetzner.Native.State = "creating"
	if unresolvedCheckpoint(hetzner) {
		t.Fatal("a Hetzner --wait=false record in creating state must stay forkable/deletable")
	}

	journal := checkpointRecord{Kind: checkpointKindHyperV}
	journal.Native.ImageID = "journal-name"
	journal.Native.State = "creating"
	if !unresolvedCheckpoint(journal) {
		t.Fatal("a pending Hyper-V journal must block capture and fork")
	}
	if unresolvedCheckpointJournal(journal) {
		t.Fatal("a pending Hyper-V journal with a delete identity must reach guarded delete cleanup")
	}
	journal.Native.State = "available"
	if unresolvedCheckpoint(journal) {
		t.Fatal("an available Hyper-V checkpoint with a delete identity is resolved")
	}
}

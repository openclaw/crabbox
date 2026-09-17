package runnerfs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func journalTestDirectory(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("publication requires POSIX")
	}
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", root)
		if err := os.MkdirAll(filepath.Join(root, "Library", "Caches"), 0o700); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Setenv("XDG_CACHE_HOME", cache)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	}
	return root
}

func journalTestFile(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func journalTestContents(t *testing.T, name, want string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil || string(data) != want {
		t.Fatalf("read %s = %q, %v; want %q", name, data, err, want)
	}
}

func TestJournalPublication(t *testing.T) {
	root := journalTestDirectory(t)
	target := filepath.Join(root, "destination")
	stage := filepath.Join(root, "stage")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(stage, "payload")
	journalTestFile(t, target, "old")
	journalTestFile(t, payload, "new")
	if err := PublishArchive(payload, target); err != nil {
		t.Fatal(err)
	}
	journalTestContents(t, target, "new")
}

func TestJournalRecoversOrdinaryPublicationPhases(t *testing.T) {
	for _, spelling := range []string{"original", "relative", "symlink-parent", "parent-case", "basename-case"} {
		t.Run(spelling, func(t *testing.T) {
			for _, phase := range []string{"decision", "old-renamed", "new-renamed", "old-retired"} {
				t.Run(phase, func(t *testing.T) {
					root := journalTestDirectory(t)
					parent := filepath.Join(root, "Parent")
					if err := os.Mkdir(parent, 0o700); err != nil {
						t.Fatal(err)
					}
					target := filepath.Join(parent, "Destination")
					journalTestFile(t, target, "old")
					c, err := openArchiveJournal(target, "")
					if err != nil {
						t.Fatal(err)
					}
					defer c.parent.Close()
					retry := target
					switch spelling {
					case "relative":
						cwd, err := os.Getwd()
						if err != nil {
							t.Fatal(err)
						}
						retry, err = filepath.Rel(cwd, target)
						if err != nil {
							t.Fatal(err)
						}
					case "symlink-parent":
						alias := filepath.Join(root, "alias")
						if err := os.Symlink(parent, alias); err != nil {
							t.Fatal(err)
						}
						retry = filepath.Join(alias, "Destination")
					case "parent-case", "basename-case":
						if !archiveParentCaseInsensitive(c.parent) {
							t.Skip("requires case-insensitive Darwin filesystem")
						}
						if spelling == "parent-case" {
							retry = filepath.Join(root, "parent", "Destination")
						} else {
							retry = filepath.Join(parent, "destination")
						}
					}
					record, err := c.workspace()
					if err != nil {
						t.Fatal(err)
					}
					fresh := filepath.Join(record.Workspace, "new")
					journalTestFile(t, filepath.Join(parent, fresh), "new")
					record.Decision = "publish"
					record.New, err = archiveEntryID(c.parent, fresh)
					if err != nil {
						t.Fatal(err)
					}
					record.Old, err = archiveEntryID(c.parent, "Destination")
					if err != nil {
						t.Fatal(err)
					}
					if err := c.sync(record); err != nil {
						t.Fatal(err)
					}
					if err := c.write(record); err != nil {
						t.Fatal(err)
					}
					if phase != "decision" {
						if err := c.move("Destination", filepath.Join(record.Workspace, "old"), record.Old); err != nil {
							t.Fatal(err)
						}
						if err := c.sync(record); err != nil {
							t.Fatal(err)
						}
					}
					if phase == "new-renamed" || phase == "old-retired" {
						if err := c.move(fresh, "Destination", record.New); err != nil {
							t.Fatal(err)
						}
						if err := c.sync(record); err != nil {
							t.Fatal(err)
						}
					}
					if phase == "old-retired" {
						if err := c.parent.Remove(filepath.Join(record.Workspace, "old")); err != nil {
							t.Fatal(err)
						}
						if err := c.sync(record); err != nil {
							t.Fatal(err)
						}
					}
					reopened, err := openArchiveJournal(retry, "")
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.parent.Close()
					saved, err := reopened.read()
					if err != nil {
						t.Fatal(err)
					}
					if err := reopened.recover(saved); err != nil {
						t.Fatal(err)
					}
					journalTestContents(t, target, "new")
					entries, err := os.ReadDir(parent)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) != 1 || entries[0].Name() != "Destination" {
						t.Fatalf("recovered destination spelling changed: %v", entries)
					}
					if _, err := os.Lstat(reopened.journal); !os.IsNotExist(err) {
						t.Fatalf("journal not retired: %v", err)
					}
				})
			}
		})
	}
}

func TestJournalTargetMatchingRemainsConservative(t *testing.T) {
	root := journalTestDirectory(t)
	c, err := openArchiveJournal(filepath.Join(root, "Destination"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.parent.Close()
	if c.matchesTarget(filepath.Join(root, "other")) {
		t.Fatal("different destination matched")
	}
	c.target = filepath.Join(root, "\u00e9")
	if c.matchesTarget(filepath.Join(root, "\u00c9")) {
		t.Fatal("unproven non-ASCII equivalence matched")
	}
	c.target = filepath.Join(root, "Destination")
	if !archiveParentCaseInsensitive(c.parent) && c.matchesTarget(filepath.Join(root, "destination")) {
		t.Fatal("case difference matched without filesystem support")
	}
	if err := c.parent.Close(); err != nil {
		t.Fatal(err)
	}
	if c.matchesTarget(filepath.Join(root, "destination")) {
		t.Fatal("failed filesystem query permitted an alias")
	}
	if !c.matchesTarget(c.target) {
		t.Fatal("exact spelling requires no case-sensitivity query")
	}
}

func TestJournalLegacyAdoptionPreservesBothChoices(t *testing.T) {
	for _, choice := range []ArchiveRecoveryChoice{ArchiveKeepDestination, ArchiveRestoreBackup} {
		t.Run(string(choice), func(t *testing.T) {
			root := journalTestDirectory(t)
			target := filepath.Join(root, "destination")
			journalTestFile(t, target, "current")
			journalTestFile(t, target+".crabbox-cp-backup", "previous")
			journalTestFile(t, target+".crabbox-cp-transaction", "legacy recovery information")
			payload := filepath.Join(root, "payload")
			journalTestFile(t, payload, "incoming")
			var adoption *ArchiveAdoptionRequiredError
			if err := PublishArchive(payload, target); !errors.As(err, &adoption) {
				t.Fatalf("wanted adoption error, got %v", err)
			}
			journalTestContents(t, target, "current")
			journalTestContents(t, target+".crabbox-cp-backup", "previous")
			retained, err := AdoptArchiveRecovery(target, choice)
			if err != nil {
				t.Fatal(err)
			}
			if choice == ArchiveKeepDestination {
				journalTestContents(t, target, "current")
				journalTestContents(t, filepath.Join(retained, "legacy-backup"), "previous")
			} else {
				journalTestContents(t, target, "previous")
				journalTestContents(t, filepath.Join(retained, "legacy-destination"), "current")
			}
			journalTestContents(t, filepath.Join(retained, "legacy-marker"), "legacy recovery information")
			journalTestContents(t, payload, "incoming")
		})
	}
}

// Historical fixture provenance: https://github.com/openclaw/crabbox/pull/1556 at
// a313703092e85ee3b493d0f971e195339adc8c64, archive.go's
// writeCopyArchiveTransactionMarker and archive_owner_unix.go's
// copyArchiveProcessIdentity. These two functions are unchanged except their
// names. Only our own process and test-owned paths are used. No historical
// recovery, owner-liveness decision, or cleanup authority is retained here.
func journalHistoricalWriteMarker(name string) error {
	file, err := os.CreateTemp(filepath.Dir(name), ".crabbox-cp-marker-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	identity, ok := journalHistoricalProcessIdentity(os.Getpid())
	if !ok {
		_ = file.Close()
		return errors.New("resolve copy transaction owner identity")
	}
	if _, err := fmt.Fprintf(file, "crabbox-cp-v1\n%d\n%s\n", os.Getpid(), identity); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, name); err != nil {
		return err
	}
	if err := syncCopyArchiveDirectory(filepath.Dir(name)); err != nil {
		return err
	}
	return nil
}

func journalHistoricalProcessIdentity(pid int) (string, bool) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return "", false
		}
		closing := strings.LastIndexByte(string(data), ')')
		if closing < 0 {
			return "", false
		}
		fields := strings.Fields(string(data)[closing+1:])
		if len(fields) <= 19 {
			return "", false
		}
		return fields[19], true
	}
	output, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	identity := strings.TrimSpace(string(output))
	return identity, err == nil && identity != ""
}

// journalHistoricalPublishPrefix executes the successful write/rename prefix
// of that revision's PublishArchive, pausing at a normal durable boundary.
// Preconditions (fresh test files, no earlier transaction) replace historical
// locking/recovery. Error rollback and destructive completion are omitted.
func journalHistoricalPublishPrefix(t *testing.T, payload, target, phase string) string {
	t.Helper()
	marker := target + ".crabbox-cp-transaction"
	if err := journalHistoricalWriteMarker(marker); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if phase == "marker-written" {
		return string(data)
	}
	if err := os.Rename(target, target+".crabbox-cp-backup"); err != nil {
		t.Fatal(err)
	}
	if err := syncCopyArchiveDirectory(filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	if phase == "old-renamed" {
		return string(data)
	}
	if err := os.Rename(payload, target); err != nil {
		t.Fatal(err)
	}
	if err := syncCopyArchiveDirectory(filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestJournalHistoricalWriterMigration(t *testing.T) {
	for _, phase := range []string{"marker-written", "old-renamed", "new-renamed"} {
		for _, choice := range []ArchiveRecoveryChoice{ArchiveKeepDestination, ArchiveRestoreBackup} {
			t.Run(phase+"/"+string(choice), func(t *testing.T) {
				root := journalTestDirectory(t)
				target, payload := filepath.Join(root, "destination"), filepath.Join(root, "historical-payload")
				journalTestFile(t, target, "previous")
				journalTestFile(t, payload, "historical incoming")
				marker := journalHistoricalPublishPrefix(t, payload, target, phase)
				next := filepath.Join(root, "next-payload")
				journalTestFile(t, next, "next incoming")
				var adoption *ArchiveAdoptionRequiredError
				if err := PublishArchive(next, target); !errors.As(err, &adoption) {
					t.Fatalf("wanted adoption error, got %v", err)
				}
				journalTestContents(t, next, "next incoming")
				journalTestContents(t, target+".crabbox-cp-transaction", marker)
				if phase != "marker-written" {
					journalTestContents(t, target+".crabbox-cp-backup", "previous")
				}
				if phase != "new-renamed" {
					journalTestContents(t, payload, "historical incoming")
				}
				if phase == "marker-written" && choice == ArchiveRestoreBackup {
					if _, err := AdoptArchiveRecovery(target, choice); err == nil {
						t.Fatal("restore without backup succeeded")
					}
					journalTestContents(t, target, "previous")
					journalTestContents(t, target+".crabbox-cp-transaction", marker)
					return
				}
				retained, err := AdoptArchiveRecovery(target, choice)
				if err != nil {
					t.Fatal(err)
				}
				checkRetained := func() {
					journalTestContents(t, filepath.Join(retained, "legacy-marker"), marker)
					if choice == ArchiveKeepDestination && phase != "marker-written" {
						journalTestContents(t, filepath.Join(retained, "legacy-backup"), "previous")
					}
					if choice == ArchiveRestoreBackup && phase == "new-renamed" {
						journalTestContents(t, filepath.Join(retained, "legacy-destination"), "historical incoming")
					}
					if phase != "new-renamed" {
						journalTestContents(t, payload, "historical incoming")
					}
				}
				checkRetained()
				switch {
				case choice == ArchiveRestoreBackup || phase == "marker-written":
					journalTestContents(t, target, "previous")
				case phase == "new-renamed":
					journalTestContents(t, target, "historical incoming")
				default:
					if _, err := os.Lstat(target); !os.IsNotExist(err) {
						t.Fatalf("keep absent destination: %v", err)
					}
				}
				if err := PublishArchive(next, target); err != nil {
					t.Fatal(err)
				}
				journalTestContents(t, target, "next incoming")
				checkRetained()
			})
		}
	}
}

func TestJournalHistoricalAdoptionAcrossRepeatedReopens(t *testing.T) {
	for _, choice := range []ArchiveRecoveryChoice{ArchiveKeepDestination, ArchiveRestoreBackup} {
		t.Run(string(choice), func(t *testing.T) {
			root := journalTestDirectory(t)
			target, payload := filepath.Join(root, "destination"), filepath.Join(root, "payload")
			journalTestFile(t, target, "previous")
			journalTestFile(t, payload, "historical incoming")
			marker := journalHistoricalPublishPrefix(t, payload, target, "new-renamed")
			c, err := openArchiveJournal(target, "")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { c.parent.Close() }()
			record, err := c.workspace()
			if err != nil {
				t.Fatal(err)
			}
			record.Decision = string(choice)
			record.Old, err = archiveEntryID(c.parent, "destination")
			if err != nil {
				t.Fatal(err)
			}
			record.Backup, err = archiveEntryID(c.parent, "destination.crabbox-cp-backup")
			if err != nil {
				t.Fatal(err)
			}
			record.Marker, err = archiveEntryID(c.parent, "destination.crabbox-cp-transaction")
			if err != nil {
				t.Fatal(err)
			}
			if err := c.write(record); err != nil {
				t.Fatal(err)
			}
			// Closing/reopening after every durable namespace transition models multiple
			// interruptions while resuming one decision, without timing or fault hooks.
			reopen := func() {
				t.Helper()
				if err := c.sync(record); err != nil {
					t.Fatal(err)
				}
				if err := c.parent.Close(); err != nil {
					t.Fatal(err)
				}
				c, err = openArchiveJournal(target, "")
				if err != nil {
					t.Fatal(err)
				}
				record, err = c.read()
				if err != nil || record == nil {
					t.Fatalf("reopen decision: %v", err)
				}
			}
			reopen()
			move := func(from, to, id string) {
				t.Helper()
				if err := c.move(from, to, id); err != nil {
					t.Fatal(err)
				}
				reopen()
			}
			if choice == ArchiveKeepDestination {
				move("destination.crabbox-cp-backup", filepath.Join(record.Workspace, "legacy-backup"), record.Backup)
			} else {
				move("destination", filepath.Join(record.Workspace, "legacy-destination"), record.Old)
				move("destination.crabbox-cp-backup", "destination", record.Backup)
			}
			move("destination.crabbox-cp-transaction", filepath.Join(record.Workspace, "legacy-marker"), record.Marker)
			retained, err := AdoptArchiveRecovery(target, choice)
			if err != nil {
				t.Fatal(err)
			}
			journalTestContents(t, filepath.Join(retained, "legacy-marker"), marker)
			saved, want := "legacy-backup", "previous"
			if choice == ArchiveRestoreBackup {
				saved, want = "legacy-destination", "historical incoming"
				journalTestContents(t, target, "previous")
			} else {
				journalTestContents(t, target, "historical incoming")
			}
			journalTestContents(t, filepath.Join(retained, saved), want)
			journalTestFile(t, payload, "next incoming")
			if err := PublishArchive(payload, target); err != nil {
				t.Fatal(err)
			}
			journalTestContents(t, target, "next incoming")
			journalTestContents(t, filepath.Join(retained, saved), want)
			journalTestContents(t, filepath.Join(retained, "legacy-marker"), marker)
		})
	}
}

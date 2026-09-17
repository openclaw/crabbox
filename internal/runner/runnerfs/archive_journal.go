package runnerfs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const archiveJournalLimit = 16384

// ArchiveAdoptionRequiredError never derives recovery authority from sidecar
// contents. The caller must obtain an explicit choice for this destination.
type ArchiveAdoptionRequiredError struct{ Target string }

func (e *ArchiveAdoptionRequiredError) Error() string {
	return fmt.Sprintf("archive copy found legacy recovery data for %q; explicit recovery is required", e.Target)
}

type ArchiveRecoveryChoice string

const (
	ArchiveKeepDestination ArchiveRecoveryChoice = "keep-destination"
	ArchiveRestoreBackup   ArchiveRecoveryChoice = "restore-backup"
)

type archiveJournal struct {
	Version     int    `json:"version"`
	Target      string `json:"target"`
	Parent      string `json:"parent"`
	Workspace   string `json:"workspace"`
	WorkspaceID string `json:"workspaceID"`
	Decision    string `json:"decision"`
	New         string `json:"new,omitempty"`
	Old         string `json:"old,omitempty"`
	Marker      string `json:"marker,omitempty"`
	Backup      string `json:"backup,omitempty"`
}

type archiveJournalContext struct {
	target    string
	parent    *os.Root
	parentID  string
	directory string
	journal   string
}

func archiveIdentity(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return copyArchiveDirectoryIdentity("", info)
}

func archiveEntryID(root *os.Root, name string) (string, error) {
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return archiveIdentity(info), nil
}

func requireLegacyArchiveAdoption(target, backup, marker string) error {
	for _, name := range []string{backup, marker} {
		if _, err := os.Lstat(name); err == nil {
			return &ArchiveAdoptionRequiredError{Target: target}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func archivePathWithin(name, directory string) bool {
	if runtime.GOOS == "darwin" {
		name, directory = strings.ToLower(name), strings.ToLower(directory)
	}
	rel, err := filepath.Rel(directory, name)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func openArchiveJournal(target, payload string) (*archiveJournalContext, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("archive publication requires a POSIX host")
	}
	resolved, err := resolvePathParent(target)
	if err != nil {
		return nil, err
	}
	target = resolved
	physical := filepath.Dir(target)
	if target == physical {
		return nil, errors.New("cannot publish over filesystem root")
	}
	state, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	state, err = canonicalArchiveStatePath(state)
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	cache, err = canonicalArchiveStatePath(cache)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(state, "crabbox", "cp-journals-v2")
	lockDirectory := filepath.Join(cache, "crabbox", "cp-locks")
	if archivePathsOverlap(directory, target) || archivePathsOverlap(lockDirectory, target) {
		return nil, errors.New("copy destination contains its recovery journal or lock directory")
	}
	if payload != "" {
		payload, err = filepath.EvalSymlinks(payload)
		if err != nil {
			return nil, err
		}
		if archivePathsOverlap(directory, payload) || archivePathsOverlap(lockDirectory, payload) {
			return nil, errors.New("copy payload contains its recovery journal or lock directory")
		}
	}
	if err := createArchiveStateDirectory(state); err != nil {
		return nil, err
	}
	// Refuse links and untrusted state directories rather than repairing them.
	for _, name := range []string{state, filepath.Join(state, "crabbox"), directory} {
		if err := os.Mkdir(name, 0o700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		info, err := os.Lstat(name)
		if err != nil {
			return nil, err
		}
		if (name == directory && !archivePrivateDirectory(info)) || !archiveTrustedDirectory(info) {
			return nil, fmt.Errorf("copy recovery directory is not private: %s", name)
		}
		if err := syncCopyArchiveDirectory(filepath.Dir(name)); err != nil {
			return nil, err
		}
	}
	key := target
	if runtime.GOOS == "darwin" {
		key = strings.ToLower(key)
	}
	digest := sha256.Sum256([]byte(key))
	parent, err := os.OpenRoot(physical)
	if err != nil {
		return nil, err
	}
	info, err := parent.Stat(".")
	if err != nil {
		parent.Close()
		return nil, err
	}
	return &archiveJournalContext{target: target, parent: parent, parentID: archiveIdentity(info), directory: directory, journal: filepath.Join(directory, fmt.Sprintf("%x.json", digest))}, nil
}

func (c *archiveJournalContext) read() (*archiveJournal, error) {
	info, err := os.Lstat(c.journal)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !archivePrivateFile(info) || archiveHasHardLinks(info) {
		return nil, errors.New("copy recovery journal is not a private regular file")
	}
	file, err := os.OpenFile(c.journal, os.O_RDONLY|nonblockingOpen, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, errors.New("copy recovery journal changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, archiveJournalLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > archiveJournalLimit {
		return nil, errors.New("copy recovery journal exceeds size limit")
	}
	var record archiveJournal
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("invalid copy recovery journal: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("copy recovery journal has trailing data")
	}
	if record.Version != 2 || record.Parent != c.parentID || !c.matchesTarget(record.Target) ||
		!strings.HasPrefix(record.Workspace, ".crabbox-cp-publish-") || filepath.Base(record.Workspace) != record.Workspace || record.WorkspaceID == "" {
		return nil, errors.New("copy recovery journal does not match destination and physical parent")
	}
	if record.Decision != "publish" && record.Decision != string(ArchiveKeepDestination) && record.Decision != string(ArchiveRestoreBackup) {
		return nil, errors.New("copy recovery journal has no recognized decision")
	}
	if record.Decision == "publish" && (record.New == "" || record.Marker != "" || record.Backup != "") {
		return nil, errors.New("copy recovery journal has invalid publication identities")
	}
	return &record, nil
}

func (c *archiveJournalContext) matchesTarget(recorded string) bool {
	if recorded == c.target {
		return true
	}
	// Name folding is a filesystem property, not an OS-wide assumption. Keep
	// non-ASCII aliases conservative until their filesystem equivalence is known.
	if len(recorded) != len(c.target) {
		return false
	}
	for i := 0; i < len(recorded); i++ {
		a, b := recorded[i], c.target[i]
		if a == b {
			continue
		}
		if a >= 0x80 || b >= 0x80 {
			return false
		}
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return archiveParentCaseInsensitive(c.parent)
}

func (c *archiveJournalContext) write(record *archiveJournal) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > archiveJournalLimit {
		return errors.New("copy recovery journal exceeds size limit")
	}
	file, err := os.CreateTemp(c.directory, ".pending-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	// An existing record must be recovered; it can never be overwritten.
	if err := os.Link(temporary, c.journal); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	return syncCopyArchiveDirectory(c.directory)
}

func (c *archiveJournalContext) workspace() (*archiveJournal, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	name := fmt.Sprintf(".crabbox-cp-publish-%x", token)
	if err := c.parent.Mkdir(name, 0o700); err != nil {
		return nil, err
	}
	info, err := c.parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !archivePrivateDirectory(info) {
		return nil, errors.New("copy publication workspace is not private")
	}
	if err := syncArchiveRootDirectory(c.parent, "."); err != nil {
		return nil, err
	}
	return &archiveJournal{Version: 2, Target: c.target, Parent: c.parentID, Workspace: name, WorkspaceID: archiveIdentity(info)}, nil
}

func (c *archiveJournalContext) sync(record *archiveJournal) error {
	return errors.Join(syncArchiveRootDirectory(c.parent, record.Workspace), syncArchiveRootDirectory(c.parent, "."))
}

// move accepts either the original or already-completed rename, and never
// replaces an extant entry. The durable record is the sole recovery decision.
func (c *archiveJournalContext) move(from, to, identity string) error {
	if identity == "" {
		return nil
	}
	source, err := archiveEntryID(c.parent, from)
	if err != nil {
		return err
	}
	destination, err := archiveEntryID(c.parent, to)
	if err != nil {
		return err
	}
	if source == "" && destination == identity {
		return nil
	}
	if source != identity || destination != "" {
		return fmt.Errorf("copy recovery needs inspection: entries changed (%s, %s); all data retained", from, to)
	}
	return c.parent.Rename(from, to)
}

func (c *archiveJournalContext) recover(record *archiveJournal) error {
	// A prior attempt may have failed while syncing the decision's directory.
	// Re-establish its durability before performing any recorded namespace move.
	if err := syncCopyArchiveDirectory(c.directory); err != nil {
		return err
	}
	info, err := c.parent.Lstat(record.Workspace)
	if err != nil {
		return err
	}
	if !archivePrivateDirectory(info) || archiveIdentity(info) != record.WorkspaceID {
		return errors.New("copy recovery workspace identity changed; all data retained")
	}
	target := filepath.Base(record.Target)
	switch record.Decision {
	case "publish":
		if err := c.replace(record, filepath.Join(record.Workspace, "new"), filepath.Join(record.Workspace, "old"), record.New); err != nil {
			return err
		}
	case string(ArchiveKeepDestination):
		current, err := archiveEntryID(c.parent, target)
		if err != nil {
			return err
		}
		if current != record.Old {
			return errors.New("adopted copy destination changed; all data retained")
		}
		if err := c.move(target+".crabbox-cp-backup", filepath.Join(record.Workspace, "legacy-backup"), record.Backup); err != nil {
			return err
		}
	case string(ArchiveRestoreBackup):
		if err := c.replace(record, target+".crabbox-cp-backup", filepath.Join(record.Workspace, "legacy-destination"), record.Backup); err != nil {
			return err
		}
	}
	if record.Decision != "publish" {
		if err := c.move(target+".crabbox-cp-transaction", filepath.Join(record.Workspace, "legacy-marker"), record.Marker); err != nil {
			return err
		}
	}
	if err := c.sync(record); err != nil {
		return err
	}
	// Retain the old tree until the published name is durable. Cleanup is not
	// required for publication correctness; a failure leaves the record retryable.
	if record.Decision == "publish" && record.Old != "" {
		old := filepath.Join(record.Workspace, "old")
		identity, err := archiveEntryID(c.parent, old)
		if err != nil {
			return err
		}
		if identity != "" && identity != record.Old {
			return errors.New("replaced copy destination changed; all data retained")
		}
		if identity != "" {
			if err := c.parent.RemoveAll(old); err != nil {
				return err
			}
		}
		if err := c.sync(record); err != nil {
			return err
		}
	}
	if err := os.Remove(c.journal); err != nil {
		return err
	}
	if err := syncCopyArchiveDirectory(c.directory); err != nil {
		return err
	}
	// Keep adopted data permanently. Empty publication workspaces are harmless
	// if interrupted after journal retirement.
	if record.Decision == "publish" {
		_ = c.parent.Remove(record.Workspace)
	}
	return nil
}

func publishJournaledArchive(payload, target string) error {
	// Validate containment before even creating the compatibility lock directory.
	c, err := openArchiveJournal(target, payload)
	if err != nil {
		return err
	}
	defer c.parent.Close()
	unlock, err := acquireCopyArchiveTargetLock(c.target)
	if err != nil {
		return err
	}
	defer unlock()
	previous, err := c.read()
	if err != nil {
		return err
	}
	if previous != nil {
		if err := c.recover(previous); err != nil {
			return err
		}
	}
	if err := requireLegacyArchiveAdoption(c.target, c.target+".crabbox-cp-backup", c.target+".crabbox-cp-transaction"); err != nil {
		return err
	}
	old, err := c.parent.Lstat(filepath.Base(c.target))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if old != nil && old.Mode()&os.ModeSymlink != 0 {
		return invalid("archive copy refuses a symlink target: %s", target)
	}
	fresh, err := os.Lstat(payload)
	if err != nil {
		return err
	}
	if !fresh.IsDir() && !fresh.Mode().IsRegular() {
		return errors.New("copy payload is not a file or directory")
	}
	record, err := c.workspace()
	if err != nil {
		return err
	}
	record.Decision, record.New, record.Old = "publish", archiveIdentity(fresh), archiveIdentity(old)
	ownedPayload := filepath.Join(filepath.Dir(c.target), record.Workspace, "new")
	physicalPayload, err := resolvePathParent(payload)
	if err != nil {
		return err
	}
	relativePayload, err := filepath.Rel(filepath.Dir(c.target), physicalPayload)
	if err != nil || !filepath.IsLocal(relativePayload) || relativePayload == filepath.Base(c.target) || archivePathWithin(c.target, physicalPayload) {
		return errors.New("copy publication payload must be staged separately beneath the destination parent")
	}
	if err := c.parent.Rename(relativePayload, filepath.Join(record.Workspace, "new")); err != nil {
		return err
	}
	// The staging owner may now Close safely even if any subsequent sync fails.
	if err := syncArchiveRootDirectory(c.parent, filepath.Dir(relativePayload)); err != nil {
		return fmt.Errorf("copy payload retained in %s: %w", ownedPayload, err)
	}
	if err := c.sync(record); err != nil {
		return fmt.Errorf("copy payload retained in %s: %w", ownedPayload, err)
	}
	if err := c.write(record); err != nil {
		return fmt.Errorf("copy payload retained in %s: %w", ownedPayload, err)
	}
	return c.recover(record)
}

// AdoptArchiveRecovery records the user's explicit, destination-scoped choice.
// It never reads old marker contents and never deletes extant legacy data.
// The returned directory retains the unselected entries for manual inspection.
func AdoptArchiveRecovery(target string, choice ArchiveRecoveryChoice) (string, error) {
	if choice != ArchiveKeepDestination && choice != ArchiveRestoreBackup {
		return "", errors.New("choose keep-destination or restore-backup")
	}
	c, err := openArchiveJournal(target, "")
	if err != nil {
		return "", err
	}
	defer c.parent.Close()
	unlock, err := acquireCopyArchiveTargetLock(c.target)
	if err != nil {
		return "", err
	}
	defer unlock()
	existing, err := c.read()
	if err != nil {
		return "", err
	}
	if existing != nil {
		if existing.Decision != string(choice) {
			return "", errors.New("destination already has a different durable recovery decision")
		}
		retained := filepath.Join(filepath.Dir(c.target), existing.Workspace)
		return retained, c.recover(existing)
	}
	name := filepath.Base(c.target)
	old, err := archiveEntryID(c.parent, name)
	if err != nil {
		return "", err
	}
	backup, err := archiveEntryID(c.parent, name+".crabbox-cp-backup")
	if err != nil {
		return "", err
	}
	marker, err := archiveEntryID(c.parent, name+".crabbox-cp-transaction")
	if err != nil {
		return "", err
	}
	if backup == "" && marker == "" {
		return "", errors.New("destination has no legacy recovery data to adopt")
	}
	if choice == ArchiveRestoreBackup && backup == "" {
		return "", errors.New("destination has no legacy backup to restore")
	}
	record, err := c.workspace()
	if err != nil {
		return "", err
	}
	record.Decision, record.Old, record.Backup, record.Marker = string(choice), old, backup, marker
	retained := filepath.Join(filepath.Dir(c.target), record.Workspace)
	if err := c.write(record); err != nil {
		return retained, err
	}
	return retained, c.recover(record)
}

func syncArchiveRootDirectory(root *os.Root, name string) error {
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// Resolve existing ancestors without creating state inside a destination that
// is about to be replaced. Missing components are created only after exclusion.
func canonicalArchiveStatePath(name string) (string, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(name); err == nil {
		return filepath.EvalSymlinks(name)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent, err := canonicalArchiveStatePath(filepath.Dir(name))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(name)), nil
}

func createArchiveStateDirectory(name string) error {
	if _, err := os.Lstat(name); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := createArchiveStateDirectory(filepath.Dir(name)); err != nil {
		return err
	}
	if err := os.Mkdir(name, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	return syncCopyArchiveDirectory(filepath.Dir(name))
}

func (c *archiveJournalContext) replace(record *archiveJournal, source, saved, replacement string) error {
	pending, err := archiveEntryID(c.parent, source)
	if err != nil {
		return err
	}
	if pending != "" {
		if pending != replacement {
			return errors.New("copy replacement identity changed; all data retained")
		}
		if err := c.move(filepath.Base(record.Target), saved, record.Old); err != nil {
			return err
		}
		if err := c.sync(record); err != nil {
			return err
		}
	}
	return c.move(source, filepath.Base(record.Target), replacement)
}

func archivePathsOverlap(a, b string) bool {
	return archivePathWithin(a, b) || archivePathWithin(b, a)
}

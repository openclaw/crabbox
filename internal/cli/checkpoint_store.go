package cli

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gofrs/flock"
)

type checkpointStore struct {
	root string
}

type checkpointNotFoundError struct {
	ExitError
}

func (e checkpointNotFoundError) Unwrap() error {
	return e.ExitError
}

func isCheckpointNotFound(err error) bool {
	var missing checkpointNotFoundError
	return errors.As(err, &missing)
}

type checkpointPaths struct {
	Dir     string
	Meta    string
	Archive string
}

func defaultCheckpointStore() (checkpointStore, error) {
	stateDir, err := crabboxStateDir()
	if err != nil {
		return checkpointStore{}, err
	}
	return checkpointStore{root: filepath.Join(stateDir, "checkpoints")}, nil
}

func checkpointDir(id string) (string, error) {
	store, err := defaultCheckpointStore()
	if err != nil {
		return "", err
	}
	paths, err := store.Paths(id)
	if err != nil {
		return "", err
	}
	return paths.Dir, nil
}

func (s checkpointStore) Paths(id string) (checkpointPaths, error) {
	id, err := validateCheckpointID(id)
	if err != nil {
		return checkpointPaths{}, err
	}
	dir := filepath.Join(s.root, id)
	return checkpointPaths{
		Dir:     dir,
		Meta:    filepath.Join(dir, checkpointMetaFile),
		Archive: filepath.Join(dir, checkpointArchive),
	}, nil
}

func (s checkpointStore) Reserve(record checkpointRecord) (checkpointRecord, checkpointPaths, error) {
	if record.ID == "" {
		id, err := newCheckpointID()
		if err != nil {
			return checkpointRecord{}, checkpointPaths{}, err
		}
		record.ID = id
	}
	id, err := validateCheckpointID(record.ID)
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	record.ID = id
	if record.CreatedAt == "" {
		record.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if record.LastUsedAt == "" {
		record.LastUsedAt = record.CreatedAt
	}
	if err := validateCheckpointRecordTimes(record); err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	paths, err := s.Paths(record.ID)
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	firstExistingDir, err := nearestExistingClaimDirectory(paths.Dir)
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return checkpointRecord{}, checkpointPaths{}, exit(2, "create checkpoint root: %v", err)
	}
	if err := os.Mkdir(paths.Dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return checkpointRecord{}, checkpointPaths{}, exit(2, "checkpoint %s already exists", record.ID)
		}
		return checkpointRecord{}, checkpointPaths{}, exit(2, "create checkpoint %s: %v", record.ID, err)
	}
	if err := s.writeMetadata(record, paths); err != nil {
		_ = os.RemoveAll(paths.Dir)
		return checkpointRecord{}, checkpointPaths{}, err
	}
	if err := syncCreatedClaimDirectoryParentsWithSync(paths.Dir, firstExistingDir, syncControllerDirectory); err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	return record, paths, nil
}

func (s checkpointStore) Create(record checkpointRecord) (checkpointRecord, error) {
	record, _, err := s.Reserve(record)
	return record, err
}

func (s checkpointStore) Write(record checkpointRecord) error {
	if err := validateCheckpointRecordTimes(record); err != nil {
		return err
	}
	paths, err := s.Paths(record.ID)
	if err != nil {
		return err
	}
	firstExistingDir, err := nearestExistingClaimDirectory(paths.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return exit(2, "create checkpoint directory: %v", err)
	}
	if err := s.writeMetadata(record, paths); err != nil {
		return err
	}
	return syncCreatedClaimDirectoryParentsWithSync(paths.Dir, firstExistingDir, syncControllerDirectory)
}

func (s checkpointStore) writeMetadata(record checkpointRecord, paths checkpointPaths) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return exit(2, "encode checkpoint %s: %v", record.ID, err)
	}
	data = append(data, '\n')
	if err := writeStateFileAtomic(paths.Meta, data, syncControllerDirectory); err != nil {
		return exit(2, "write checkpoint %s: %v", record.ID, err)
	}
	return syncControllerDirectory(s.root)
}

func (s checkpointStore) WithLock(id string, action func() error) error {
	if _, err := validateCheckpointID(id); err != nil {
		return err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return exit(2, "create checkpoint root: %v", err)
	}
	return s.withLock(id, true, action)
}

func (s checkpointStore) Read(id string) (checkpointRecord, checkpointPaths, error) {
	paths, err := s.Paths(id)
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	data, err := os.ReadFile(paths.Meta)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkpointRecord{}, checkpointPaths{}, checkpointNotFoundError{exit(2, "checkpoint %s not found", id)}
		}
		return checkpointRecord{}, checkpointPaths{}, exit(2, "read checkpoint %s: %v", id, err)
	}
	var record checkpointRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return checkpointRecord{}, checkpointPaths{}, exit(2, "parse checkpoint %s: %v", id, err)
	}
	dirID := filepath.Base(paths.Dir)
	if record.ID == "" {
		record.ID = dirID
	} else if record.ID != dirID {
		return checkpointRecord{}, checkpointPaths{}, exit(2, "checkpoint %s metadata id mismatch: %s", dirID, record.ID)
	}
	if record.LastUsedAt == "" {
		record.LastUsedAt = record.CreatedAt
	}
	return record, paths, nil
}

func validateCheckpointRecordTimes(record checkpointRecord) error {
	if _, err := time.Parse(time.RFC3339, record.CreatedAt); err != nil {
		return exit(2, "checkpoint createdAt must be RFC3339: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, record.LastUsedAt); err != nil {
		return exit(2, "checkpoint lastUsedAt must be RFC3339: %v", err)
	}
	return nil
}

// The lock survives record deletion; capture and deletion cannot publish through
// different lock inodes. Usage updates reread under the same lock instead of
// restoring a stale record after a concurrent delete or capture completion.
func (s checkpointStore) withRecord(id string, wait bool, action func(checkpointRecord) error) error {
	return s.withLock(id, wait, func() error {
		current, _, err := s.Read(id)
		if err != nil {
			return err
		}
		return action(current)
	})
}

func (s checkpointStore) withLock(id string, wait bool, action func() error) error {
	paths, err := s.Paths(id)
	if err != nil {
		return err
	}
	lockPath := paths.Dir + ".lock"
	busy := exit(2, "checkpoint %s is busy; retry after its current operation finishes", id)
	mu := claimMutationMutex(lockPath)
	if wait {
		mu.Lock()
	} else if !mu.TryLock() {
		return busy
	}
	defer mu.Unlock()
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	defer lock.Close()
	locked := true
	if wait {
		err = lock.Lock()
	} else {
		locked, err = lock.TryLock()
	}
	if errors.Is(err, os.ErrNotExist) {
		return checkpointNotFoundError{exit(2, "checkpoint %s not found", id)}
	}
	if err != nil {
		return exit(2, "lock checkpoint %s: %v", id, err)
	}
	if !locked {
		return busy
	}
	return action()
}

func recordCheckpointUse(store checkpointStore, record *checkpointRecord) error {
	err := store.withRecord(record.ID, true, func(current checkpointRecord) error {
		current.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		if err := store.Write(current); err != nil {
			return err
		}
		*record = current
		return nil
	})
	if isCheckpointNotFound(err) {
		// An already-created fork survives checkpoint deletion; usage must not recreate it.
		return nil
	}
	return err
}

func (s checkpointStore) List() ([]checkpointRecord, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, exit(2, "read checkpoints: %v", err)
	}
	records := []checkpointRecord{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, _, err := s.Read(entry.Name())
		// The directory can precede atomic metadata publication or outlive a
		// concurrent deletion. Only published metadata constitutes a record.
		if isCheckpointNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339, records[i].CreatedAt)
		right, rightErr := time.Parse(time.RFC3339, records[j].CreatedAt)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.After(right)
		}
		return records[i].ID > records[j].ID
	})
	return records, nil
}

func (s checkpointStore) Delete(id string) error {
	paths, err := s.Paths(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(paths.Dir); err != nil {
		return exit(2, "delete checkpoint %s: %v", id, err)
	}
	return nil
}

// WriteNativeProgress keeps the CLI's canonical record authoritative across provider
// interruption. Do not advance the caller's state until the durable write succeeds.
func (s checkpointStore) WriteNativeProgress(record *checkpointRecord, result NativeCheckpointCreateResult, noReboot bool) error {
	next := *record
	applyNativeImageCheckpointRecord(&next, coordinatorImageFromNativeCheckpoint(result.Image), noReboot)
	next.Native.Metadata = maps.Clone(result.Metadata)
	if err := s.Write(next); err != nil {
		return err
	}
	*record = next
	return nil
}

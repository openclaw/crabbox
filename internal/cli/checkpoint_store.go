package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type checkpointStore struct {
	root string
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
	if _, err := time.Parse(time.RFC3339, record.CreatedAt); err != nil {
		return checkpointRecord{}, checkpointPaths{}, exit(2, "checkpoint createdAt must be RFC3339: %v", err)
	}
	paths, err := s.Paths(record.ID)
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
	return record, paths, nil
}

func (s checkpointStore) Create(record checkpointRecord) (checkpointRecord, error) {
	record, paths, err := s.Reserve(record)
	if err != nil {
		return checkpointRecord{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(paths.Dir)
		}
	}()
	if err := s.Write(record); err != nil {
		return checkpointRecord{}, err
	}
	committed = true
	return record, nil
}

func (s checkpointStore) Write(record checkpointRecord) error {
	paths, err := s.Paths(record.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return exit(2, "create checkpoint directory: %v", err)
	}
	return s.writeMetadata(record, paths)
}

func (s checkpointStore) writeMetadata(record checkpointRecord, paths checkpointPaths) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return exit(2, "encode checkpoint %s: %v", record.ID, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(paths.Meta, data, 0o600); err != nil {
		return exit(2, "write checkpoint %s: %v", record.ID, err)
	}
	return nil
}

func (s checkpointStore) Read(id string) (checkpointRecord, checkpointPaths, error) {
	paths, err := s.Paths(id)
	if err != nil {
		return checkpointRecord{}, checkpointPaths{}, err
	}
	data, err := os.ReadFile(paths.Meta)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkpointRecord{}, checkpointPaths{}, exit(2, "checkpoint %s not found", id)
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
	return record, paths, nil
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

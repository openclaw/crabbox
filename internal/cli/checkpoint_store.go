package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const checkpointIDPrefix = "chk_"

type CheckpointRecord struct {
	ID               string                      `json:"id"`
	ParentID         string                      `json:"parentId,omitempty"`
	Name             string                      `json:"name,omitempty"`
	Notes            string                      `json:"notes,omitempty"`
	CreatedAt        string                      `json:"createdAt"`
	CreatedBy        string                      `json:"createdBy,omitempty"`
	Repo             CheckpointRepo              `json:"repo"`
	Lease            CheckpointLease             `json:"lease,omitempty"`
	Run              CheckpointRun               `json:"run,omitempty"`
	Workspace        CheckpointWorkspace         `json:"workspace,omitempty"`
	Artifacts        []CheckpointArtifact        `json:"artifacts,omitempty"`
	ProviderSnapshot *CheckpointProviderSnapshot `json:"providerSnapshot,omitempty"`
}

type CheckpointRepo struct {
	Root      string `json:"root,omitempty"`
	Name      string `json:"name,omitempty"`
	RemoteURL string `json:"remoteUrl,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Head      string `json:"head,omitempty"`
	BaseRef   string `json:"baseRef,omitempty"`
}

type CheckpointLease struct {
	ID          string `json:"id,omitempty"`
	Slug        string `json:"slug,omitempty"`
	Provider    string `json:"provider,omitempty"`
	TargetOS    string `json:"targetOS,omitempty"`
	WindowsMode string `json:"windowsMode,omitempty"`
	Class       string `json:"class,omitempty"`
	ServerType  string `json:"serverType,omitempty"`
}

type CheckpointRun struct {
	ID          string `json:"id,omitempty"`
	ExitCode    *int   `json:"exitCode,omitempty"`
	DurationMs  int64  `json:"durationMs,omitempty"`
	Command     string `json:"command,omitempty"`
	LogPath     string `json:"logPath,omitempty"`
	ResultsPath string `json:"resultsPath,omitempty"`
}

type CheckpointWorkspace struct {
	ManifestPath string `json:"manifestPath,omitempty"`
	PatchPath    string `json:"patchPath,omitempty"`
	ArchivePath  string `json:"archivePath,omitempty"`
	ChangedFiles int    `json:"changedFiles,omitempty"`
	DeletedFiles int    `json:"deletedFiles,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
}

type CheckpointArtifact struct {
	Path string `json:"path,omitempty"`
	URL  string `json:"url,omitempty"`
	Type string `json:"type,omitempty"`
}

type CheckpointProviderSnapshot struct {
	Provider string `json:"provider,omitempty"`
	ID       string `json:"id,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

type checkpointStore struct {
	dir string
}

func defaultCheckpointStore() (checkpointStore, error) {
	stateDir, err := crabboxStateDir()
	if err != nil {
		return checkpointStore{}, err
	}
	return checkpointStore{dir: filepath.Join(stateDir, "checkpoints")}, nil
}

func (s checkpointStore) Create(record CheckpointRecord) (CheckpointRecord, error) {
	if strings.TrimSpace(record.ID) == "" {
		id, err := newCheckpointID()
		if err != nil {
			return CheckpointRecord{}, err
		}
		record.ID = id
	}
	record.ID = strings.TrimSpace(record.ID)
	if err := validateCheckpointID(record.ID); err != nil {
		return CheckpointRecord{}, err
	}
	record.ParentID = strings.TrimSpace(record.ParentID)
	if record.ParentID != "" {
		if err := validateCheckpointID(record.ParentID); err != nil {
			return CheckpointRecord{}, fmt.Errorf("parent id: %w", err)
		}
	}
	if strings.TrimSpace(record.CreatedAt) == "" {
		record.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if _, err := time.Parse(time.RFC3339, record.CreatedAt); err != nil {
		return CheckpointRecord{}, exit(2, "checkpoint createdAt must be RFC3339: %v", err)
	}
	path := s.path(record.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return CheckpointRecord{}, exit(2, "create checkpoint directory: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		return CheckpointRecord{}, exit(2, "checkpoint %s already exists", record.ID)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return CheckpointRecord{}, exit(2, "stat checkpoint %s: %v", path, err)
	}
	if err := writeCheckpointJSON(path, record); err != nil {
		return CheckpointRecord{}, err
	}
	return record, nil
}

func (s checkpointStore) Read(id string) (CheckpointRecord, error) {
	id = strings.TrimSpace(id)
	if err := validateCheckpointID(id); err != nil {
		return CheckpointRecord{}, err
	}
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return CheckpointRecord{}, exit(2, "checkpoint %s not found", id)
	}
	if err != nil {
		return CheckpointRecord{}, exit(2, "read checkpoint %s: %v", id, err)
	}
	var record CheckpointRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return CheckpointRecord{}, exit(2, "parse checkpoint %s: %v", id, err)
	}
	if record.ID == "" {
		record.ID = id
	}
	return record, nil
}

func (s checkpointStore) List() ([]CheckpointRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, exit(2, "read checkpoints directory: %v", err)
	}
	records := make([]CheckpointRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		record, err := s.Read(id)
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

func (s checkpointStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func writeCheckpointJSON(path string, record CheckpointRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*.tmp")
	if err != nil {
		return exit(2, "create checkpoint temp file: %v", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return exit(2, "write checkpoint %s: %v", path, err)
	}
	if err := tmp.Close(); err != nil {
		return exit(2, "close checkpoint %s: %v", path, err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return exit(2, "chmod checkpoint %s: %v", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return exit(2, "write checkpoint %s: %v", path, err)
	}
	return nil
}

func newCheckpointID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return checkpointIDPrefix + hex.EncodeToString(raw[:]), nil
}

func validateCheckpointID(id string) error {
	if !strings.HasPrefix(id, checkpointIDPrefix) || len(id) <= len(checkpointIDPrefix) {
		return exit(2, "checkpoint id must start with %s", checkpointIDPrefix)
	}
	for _, r := range strings.TrimPrefix(id, checkpointIDPrefix) {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return exit(2, "checkpoint id contains unsafe character %q", r)
	}
	return nil
}

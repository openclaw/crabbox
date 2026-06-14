package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	firecrackerStateSchemaVersion = 1
	firecrackerLeasesDirName      = "leases"
	firecrackerMetadataFile       = "metadata.json"
	firecrackerSocketFile         = "firecracker.sock"
	firecrackerLogFile            = "firecracker.log"
	firecrackerRootFSFile         = "rootfs.ext4"
	firecrackerCloudInitFile      = "cidata.img"
	firecrackerNetNSFile          = "netns"
	firecrackerCNICacheDirName    = "cni-cache"
)

type leaseStatePaths struct {
	Dir       string
	Metadata  string
	Socket    string
	Log       string
	RootFS    string
	CloudInit string
	NetNS     string
	CNICache  string
}

type leaseStateRecord struct {
	SchemaVersion   int               `json:"schemaVersion"`
	LeaseID         string            `json:"leaseID"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	VMID            string            `json:"vmid"`
	Provider        string            `json:"provider"`
	StateDir        string            `json:"stateDir"`
	SocketPath      string            `json:"socketPath"`
	LogPath         string            `json:"logPath"`
	RootFSPath      string            `json:"rootfsPath"`
	CloudInitPath   string            `json:"cloudInitPath"`
	NetNSPath       string            `json:"netnsPath"`
	CNICacheDir     string            `json:"cniCacheDir"`
	GuestIP         string            `json:"guestIP,omitempty"`
	SSHUser         string            `json:"sshUser"`
	SSHPort         string            `json:"sshPort"`
	BinaryPath      string            `json:"binaryPath"`
	KernelPath      string            `json:"kernelPath"`
	SourceRootFS    string            `json:"sourceRootfs"`
	CNINetwork      string            `json:"cniNetwork"`
	CNIConfDir      string            `json:"cniConfDir"`
	CNIBinDir       string            `json:"cniBinDir"`
	PID             int               `json:"pid,omitempty"`
	ProcessStarted  string            `json:"processStarted,omitempty"`
	BootID          string            `json:"bootID,omitempty"`
	DeleteOnRelease bool              `json:"deleteOnRelease"`
	Labels          map[string]string `json:"labels,omitempty"`
	CreatedAt       string            `json:"createdAt"`
	UpdatedAt       string            `json:"updatedAt"`
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func (b *backend) firecrackerStateRoot() (string, error) {
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(stateDir, providerName)
	if err := ensurePrivateDir(root); err != nil {
		return "", exit(2, "create firecracker state directory: %v", err)
	}
	leasesDir := filepath.Join(root, firecrackerLeasesDirName)
	if err := ensurePrivateDir(leasesDir); err != nil {
		return "", exit(2, "create firecracker leases directory: %v", err)
	}
	return root, nil
}

func (b *backend) leasePaths(leaseID string) (leaseStatePaths, error) {
	root, err := b.stateRoot()
	if err != nil {
		return leaseStatePaths{}, err
	}
	leasesDir := filepath.Join(root, firecrackerLeasesDirName)
	if err := ensurePrivateDir(leasesDir); err != nil {
		return leaseStatePaths{}, exit(2, "create firecracker leases directory: %v", err)
	}
	dir := filepath.Join(leasesDir, leaseID)
	return leaseStatePaths{
		Dir:       dir,
		Metadata:  filepath.Join(dir, firecrackerMetadataFile),
		Socket:    filepath.Join(dir, firecrackerSocketFile),
		Log:       filepath.Join(dir, firecrackerLogFile),
		RootFS:    filepath.Join(dir, firecrackerRootFSFile),
		CloudInit: filepath.Join(dir, firecrackerCloudInitFile),
		NetNS:     filepath.Join(dir, firecrackerNetNSFile),
		CNICache:  filepath.Join(dir, firecrackerCNICacheDirName),
	}, nil
}

func (b *backend) ensureLeaseDir(leaseID string) (leaseStatePaths, error) {
	paths, err := b.leasePaths(leaseID)
	if err != nil {
		return leaseStatePaths{}, err
	}
	if err := ensurePrivateDir(paths.Dir); err != nil {
		return leaseStatePaths{}, exit(2, "create firecracker lease directory %s: %v", paths.Dir, err)
	}
	return paths, nil
}

func (b *backend) writeStateRecord(record leaseStateRecord) error {
	if strings.TrimSpace(record.LeaseID) == "" {
		return exit(2, "write firecracker state requires a lease id")
	}
	paths, err := b.ensureLeaseDir(record.LeaseID)
	if err != nil {
		return err
	}
	record.SchemaVersion = firecrackerStateSchemaVersion
	record.Provider = providerName
	record.StateDir = paths.Dir
	record.SocketPath = paths.Socket
	record.LogPath = paths.Log
	record.RootFSPath = paths.RootFS
	record.CloudInitPath = paths.CloudInit
	record.NetNSPath = paths.NetNS
	record.CNICacheDir = paths.CNICache
	if record.Labels == nil {
		record.Labels = map[string]string{}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return exit(2, "encode firecracker state for %s: %v", record.LeaseID, err)
	}
	tmp, err := os.CreateTemp(paths.Dir, ".metadata-*.tmp")
	if err != nil {
		return exit(2, "create firecracker state temp file for %s: %v", record.LeaseID, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return exit(2, "write firecracker state for %s: %v", record.LeaseID, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return exit(2, "sync firecracker state for %s: %v", record.LeaseID, err)
	}
	if err := tmp.Close(); err != nil {
		return exit(2, "close firecracker state temp file for %s: %v", record.LeaseID, err)
	}
	if err := os.Rename(tmpPath, paths.Metadata); err != nil {
		return exit(2, "install firecracker state for %s: %v", record.LeaseID, err)
	}
	return nil
}

func (b *backend) readStateRecord(leaseID string) (leaseStateRecord, error) {
	paths, err := b.leasePaths(leaseID)
	if err != nil {
		return leaseStateRecord{}, err
	}
	return readStateRecordFile(paths.Metadata)
}

func readStateRecordFile(path string) (leaseStateRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return leaseStateRecord{}, err
	}
	var record leaseStateRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return leaseStateRecord{}, fmt.Errorf("decode firecracker state %s: %w", path, err)
	}
	if strings.TrimSpace(record.LeaseID) == "" {
		record.LeaseID = filepath.Base(filepath.Dir(path))
	}
	record.Provider = providerName
	record.StateDir = filepath.Dir(path)
	record.SocketPath = filepath.Join(record.StateDir, firecrackerSocketFile)
	record.LogPath = filepath.Join(record.StateDir, firecrackerLogFile)
	record.RootFSPath = filepath.Join(record.StateDir, firecrackerRootFSFile)
	record.CloudInitPath = filepath.Join(record.StateDir, firecrackerCloudInitFile)
	record.NetNSPath = filepath.Join(record.StateDir, firecrackerNetNSFile)
	record.CNICacheDir = filepath.Join(record.StateDir, firecrackerCNICacheDirName)
	if strings.TrimSpace(record.VMID) == "" {
		record.VMID = firstNonBlank(record.Name, record.LeaseID)
	}
	if record.Labels == nil {
		record.Labels = map[string]string{}
	}
	return record, nil
}

func (b *backend) listStateRecords() ([]leaseStateRecord, error) {
	root, err := b.stateRoot()
	if err != nil {
		return nil, err
	}
	leasesDir := filepath.Join(root, firecrackerLeasesDirName)
	entries, err := os.ReadDir(leasesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, exit(2, "read firecracker leases directory: %v", err)
	}
	records := make([]leaseStateRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(leasesDir, entry.Name(), firecrackerMetadataFile)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, exit(2, "stat firecracker state for %s: %v", entry.Name(), err)
		}
		record, err := readStateRecordFile(path)
		if err != nil {
			return nil, exit(2, "read firecracker state for %s: %v", entry.Name(), err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].LeaseID < records[j].LeaseID
	})
	return records, nil
}

func (b *backend) removeStateDir(record leaseStateRecord) error {
	if strings.TrimSpace(record.LeaseID) == "" {
		return exit(2, "remove firecracker state requires a lease id")
	}
	paths, err := b.leasePaths(record.LeaseID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(paths.Dir); err != nil {
		return exit(2, "remove firecracker state for %s: %v", record.LeaseID, err)
	}
	return nil
}

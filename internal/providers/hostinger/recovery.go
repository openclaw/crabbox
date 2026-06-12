package hostinger

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const hostingerRecoveryFile = "hostinger-recovery.json"

type hostingerRecoveryRecord struct {
	LeaseID  string `json:"leaseId"`
	Slug     string `json:"slug"`
	VMID     string `json:"vmId,omitempty"`
	Hostname string `json:"hostname"`
}

func hostingerRecoveryRecordPath(leaseID string) (string, error) {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(keyPath), hostingerRecoveryFile), nil
}

func writeHostingerRecoveryRecord(record hostingerRecoveryRecord) error {
	path, err := hostingerRecoveryRecordPath(record.LeaseID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			_ = os.Remove(tmp)
			return err
		}
		if retryErr := os.Rename(tmp, path); retryErr != nil {
			_ = os.Remove(tmp)
			return retryErr
		}
	}
	return nil
}

func removeHostingerRecoveryRecord(leaseID string) {
	path, err := hostingerRecoveryRecordPath(leaseID)
	if err == nil {
		_ = os.Remove(path)
	}
}

func findHostingerRecoveryRecord(vm hostingerVM) (hostingerRecoveryRecord, bool, error) {
	probe, err := testboxKeyPath("cbx_hostinger_recovery_probe")
	if err != nil {
		return hostingerRecoveryRecord{}, false, err
	}
	root := filepath.Dir(filepath.Dir(probe))
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return hostingerRecoveryRecord{}, false, nil
	}
	if err != nil {
		return hostingerRecoveryRecord{}, false, err
	}
	var match hostingerRecoveryRecord
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), hostingerRecoveryFile)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			continue
		}
		var record hostingerRecoveryRecord
		if err := json.Unmarshal(data, &record); err != nil {
			continue
		}
		if record.LeaseID != entry.Name() {
			continue
		}
		if record.VMID != vm.IDString() && (record.VMID != "" || record.Hostname != vm.NameValue()) {
			continue
		}
		if match.LeaseID != "" {
			return hostingerRecoveryRecord{}, false, fmt.Errorf("multiple hostinger recovery records match vps %s", vm.IDString())
		}
		match = record
	}
	return match, match.LeaseID != "", nil
}

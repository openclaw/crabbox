package utm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// Execution can hold a shared core claim fence. Store idle activity separately
// from claims and ownership journals so concurrent commands need no claim write.
type activityStamp struct {
	VMID  string    `json:"vmId"`
	Token string    `json:"token"`
	At    time.Time `json:"at"`
}

func activityPath(id string) (string, error) {
	path, err := recordPath(id)
	if err != nil {
		return "", err
	}
	return path + ".activity", nil
}
func (b *backend) stampActivity(ctx context.Context, r record) error {
	unlock, err := lockActivity(ctx, r.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	journalPath, err := recordPath(r.LeaseID)
	if err != nil {
		return err
	}
	journal, err := os.ReadFile(journalPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var current record
	if err = json.Unmarshal(journal, &current); err != nil {
		return err
	}
	if current.VMID != r.VMID || current.Token != r.Token {
		return fmt.Errorf("UTM SSH activity journal identity changed")
	}
	if current.State != "ready" {
		return nil
	}
	path, err := activityPath(r.LeaseID)
	if err != nil {
		return err
	}
	at := core.ClockNow(b.rt.Clock).UTC()
	if data, readErr := os.ReadFile(path); readErr == nil {
		var previous activityStamp
		if err = json.Unmarshal(data, &previous); err != nil {
			return err
		}
		if previous.VMID != r.VMID || previous.Token != r.Token {
			return fmt.Errorf("UTM SSH activity ownership mismatch")
		}
		if !at.After(previous.At) {
			return nil
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	data, err := json.Marshal(activityStamp{VMID: r.VMID, Token: r.Token, At: at})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".activity-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func (b *backend) BeginSSHRunActivity(ctx context.Context, lease core.LeaseTarget) (func(), error) {
	r, err := b.lookup(lease.LeaseID)
	if err != nil {
		return nil, err
	}
	if r.State != "ready" || lease.Server.Provider != "utm" || lease.Server.CloudID != r.VMID {
		return nil, fmt.Errorf("UTM SSH activity requires a completed exact owned lease")
	}
	if err = b.stampActivity(ctx, r); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := b.stampActivity(finish, r); err != nil {
				fmt.Fprintf(b.rt.Stderr, "warning: UTM SSH idle activity: %v\n", err)
			}
		})
	}, nil
}
func (b *backend) activityLabels(r record, labels map[string]string) (map[string]string, error) {
	path, err := activityPath(r.LeaseID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return labels, nil
	}
	if err != nil {
		return nil, err
	}
	var stamp activityStamp
	if err = json.Unmarshal(data, &stamp); err != nil {
		return nil, err
	}
	if stamp.VMID != r.VMID || stamp.Token != r.Token {
		return nil, fmt.Errorf("UTM SSH activity identity does not match ownership")
	}
	last, _ := strconv.ParseInt(labels["last_touched_at"], 10, 64)
	if !stamp.At.After(time.Unix(last, 0)) {
		return labels, nil
	}
	// Existing ttl_secs and created_at retain the original absolute deadline.
	return core.TouchDirectLeaseLabels(labels, b.cfg, "", stamp.At), nil
}
func (b *backend) cleanupEligible(r record, s core.Server) (bool, string, error) {
	labels, err := b.activityLabels(r, s.Labels)
	if err != nil {
		return false, "", err
	}
	s.Labels = labels
	eligible, reason := core.ShouldCleanupServer(s, core.ClockNow(b.rt.Clock).UTC())
	if r.State != "ready" && !r.Keep {
		eligible = true
		reason = "incomplete allocation"
	}
	return eligible, reason, nil
}

func lockActivity(ctx context.Context, id string) (func(), error) {
	return shared.LockOperation(ctx, "utm", id+".utm-activity.lock", "UTM SSH idle activity")
}

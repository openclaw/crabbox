package utm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validUUID(s string) bool { return uuidPattern.MatchString(s) }

// A pending record precedes clone. The before set and unguessable name allow
// recovery after interruption without treating a name as deletion authority.
// Once resolved, VMID is immutable and all destructive calls use only that UUID.
type record struct {
	Acquisition        string `json:"acquisition,omitempty"`
	Backend            string `json:"backend,omitempty"`
	TargetOS           string `json:"targetOS,omitempty"`
	ImageVersion       string `json:"imageVersion,omitempty"`
	ImageBuild         string `json:"imageBuild,omitempty"`
	ImagePath          string `json:"imagePath,omitempty"`
	Address            string `json:"address,omitempty"`
	SSHHostKey         string `json:"sshHostKey,omitempty"`
	GeneratedKey       bool   `json:"generatedKey,omitempty"`
	InstallComplete    bool   `json:"installComplete,omitempty"`
	FirstBootSubmitted bool   `json:"firstBootSubmitted,omitempty"`
	AccountComplete    bool   `json:"accountComplete,omitempty"`
	SSHReady           bool   `json:"sshReady,omitempty"`

	Version      int               `json:"version"`
	LeaseID      string            `json:"leaseId"`
	Slug         string            `json:"slug"`
	AppPath      string            `json:"appPath"`
	SourceID     string            `json:"sourceId"`
	OwnedName    string            `json:"ownedName"`
	Token        string            `json:"token"`
	Before       []string          `json:"before,omitempty"`
	BundlePath   string            `json:"bundlePath,omitempty"`
	VMID         string            `json:"vmId,omitempty"`
	Architecture string            `json:"architecture"`
	MAC          string            `json:"mac"`
	Port         int               `json:"port"`
	User         string            `json:"user"`
	SSHKey       string            `json:"sshKey"`
	WorkRoot     string            `json:"workRoot"`
	Configured   bool              `json:"configured"`
	State        string            `json:"state"`
	Keep         bool              `json:"keep"`
	Created      time.Time         `json:"created"`
	Labels       map[string]string `json:"labels"`
}

func (r record) isMacOS() bool  { return r.Version == 2 && r.Acquisition == "ipsw" }
func (r record) marker() string { return "crabbox:utm:v1:" + r.LeaseID + ":" + r.Token }
func (r record) validate() error {
	if (r.Version != 1 && r.Version != 2) || r.LeaseID == "" || strings.ContainsAny(r.LeaseID, "/\\.\x00") || r.Token == "" || r.OwnedName != "crabbox-utm-"+r.Token || !filepath.IsAbs(r.AppPath) {
		return fmt.Errorf("invalid UTM ownership journal")
	}
	if r.isMacOS() {
		if r.Backend != "apple" || r.TargetOS != core.TargetMacOS || r.SourceID != "" || r.Port != 22 || r.Architecture != "aarch64" || r.ImageVersion == "" || r.ImageBuild == "" || !r.GeneratedKey {
			return fmt.Errorf("invalid UTM macOS ownership journal")
		}
		if r.Configured && (r.MAC == "" || r.BundlePath == "") {
			return fmt.Errorf("incomplete UTM macOS identity")
		}
		if r.FirstBootSubmitted && !r.InstallComplete || r.AccountComplete && !r.FirstBootSubmitted || r.SSHReady && (!r.AccountComplete || r.SSHHostKey == "" || r.Address == "") {
			return fmt.Errorf("invalid UTM macOS provisioning phases")
		}
	} else if r.Version != 1 || r.Acquisition != "" && r.Acquisition != "template" || !validUUID(r.SourceID) || r.Port < 1024 || r.Port > 65535 {
		return fmt.Errorf("invalid UTM template ownership journal")
	}
	if r.VMID != "" && (!validUUID(r.VMID) || strings.EqualFold(r.VMID, r.SourceID)) {
		return fmt.Errorf("UTM journal has unsafe clone UUID")
	}
	for _, id := range r.Before {
		if strings.EqualFold(id, r.VMID) && r.VMID != "" {
			return fmt.Errorf("UTM clone UUID was present before allocation")
		}
	}
	return nil
}
func stateDir() (string, error) {
	dir, err := core.CrabboxStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "utm"), nil
}
func recordPath(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, "/\\.\x00") {
		return "", fmt.Errorf("invalid UTM lease identity")
	}
	dir, err := stateDir()
	return filepath.Join(dir, id+".json"), err
}
func saveRecord(r record) error {
	if err := r.validate(); err != nil {
		return err
	}
	path, err := recordPath(r.LeaseID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func removeRecord(id string) error {
	path, err := recordPath(id)
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func readRecords() ([]record, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []record{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing symlink UTM journal")
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var r record
		if err = json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		if err = r.validate(); err != nil {
			return nil, err
		}
		if e.Name() != r.LeaseID+".json" {
			return nil, fmt.Errorf("UTM journal filename mismatch")
		}
		out = append(out, r)
	}
	return out, nil
}
func (b *backend) records() ([]record, error) {
	all, err := readRecords()
	if err != nil {
		return nil, err
	}
	out := []record{}
	for _, r := range all {
		if r.AppPath == b.cfg.UTM.AppPath {
			out = append(out, r)
		}
	}
	return out, nil
}
func (b *backend) lookup(id string) (record, error) {
	records, err := b.records()
	if err != nil {
		return record{}, err
	}
	var matches []record
	for _, r := range records {
		if id == r.LeaseID || id == r.Slug || id == r.VMID {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		return record{}, core.Exit(4, "UTM lease %q: expected one exact ownership journal, found %d", id, len(matches))
	}
	return matches[0], nil
}
func (b *backend) lock(ctx context.Context) (func(), error) {
	return shared.LockOperation(ctx, "utm", "utm-host.lock", "UTM local allocation/lifecycle")
}
func randomToken() (string, error) {
	var v [16]byte
	if _, err := rand.Read(v[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(v[:]), nil
}
func allocateNetwork(records []record, vms []vm) (string, int, error) {
	usedMAC := map[string]bool{}
	usedPort := map[int]bool{}
	for _, r := range records {
		usedMAC[strings.ToLower(r.MAC)] = true
		usedPort[r.Port] = true
	}
	for _, v := range vms {
		for _, n := range v.Configuration.Networks {
			usedMAC[strings.ToLower(n.Address)] = true
			for _, f := range n.Forwards {
				usedPort[f.HostPort] = true
			}
		}
	}
	var mac string
	for i := 0; i < 100; i++ {
		var v [6]byte
		if _, err := rand.Read(v[:]); err != nil {
			return "", 0, err
		}
		v[0] = (v[0] | 2) & 0xfe
		mac = net.HardwareAddr(v[:]).String()
		if !usedMAC[mac] {
			break
		}
		mac = ""
	}
	if mac == "" {
		return "", 0, fmt.Errorf("could not allocate a distinct UTM MAC")
	}
	for i := 0; i < 100; i++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return "", 0, err
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		if port >= 1024 && !usedPort[port] {
			return mac, port, nil
		}
	}
	return "", 0, fmt.Errorf("could not reserve a distinct loopback SSH port")
}
func (b *backend) reconcile(ctx context.Context, r *record, vms []vm) error {
	if r.VMID != "" {
		return nil
	}
	before := map[string]bool{}
	for _, id := range r.Before {
		before[strings.ToLower(id)] = true
	}
	var candidates []vm
	for _, v := range vms {
		if r.isMacOS() && (v.Backend != "apple" || v.Configuration.Notes != r.marker()) {
			continue
		}
		if v.Name == r.OwnedName && !before[strings.ToLower(v.ID)] && !strings.EqualFold(v.ID, r.SourceID) {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) != 1 {
		return fmt.Errorf("UTM lease=%s clone discovery unresolved: found %d new VMs with exact allocated name %q; journal retained, do not delete by name", r.LeaseID, len(candidates), r.OwnedName)
	}
	r.VMID = candidates[0].ID
	r.State = "created"
	return saveRecord(*r)
}

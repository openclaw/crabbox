package utm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func validateMacOSVM(v vm, r record) error {
	if v.ID != r.VMID || v.Backend != "apple" || v.Configuration.Notes != r.marker() || len(v.Configuration.Networks) != 1 {
		return fmt.Errorf("UTM Apple VM ownership/configuration changed")
	}
	n := v.Configuration.Networks[0]
	if n.Mode != "shared" || !strings.EqualFold(n.Address, r.MAC) || len(n.Forwards) != 0 {
		return fmt.Errorf("UTM Apple VM shared NIC identity changed")
	}
	return nil
}
func (b *backend) validateMacOSBundle(ctx context.Context, path string, r record, v vm) error {
	if err := validateMacOSVM(v, r); err != nil {
		return err
	}
	var info bundleInformation
	var networks []bundleNetwork
	var backend, architecture, guestOS string
	for _, read := range []func() error{
		func() error { return b.plistExtract(ctx, path, "Information", "json", &info) },
		func() error { return b.plistExtract(ctx, path, "Backend", "raw", &backend) },
		func() error { return b.plistExtract(ctx, path, "System.Architecture", "raw", &architecture) },
		func() error { return b.plistExtract(ctx, path, "System.Boot.OperatingSystem", "raw", &guestOS) },
		func() error { return b.plistExtract(ctx, path, "Network", "json", &networks) },
	} {
		if err := read(); err != nil {
			return err
		}
	}
	if !strings.EqualFold(info.UUID, r.VMID) || info.Name != v.Name || info.Notes != r.marker() || backend != "Apple" || architecture != "aarch64" || guestOS != "macOS" || len(networks) != 1 || networks[0].Mode != "Shared" || !strings.EqualFold(networks[0].MacAddress, r.MAC) {
		return fmt.Errorf("UTM Apple bundle UUID/marker/OS/network does not match journal and live configuration")
	}
	return nil
}
func (b *backend) validateOwnedMacOS(ctx context.Context, r record, vms []vm, stopped bool) error {
	v, ok := findVM(vms, r.VMID)
	if !ok || stopped && v.Status != "stopped" {
		return fmt.Errorf("UTM owned Apple VM is absent or not stopped")
	}
	path, err := b.cloneBundle(ctx, r)
	if err != nil {
		return err
	}
	return b.validateMacOSBundle(ctx, path, r, v)
}
func macOSReadyCheck(r record) string {
	// The first NIC's address is only a host-side hint until this authenticated
	// check proves the guest's own platform and assigned NIC identity.
	return fmt.Sprintf(`test "$(uname -s)" = Darwin && test "$(uname -m)" = arm64 && (test "$(/usr/bin/sw_vers -productVersion)" = %s || test "$(/usr/bin/sw_vers -productVersion)" = %s) && test "$(/usr/bin/sw_vers -buildVersion)" = %s && command -v bash >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null && command -v perl >/dev/null && perl -e 'exit 0' && /sbin/ifconfig -a | /usr/bin/awk -v wanted=%s '$1 == "ether" && tolower($2) == wanted { found=1 } END { exit !found }'`, core.ShellQuote(r.ImageVersion), core.ShellQuote(strings.TrimSuffix(r.ImageVersion, ".0")), core.ShellQuote(r.ImageBuild), core.ShellQuote(strings.ToLower(r.MAC)))
}
func (b *backend) createMacOS(ctx context.Context, r record, image macOSImage) error {
	script := fmt.Sprintf(`tell application %s
 set imageFile to POSIX file %s
 set createdVM to make new virtual machine with properties {backend:apple, configuration:{name:%s, notes:%s, os:macOS, macRecoveryIpsw:imageFile, memory:%d, cpu cores:%d}}
 return id of createdVM
end tell`, scriptString(r.AppPath), scriptString(r.ImagePath), scriptString(r.OwnedName), scriptString(r.marker()), image.MemoryMiB, image.CPUCount)
	_, err := b.command(ctx, "/usr/bin/osascript", []string{"-"}, strings.NewReader(script))
	return err
}
func (b *backend) installMacOS(ctx context.Context, r *record) error {
	r.State = "installing"
	if err := saveRecord(*r); err != nil {
		return err
	}
	script := "tell application " + scriptString(r.AppPath) + "\n set targetVM to first virtual machine whose id is " + scriptString(r.VMID) + "\n install targetVM\nend tell"
	if _, err := b.command(ctx, "/usr/bin/osascript", []string{"-"}, strings.NewReader(script)); err != nil {
		return err
	}
	var previous int = -1
	for {
		pending, progress, err := b.queryMacOSInstall(ctx, *r)
		if err != nil {
			return fmt.Errorf("UTM installation status unavailable or failed; ownership and installation phase retained: %w", err)
		}
		percent := int(progress * 100)
		if percent != previous {
			fmt.Fprintf(b.rt.Stderr, "UTM macOS installation lease=%s progress=%d%% pending=%t\n", r.LeaseID, percent, pending)
			previous = percent
		}
		if !pending {
			if progress != 1 {
				return fmt.Errorf("UTM installation ended without completed progress")
			}
			if err = b.waitObservation(ctx, r.VMID, false); err != nil {
				return err
			}
			vms, err := b.inventory(ctx)
			if err != nil {
				return err
			}
			if err = b.validateOwnedMacOS(ctx, *r, vms, true); err != nil {
				return err
			}
			r.InstallComplete = true
			r.State = "installed"
			return saveRecord(*r)
		}
		if err := waitPoll(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}
func waitPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (b *backend) queryMacOSInstall(ctx context.Context, r record) (bool, float64, error) {
	script := "tell application " + scriptString(r.AppPath) + "\n set targetVM to first virtual machine whose id is " + scriptString(r.VMID) + "\n set p to query install targetVM\n return (installing of p as text) & tab & (progress of p as text)\nend tell"
	out, err := b.command(ctx, "/usr/bin/osascript", []string{"-"}, strings.NewReader(script))
	if err != nil {
		return false, 0, err
	}
	fields := strings.Fields(out.Stdout)
	if len(fields) != 2 {
		return false, 0, fmt.Errorf("invalid UTM installation progress response")
	}
	pending, e1 := strconv.ParseBool(fields[0])
	progress, e2 := strconv.ParseFloat(strings.ReplaceAll(fields[1], ",", "."), 64)
	if e1 != nil || e2 != nil || !(progress >= 0 && progress <= 1) {
		return false, 0, fmt.Errorf("invalid UTM installation progress values")
	}
	return pending, progress, nil
}
func (b *backend) firstBootMacOS(ctx context.Context, r *record, password string) error {
	if !r.isMacOS() || !r.Configured || !r.InstallComplete || r.FirstBootSubmitted || r.AccountComplete || r.SSHReady || r.State != "installed" {
		return fmt.Errorf("UTM first boot is ineligible or has already been submitted; provisioning credentials will not be replayed")
	}
	vms, err := b.inventory(ctx)
	if err != nil {
		return err
	}
	if err = b.validateOwnedMacOS(ctx, *r, vms, true); err != nil {
		return err
	}
	// UTM also validates no pending IPSW, no saved suspension and no recovery
	// before actually starting. The journal proves we never started this owned VM.
	r.FirstBootSubmitted = true
	r.State = "first-boot-submitted"
	if err = saveRecord(*r); err != nil {
		return err
	}
	script := fmt.Sprintf(`tell application %s
 set targetVM to first virtual machine whose id is %s
 start targetVM provisioning {username:%s, full name:%s, password:%s, automatic login:false, remote login:true}
 return "submitted"
end tell`, scriptString(r.AppPath), scriptString(r.VMID), scriptString(r.User), scriptString(r.User), scriptString(password))
	out, err := b.privateCommand(ctx, "first boot", core.LocalCommandRequest{Name: "/usr/bin/osascript", Args: []string{"-"}, Stdin: strings.NewReader(script)})
	if err != nil {
		return err
	}
	if strings.TrimSpace(out.Stdout) != "submitted" {
		return fmt.Errorf("UTM first boot submission was not acknowledged; credential-bearing diagnostics suppressed")
	}
	return nil
}
func (b *backend) queryMacOSAddress(ctx context.Context, r record) (string, error) {
	script := "tell application " + scriptString(r.AppPath) + "\n set targetVM to first virtual machine whose id is " + scriptString(r.VMID) + "\n set addresses to query ip targetVM\n if (count of addresses) is 0 then return \"\"\n return item 1 of addresses\nend tell"
	out, err := b.command(ctx, "/usr/bin/osascript", []string{"-"}, strings.NewReader(script))
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(out.Stdout)
	if raw == "" {
		return "", nil
	}
	ip := net.ParseIP(raw)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return "", fmt.Errorf("UTM returned an invalid guest IPv4 address")
	}
	return ip.String(), nil
}
func (b *backend) refreshMacOSAddress(ctx context.Context, r *record) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.UTM.StartupTimeout)
	defer cancel()
	for {
		address, err := b.queryMacOSAddress(ctx, *r)
		if err != nil {
			return err
		}
		if address != "" {
			r.Address = address
			return saveRecord(*r)
		}
		if err = waitPoll(ctx, time.Second); err != nil {
			return fmt.Errorf("UTM guest IPv4 ARP discovery timed out: %w", err)
		}
	}
}
func (b *backend) acquireMacOS(ctx context.Context, req core.AcquireRequest) (lease core.LeaseTarget, err error) {
	if err = b.macOSInput(); err != nil {
		return lease, err
	}
	if _, err = b.storageRoot(); err != nil {
		return lease, err
	}
	password, err := randomToken()
	if err != nil {
		return lease, err
	}
	image, err := inspectMacOSImage(b, ctx, password)
	if err != nil {
		return lease, err
	}
	if err = image.validate(); err != nil {
		return lease, err
	}
	unlock, err := b.lock(ctx)
	if err != nil {
		return lease, err
	}
	defer unlock()
	vms, err := b.inventory(ctx)
	if err != nil {
		return lease, err
	}
	records, err := b.records()
	if err != nil {
		return lease, err
	}
	servers := make([]core.Server, 0, len(records))
	for _, r := range records {
		servers = append(servers, b.project(r, vms))
	}
	id := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(id, req.RequestedSlug, servers)
	if err != nil {
		return lease, err
	}
	token, err := randomToken()
	if err != nil {
		return lease, err
	}
	r := record{Version: 2, Acquisition: "ipsw", Backend: "apple", TargetOS: core.TargetMacOS, LeaseID: id, Slug: slug, AppPath: b.cfg.UTM.AppPath, OwnedName: "crabbox-utm-" + token, Token: token, Architecture: "aarch64", Port: 22, User: b.cfg.UTM.User, WorkRoot: b.cfg.UTM.WorkRoot, State: "creating", Keep: req.Keep, Created: core.ClockNow(b.rt.Clock).UTC(), GeneratedKey: true, ImagePath: b.cfg.UTM.IPSW, ImageVersion: image.GuestVersion, ImageBuild: image.GuestBuild}
	r.Labels = core.DirectLeaseLabels(b.cfg, id, slug, "utm", "", req.Keep, r.Created)
	r.Labels["target"] = core.TargetMacOS
	r.Labels["work_root"] = r.WorkRoot
	r.Labels["ssh_user"] = r.User
	r.Labels["ssh_port"] = "22"
	r.Labels["architecture"] = "arm64"
	r.Labels["utm_acquisition"] = "ipsw"
	for _, v := range vms {
		r.Before = append(r.Before, v.ID)
		if v.Name == r.OwnedName {
			return lease, fmt.Errorf("UTM allocated name already exists")
		}
	}
	if err = saveRecord(r); err != nil {
		return lease, err
	}
	retired := false
	defer func() {
		if err != nil && !retired {
			r.State = "recovery"
			r.Labels["state"] = "recovery"
			err = errors.Join(err, saveRecord(r))
			fmt.Fprintf(b.rt.Stderr, "UTM acquisition incomplete lease=%s vm=%s installed=%t firstBootSubmitted=%t account=%t ssh=%t; journal retained; use crabbox stop --provider utm %s\n", r.LeaseID, core.Blank(r.VMID, "unresolved"), r.InstallComplete, r.FirstBootSubmitted, r.AccountComplete, r.SSHReady, r.LeaseID)
		}
	}()
	publicKey, err := b.generateMacOSKey(&r)
	if err != nil {
		return lease, err
	}
	if err = saveRecord(r); err != nil {
		return lease, err
	}
	// Record resources that an Apple event may finish creating after cancellation.
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	createErr := b.createMacOS(createCtx, r, image)
	after, inventoryErr := b.inventory(createCtx)
	if inventoryErr == nil {
		inventoryErr = b.reconcile(createCtx, &r, after)
	}
	cancel()
	if createErr != nil || inventoryErr != nil {
		return lease, errors.Join(createErr, inventoryErr)
	}
	lease = b.target(r, after)
	if req.OnAcquired != nil {
		if err = req.OnAcquired(lease); err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			cleanupErr := b.releaseRecord(cleanupCtx, &r, "", nil)
			cleanupCancel()
			retired = cleanupErr == nil
			return lease, errors.Join(err, cleanupErr)
		}
	}
	if err = ctx.Err(); err != nil {
		return lease, err
	}
	v, ok := findVM(after, r.VMID)
	if !ok || len(v.Configuration.Networks) != 1 {
		return lease, fmt.Errorf("UTM created Apple VM lacks one NIC")
	}
	mac, parseErr := net.ParseMAC(v.Configuration.Networks[0].Address)
	if parseErr != nil || len(mac) != 6 || mac[0]&1 != 0 {
		return lease, fmt.Errorf("UTM created Apple VM has invalid MAC")
	}
	r.MAC = strings.ToLower(mac.String())
	for _, other := range after {
		if other.ID != r.VMID {
			for _, n := range other.Configuration.Networks {
				if strings.EqualFold(n.Address, r.MAC) {
					return lease, fmt.Errorf("UTM created Apple MAC is not unique")
				}
			}
		}
	}
	for _, other := range records {
		if strings.EqualFold(other.MAC, r.MAC) {
			return lease, fmt.Errorf("UTM created Apple MAC conflicts with ownership journal")
		}
	}
	path, err := b.cloneBundle(ctx, r)
	if err != nil {
		return lease, err
	}
	r.BundlePath = filepath.Dir(path)
	if err = b.validateOwnedMacOS(ctx, r, after, true); err != nil {
		return lease, err
	}
	r.Configured = true
	r.State = "configured"
	r.Labels["utm_mac"] = r.MAC
	if err = saveRecord(r); err != nil {
		return lease, err
	}
	installCtx, installCancel := context.WithTimeout(ctx, b.cfg.UTM.InstallTimeout)
	err = b.installMacOS(installCtx, &r)
	installCancel()
	if err != nil {
		return lease, err
	}
	if err = b.firstBootMacOS(ctx, &r, password); err != nil {
		return lease, err
	}
	startup, startupCancel := context.WithTimeout(ctx, b.cfg.UTM.StartupTimeout)
	defer startupCancel()
	if err = b.refreshMacOSAddress(startup, &r); err != nil {
		return lease, err
	}
	if err = bootstrapMacOSGuest(b, startup, &r, password, publicKey); err != nil {
		return lease, err
	}
	password = ""
	if err = b.pinMacOSHostKey(r); err != nil {
		return lease, err
	}
	r.AccountComplete = true
	r.State = "account-configured"
	if err = saveRecord(r); err != nil {
		return lease, err
	}
	lease = b.target(r, after)
	if err = waitForSSHReady(startup, &lease.SSH, b.rt.Stderr, "UTM macOS authenticated continuing SSH", b.cfg.UTM.StartupTimeout); err != nil {
		return lease, err
	}
	r.SSHReady = true
	r.State = "ready"
	r.Labels = core.TouchDirectLeaseLabels(r.Labels, b.cfg, "ready", core.ClockNow(b.rt.Clock).UTC())
	if err = saveRecord(r); err != nil {
		return lease, err
	}
	lease = b.target(r, after)
	lease.Server.Status = "ready"
	claim, err := b.claimTarget(lease, slug, req.Repo.Root, req.Reclaim, core.LeaseClaim{}, false)
	if err != nil {
		return lease, err
	}
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	fmt.Fprintf(b.rt.Stderr, "UTM macOS ready lease=%s vm=%s installed=true account=true ssh=true\n", r.LeaseID, r.VMID)
	return lease, nil
}

// Only derived per-lease files can be removed; journal-supplied arbitrary paths
// and an operator's template key or source IPSW are never deletion authority.
func (b *backend) removeGeneratedKey(r record) error {
	if !r.isMacOS() || !r.GeneratedKey {
		return nil
	}
	dir, err := macOSKeyDir(r)
	if err != nil {
		return err
	}
	if r.SSHKey != "" && r.SSHKey != filepath.Join(dir, "id_ed25519") {
		recordedDir := filepath.Dir(r.SSHKey)
		if !filepath.IsAbs(r.SSHKey) || filepath.Clean(r.SSHKey) != r.SSHKey || filepath.Base(r.SSHKey) != "id_ed25519" || filepath.Base(recordedDir) != filepath.Base(dir) {
			return fmt.Errorf("UTM generated key path does not match lease ownership")
		}
		// State-root aliases can spell the same owned directory differently.
		// Compare their provider-state parents without following the recorded
		// key leaf, which may already be absent after interrupted cleanup.
		recordedParent, recordedErr := os.Stat(filepath.Dir(recordedDir))
		currentParent, currentErr := os.Stat(filepath.Dir(dir))
		if recordedErr != nil || currentErr != nil || !recordedParent.IsDir() || !currentParent.IsDir() || !os.SameFile(recordedParent, currentParent) {
			return fmt.Errorf("UTM generated key path does not match lease ownership")
		}
		if info, e := os.Lstat(recordedDir); e != nil && !os.IsNotExist(e) {
			return e
		} else if e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("UTM generated key directory identity changed")
		}
	}
	if info, e := os.Lstat(dir); os.IsNotExist(e) {
		return nil
	} else if e != nil {
		return e
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("UTM generated key directory identity changed")
	}
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts"} {
		if err = os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err = os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Claim-fenced exec already holds the shared core claim fence. Resolve only
// observations here: taking the host lock, saving the journal, or rewriting the
// core claim would invert release's host-lock -> exclusive-claim-fence order.
func (b *backend) macOSExecLeaseTarget(ctx context.Context, r record, vms []vm) (core.LeaseTarget, error) {
	if !r.isMacOS() || r.State != "ready" || !r.SSHReady || !r.AccountComplete {
		return core.LeaseTarget{}, fmt.Errorf("UTM macOS exec requires a completed ready lease")
	}
	v, ok := findVM(vms, r.VMID)
	if !ok || v.Status != "started" {
		return core.LeaseTarget{}, fmt.Errorf("UTM macOS exec guest is absent or not running")
	}
	if err := b.validateOwnedMacOS(ctx, r, vms, false); err != nil {
		return core.LeaseTarget{}, err
	}
	address, err := b.queryMacOSAddress(ctx, r)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if address == "" {
		return core.LeaseTarget{}, fmt.Errorf("UTM macOS exec guest IPv4 ARP address is unavailable; retry after guest networking is ready")
	}
	r.Address = address
	return b.target(r, vms), nil
}

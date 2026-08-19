package boxd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSSHConfig = `# user's own entries
Host github.com
    User git

# BEGIN boxd (managed by boxd; do not edit)
Host crabbox-cbx-a1b2c3d4e5f6.boxd crabbox-cbx-a1b2c3d4e5f6.boxd.sh
    HostName crabbox-cbx-a1b2c3d4e5f6.boxd.sh
    Port 14022
    User boxd
    IdentityFile "/home/user/.config/boxd/id_ed25519_dev_user"
    IdentitiesOnly yes
    ServerAliveInterval 30
Host pending-vm.boxd pending-vm.boxd.sh
    HostName pending-vm.boxd.sh
    User boxd
    IdentityFile "/home/user/.config/boxd/id_ed25519_dev_user"
# END boxd
`

func TestSelectBoxdSSHEntry(t *testing.T) {
	entry, found, err := selectBoxdSSHEntry(testSSHConfig, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if entry.HostName != "crabbox-cbx-a1b2c3d4e5f6.boxd.sh" || entry.Port != "14022" || entry.User != "boxd" {
		t.Fatalf("entry=%#v", entry)
	}
	if entry.IdentityFile != "/home/user/.config/boxd/id_ed25519_dev_user" {
		t.Fatalf("identity=%q (quotes not stripped?)", entry.IdentityFile)
	}
	if _, found, _ := selectBoxdSSHEntry(testSSHConfig, "missing.boxd.sh"); found {
		t.Fatal("missing host reported as found")
	}
}

func TestSSHTargetFromEntryRequiresPortAndIdentity(t *testing.T) {
	entry, found, err := selectBoxdSSHEntry(testSSHConfig, "pending-vm.boxd.sh")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if _, err := sshTargetFromEntry(entry, "pending-vm.boxd.sh", ""); err == nil || !strings.Contains(err.Error(), "no Port") {
		t.Fatalf("port-less entry must be rejected, err=%v", err)
	}
	entry.Port = "14022"
	entry.IdentityFile = ""
	if _, err := sshTargetFromEntry(entry, "pending-vm.boxd.sh", ""); err == nil || !strings.Contains(err.Error(), "IdentityFile") {
		t.Fatalf("identity-less entry must be rejected, err=%v", err)
	}
}

func TestSSHTargetFromEntryDisablesMultiplexing(t *testing.T) {
	entry, _, _ := selectBoxdSSHEntry(testSSHConfig, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh")
	target, err := sshTargetFromEntry(entry, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh", "")
	if err != nil {
		t.Fatal(err)
	}
	if !target.NoControlMaster {
		t.Fatal("boxd targets must disable ControlMaster (the proxy keeps per-connection session state)")
	}
	if target.User != "boxd" || target.Port != "14022" || target.TargetOS != "linux" {
		t.Fatalf("target=%#v", target)
	}
}

func TestSelectBoxdSSHEntryAmbiguous(t *testing.T) {
	doubled := testSSHConfig + "\nHost crabbox-cbx-a1b2c3d4e5f6.boxd.sh\n    Port 1\n"
	if _, _, err := selectBoxdSSHEntry(doubled, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh"); err == nil {
		t.Fatal("ambiguous entries must error")
	}
}

// TestSSHTargetPinsVendorHostKey pins the host-trust contract: the target
// carries the vendor-managed known_hosts file, and a `[host]:port` pin in it
// is surfaced as SSHHostKey — which the shared SSH transport turns into
// StrictHostKeyChecking=yes with GlobalKnownHostsFile/KnownHostsCommand
// disabled, so a mismatched edge key is REJECTED rather than trusted on
// first use.
func TestSSHTargetPinsVendorHostKey(t *testing.T) {
	entry, _, _ := selectBoxdSSHEntry(testSSHConfig, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	pin := "[crabbox-cbx-a1b2c3d4e5f6.boxd.sh]:14022 ssh-ed25519 AAAATESTKEY boxd-hosts\n[other.boxd.sh]:22222 ssh-ed25519 AAAAOTHER\n"
	if err := os.WriteFile(knownHosts, []byte(pin), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := sshTargetFromEntry(entry, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh", knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if target.KnownHostsFile != knownHosts {
		t.Fatalf("KnownHostsFile=%q want the vendor-managed file %q", target.KnownHostsFile, knownHosts)
	}
	if target.SSHHostKey != "ssh-ed25519 AAAATESTKEY" {
		t.Fatalf("SSHHostKey=%q not read from the vendor pin", target.SSHHostKey)
	}
}

// TestSSHTargetWithoutPinKeepsVendorKnownHosts pins the fallback: with no pin
// for this host:port, SSHHostKey stays empty but verification still runs
// against the vendor-managed file (never a key-directory-derived one), where
// accept-new rejects a CHANGED key for known entries.
func TestSSHTargetWithoutPinKeepsVendorKnownHosts(t *testing.T) {
	entry, _, _ := selectBoxdSSHEntry(testSSHConfig, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("[unrelated.boxd.sh]:1 ssh-ed25519 AAAAX\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := sshTargetFromEntry(entry, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh", knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if target.KnownHostsFile != knownHosts || target.SSHHostKey != "" {
		t.Fatalf("target=%#v want vendor known_hosts with empty SSHHostKey", target)
	}
}

func TestPinnedHostKeyMatchesExactHostPort(t *testing.T) {
	data := "# comment\n[a.boxd.sh]:100,[b.boxd.sh]:200 ssh-ed25519 KEY1\n[a.boxd.sh]:999 ssh-rsa KEY2\n"
	if got := pinnedHostKey(data, "b.boxd.sh", "200"); got != "ssh-ed25519 KEY1" {
		t.Fatalf("comma-list match=%q", got)
	}
	if got := pinnedHostKey(data, "a.boxd.sh", "999"); got != "ssh-rsa KEY2" {
		t.Fatalf("second entry=%q", got)
	}
	if got := pinnedHostKey(data, "a.boxd.sh", "100"); got != "ssh-ed25519 KEY1" {
		t.Fatalf("first entry=%q", got)
	}
	if got := pinnedHostKey(data, "missing.boxd.sh", "1"); got != "" {
		t.Fatalf("missing entry=%q", got)
	}
}

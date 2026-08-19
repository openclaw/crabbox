package boxd

import (
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
	if _, err := sshTargetFromEntry(entry, "pending-vm.boxd.sh"); err == nil || !strings.Contains(err.Error(), "no Port") {
		t.Fatalf("port-less entry must be rejected, err=%v", err)
	}
	entry.Port = "14022"
	entry.IdentityFile = ""
	if _, err := sshTargetFromEntry(entry, "pending-vm.boxd.sh"); err == nil || !strings.Contains(err.Error(), "IdentityFile") {
		t.Fatalf("identity-less entry must be rejected, err=%v", err)
	}
}

func TestSSHTargetFromEntryDisablesMultiplexing(t *testing.T) {
	entry, _, _ := selectBoxdSSHEntry(testSSHConfig, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh")
	target, err := sshTargetFromEntry(entry, "crabbox-cbx-a1b2c3d4e5f6.boxd.sh")
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

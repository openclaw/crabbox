package cli

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Extract the emitted commands so filesystem tests exercise the real renderer.
func cloudInitOwnershipBlock(t *testing.T, cfg Config, cacheRoot string) string {
	t.Helper()
	var doc struct {
		WriteFiles []struct {
			Path, Content string
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(awsUserData(cfg, "ssh-ed25519 fixture")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, file := range doc.WriteFiles {
		if file.Path != "/usr/local/lib/crabbox-bootstrap.sh" {
			continue
		}
		start := strings.Index(file.Content, "mkdir -p "+shellQuote(cfg.WorkRoot))
		if start < 0 {
			t.Fatal("missing workspace ownership preparation")
		}
		end := strings.Index(file.Content[start:], "install -d /var/lib/crabbox")
		if end < 0 {
			t.Fatal("missing ownership preparation boundary")
		}
		return strings.NewReplacer(
			shellQuote("/var/cache/crabbox"), shellQuote(cacheRoot),
			"/var/cache/crabbox/pnpm", shellQuote(cacheRoot+"/pnpm"),
			"/var/cache/crabbox/npm", shellQuote(cacheRoot+"/npm"),
		).Replace(file.Content[start : start+end])
	}
	t.Fatal("missing bootstrap script")
	return ""
}

func TestCloudInitFiltersNumericOwnership(t *testing.T) {
	for _, image := range []string{"stock", "custom AMI", "snapshot"} {
		t.Run(image, func(t *testing.T) {
			cfg := baseConfig()
			cfg.WorkRoot = "/work/fixture's tree"
			if image == "custom AMI" {
				cfg.AWSAMI = "ami-fixture"
			} else if image == "snapshot" {
				cfg.AWSSnapshot = "snap-fixture"
			}
			script := cloudInitOwnershipBlock(t, cfg, "/tmp/cache")
			for _, want := range []string{
				"PATH=/usr/sbin:/usr/bin:/sbin:/bin python3 -I - 'crabbox' '/work/fixture'\\''s tree' '/tmp/cache'",
				"uid = pwd.getpwnam(sys.argv[1]).pw_uid",
				"gid = grp.getgrnam(sys.argv[1]).gr_gid",
				"if info.st_uid != uid or info.st_gid != gid:",
				"os.chown(name, uid, gid, dir_fd=directory_fd, follow_symlinks=False)",
				"os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd",
			} {
				if !strings.Contains(script, want) {
					t.Errorf("missing %q in emitted ownership block:\n%s", want, script)
				}
			}
			if runtime.GOOS != "windows" {
				cmd := exec.Command("bash", "-n")
				cmd.Stdin = strings.NewReader(script)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("bash syntax: %v: %s", err, out)
				}
			}
		})
	}
}

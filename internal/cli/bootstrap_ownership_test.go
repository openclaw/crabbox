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
		return strings.ReplaceAll(file.Content[start:start+end], "/var/cache/crabbox", shellQuote(cacheRoot))
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
				"PATH=/usr/sbin:/usr/bin:/sbin:/bin find -P '/work/fixture'\\''s tree' '/tmp/cache'",
				`\( ! -user 'crabbox' -o ! -group 'crabbox' \)`,
				"-execdir chown -h -- 'crabbox:crabbox' {} +",
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

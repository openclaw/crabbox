package tencentcloud

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestTencentCloudUserDataFitsProviderLimit(t *testing.T) {
	cfg := cfgForRun(core.BaseConfig())
	cfg.Tailscale.Enabled, cfg.Tailscale.AuthKey = true, "synthetic-auth-key"
	tags := leaseTags(cfg, "cbx_123456abcdef", "fixture", "provisioning", true, time.Unix(1800000000, 0))
	req, err := buildRunInstanceRequest(cfg, "cbx_123456abcdef", "fixture", "ssh-ed25519 fixture", tags)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.UserData) > 16*1024 {
		t.Fatalf("encoded user-data is %d bytes; limit is 16384", len(req.UserData))
	}
	compressed, err := base64.StdEncoding.DecodeString(req.UserData)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	want := core.CloudInitUserData(core.WithGuestLeaseExpiry(cfg, labelsFromTags(tags)), "ssh-ed25519 fixture")
	if string(decoded) != want || !gz.ModTime.IsZero() {
		t.Fatal("gzip changed cloud-init content or included a timestamp")
	}
	t.Logf("encoded user-data=%d limit=16384", len(req.UserData))
}

func TestTencentCloudUserDataRejectsOversizedPayload(t *testing.T) {
	var entropy bytes.Buffer
	for i := range 1024 {
		digest := sha256.Sum256([]byte(strconv.Itoa(i)))
		entropy.Write(digest[:])
	}
	_, err := tencentCloudUserData(cfgForRun(core.BaseConfig()), "ssh-rsa "+base64.StdEncoding.EncodeToString(entropy.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "user-data limit") {
		t.Fatalf("oversized payload error=%v", err)
	}
}

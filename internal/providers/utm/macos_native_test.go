package utm

import (
	"context"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Explicit fixture opt-in makes this a read-only native integration check. No
// UTM app commands are sent; all temporary helper files are removed by preflight.
func TestNativeMacOSImageAndAccountPreflight(t *testing.T) {
	path := os.Getenv("CRABBOX_TEST_UTM_IPSW")
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || path == "" {
		t.Skip("requires Apple Silicon macOS 27+, full Xcode and CRABBOX_TEST_UTM_IPSW")
	}
	cfg := core.BaseConfig()
	cfg.UTM.IPSW = path
	cfg.UTM.User = "crabbox"
	b := &backend{cfg: cfg, rt: core.RuntimeForProviderOperation(io.Discard), hostOS: "darwin", hostArch: "arm64"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	password, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	image, err := b.nativeMacOSPreflight(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	if err = image.validate(); err != nil {
		t.Fatal(err)
	}
	t.Logf("native image/account validated host=%s guest=%s build=%s CPU=%d memoryMiB=%d", image.HostVersion, image.GuestVersion, image.GuestBuild, image.CPUCount, image.MemoryMiB)
	cfg.UTM.User = "invalid:user"
	b.cfg = cfg
	if _, err = b.nativeMacOSPreflight(ctx, password); err == nil {
		t.Fatal("native SDK accepted invalid account")
	}
}

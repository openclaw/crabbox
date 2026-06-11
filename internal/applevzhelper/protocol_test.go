package applevzhelper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtocolPathsAndStatusHelpers(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "crabbox-apple-vz")
	name := "crabbox-cbx123-demo"

	if got := InstancesDir(root); got != filepath.Join(root, "instances") {
		t.Fatalf("InstancesDir=%q", got)
	}
	if got := CacheDir(root); got != filepath.Join(root, "cache") {
		t.Fatalf("CacheDir=%q", got)
	}
	if got := DownloadsDir(root); got != filepath.Join(root, "cache", "downloads") {
		t.Fatalf("DownloadsDir=%q", got)
	}
	if got := ImagesDir(root); got != filepath.Join(root, "cache", "images") {
		t.Fatalf("ImagesDir=%q", got)
	}
	if got := InstanceDir(root, name); got != filepath.Join(root, "instances", name) {
		t.Fatalf("InstanceDir=%q", got)
	}
	if got := MetadataPath(root, name); got != filepath.Join(root, "instances", name, MetadataFileName) {
		t.Fatalf("MetadataPath=%q", got)
	}
	if got := PreparationPath(root, name); got != filepath.Join(root, "instances", name, PreparationFileName) {
		t.Fatalf("PreparationPath=%q", got)
	}
	for label, path := range map[string]string{
		"disk":    DiskPath(root, name),
		"seed":    SeedPath(root, name),
		"efi":     EFIPath(root, name),
		"console": ConsoleLogPath(root, name),
		"helper":  HelperLogPath(root, name),
	} {
		if path == "" {
			t.Fatalf("%s path is empty", label)
		}
	}
	if !IsRunningStatus(StatusStarting) || !IsRunningStatus(StatusRunning) {
		t.Fatal("starting and running should be active states")
	}
	if IsRunningStatus(StatusStopped) || IsRunningStatus("") {
		t.Fatal("stopped and empty should not be active states")
	}
}

func TestRedactImageRefRemovesRemoteCredentials(t *testing.T) {
	for _, input := range []string{
		"https://alice:secret@example.test/images/ubuntu.img?token=private#fragment",
		"HTTPS://alice:secret@example.test/images/ubuntu.img?token=private#fragment",
		"https://downloads.example.test/bearer-token/ubuntu.img",
	} {
		got := RedactImageRef(input)
		if got != "<remote-image>" {
			t.Fatalf("RedactImageRef(%q)=%q", input, got)
		}
	}
	if got := RedactImageRef("/tmp/ubuntu.img"); got != "/tmp/ubuntu.img" {
		t.Fatalf("local RedactImageRef=%q", got)
	}
	if got := RedactImageRef("HTTPS://example.test/%zz?token=private"); got != "<remote-image>" {
		t.Fatalf("invalid remote RedactImageRef=%q", got)
	}
}

func TestImageIdentityUsesRemoteChecksum(t *testing.T) {
	checksum := strings.Repeat("a", 64)
	if got := ImageIdentity("https://downloads.example.test/bearer-token/ubuntu.img", checksum); got != "remote:sha256:aaaaaaaaaaaa" {
		t.Fatalf("ImageIdentity=%q", got)
	}
	if got := ImageIdentity("https://example.test/image.img", "invalid"); got != "<remote-image>" {
		t.Fatalf("invalid checksum ImageIdentity=%q", got)
	}
	if got := ImageIdentity("/tmp/ubuntu.img", checksum); got != "/tmp/ubuntu.img" {
		t.Fatalf("local ImageIdentity=%q", got)
	}
}

func TestIsRemoteImageRef(t *testing.T) {
	for _, input := range []string{"https://example.test/image.img", "HTTP://example.test/image.img"} {
		if !IsRemoteImageRef(input) {
			t.Fatalf("IsRemoteImageRef(%q)=false", input)
		}
	}
	if IsRemoteImageRef("/tmp/image.img") {
		t.Fatal("local path classified as remote")
	}
}

func TestSanitizeDiagnosticTextEscapesTerminalControls(t *testing.T) {
	input := "ready\n\t\x1b]0;owned\x07\r\u009b31m\u202etrusted"
	want := "ready\n\t\\x1b]0;owned\\x07\\x0d\\x9b31m\\u202etrusted"
	if got := SanitizeDiagnosticText(input); got != want {
		t.Fatalf("SanitizeDiagnosticText=%q, want %q", got, want)
	}
}

func TestEnsurePrivateDirTightensExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("private dir mode=%#o, want 0700", got)
	}
}

func TestValidatePOSIXAccountName(t *testing.T) {
	for _, user := range []string{"crabbox", "_runner", "ci.user-1", strings.Repeat("a", 32)} {
		if err := ValidatePOSIXAccountName(user); err != nil {
			t.Fatalf("ValidatePOSIXAccountName(%q): %v", user, err)
		}
	}
	for _, user := range []string{"", "yes\nroot", "user:name", "has space", strings.Repeat("a", 33)} {
		if err := ValidatePOSIXAccountName(user); err == nil {
			t.Fatalf("ValidatePOSIXAccountName(%q) succeeded", user)
		}
	}
}

func TestValidatePOSIXWorkRoot(t *testing.T) {
	for _, workRoot := range []string{"/work/crabbox", "/var/lib/my-app_1.2", "/.cache/crabbox"} {
		if err := ValidatePOSIXWorkRoot(workRoot); err != nil {
			t.Fatalf("ValidatePOSIXWorkRoot(%q): %v", workRoot, err)
		}
	}
	for _, workRoot := range []string{
		"",
		"/",
		"relative/work",
		`C:\work\crabbox`,
		"/work/../root",
		"/work/./root",
		"/work//root",
		"/work/root/",
		"/work/has space",
		"/work/$(touch)",
		"/work/quote'",
	} {
		if err := ValidatePOSIXWorkRoot(workRoot); err == nil {
			t.Fatalf("ValidatePOSIXWorkRoot(%q) succeeded", workRoot)
		}
	}
}

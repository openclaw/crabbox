package applevzhelper

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	ManagedHelperName       = "crabbox-apple-vz-helper"
	ManagedHelperUseLockEnv = "CRABBOX_APPLE_VZ_HELPER_USE_LOCK"

	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopping = "stopping"
	StatusStopped  = "stopped"
	StatusError    = "error"

	GuestSSHPort     uint32 = 22
	HostVSOCKSSHPort uint32 = 2222

	MetadataFileName    = "instance.json"
	HelperLogFileName   = "helper.log"
	ConsoleLogFileName  = "console.log"
	DiskFileName        = "disk.raw"
	SeedFileName        = "seed.img"
	EFIFileName         = "efi-variable-store.bin"
	PreparationFileName = "preparing.json"
)

func ManagedHelperUseLockPath(helperPath string) string {
	return helperPath + ".use.lock"
}

const HelperEntitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>com.apple.security.virtualization</key>
  <true/>
  <key>com.apple.security.network.client</key>
  <true/>
  <key>com.apple.security.network.server</key>
  <true/>
</dict>
</plist>
`

type Instance struct {
	Name                 string    `json:"name"`
	LeaseID              string    `json:"leaseID"`
	Slug                 string    `json:"slug"`
	Status               string    `json:"status"`
	Error                string    `json:"error,omitempty"`
	Image                string    `json:"image"`
	SourceImage          string    `json:"sourceImage,omitempty"`
	SSHUser              string    `json:"sshUser"`
	WorkRoot             string    `json:"workRoot"`
	CPUs                 int       `json:"cpus"`
	MemoryMiB            int       `json:"memoryMiB"`
	DiskGiB              int       `json:"diskGiB"`
	PID                  int       `json:"pid,omitempty"`
	PIDStartedAt         string    `json:"pidStartedAt,omitempty"`
	SSHHost              string    `json:"sshHost,omitempty"`
	SSHPort              int       `json:"sshPort,omitempty"`
	DiskPath             string    `json:"diskPath,omitempty"`
	SeedPath             string    `json:"seedPath,omitempty"`
	EFIVariableStorePath string    `json:"efiVariableStorePath,omitempty"`
	ConsoleLogPath       string    `json:"consoleLogPath,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type StartResponse struct {
	Instance Instance `json:"instance"`
}

type InspectResponse struct {
	Instance Instance `json:"instance"`
}

type ListResponse struct {
	Instances []Instance `json:"instances"`
}

type DeleteResponse struct {
	Deleted  bool     `json:"deleted"`
	Instance Instance `json:"instance"`
}

type DoctorResponse struct {
	Status    string            `json:"status"`
	Message   string            `json:"message"`
	Details   map[string]string `json:"details,omitempty"`
	Instances int               `json:"instances"`
}

type ImageRequest struct {
	Image  string `json:"image"`
	SHA256 string `json:"sha256,omitempty"`
}

var (
	validPOSIXAccountName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9._-]*$`)
	validPOSIXWorkRoot    = regexp.MustCompile(`^/[a-zA-Z0-9._-]+(?:/[a-zA-Z0-9._-]+)*$`)
)

func ValidatePOSIXAccountName(value string) error {
	normalized := strings.TrimSpace(value)
	if len(normalized) > 32 || !validPOSIXAccountName.MatchString(normalized) {
		return fmt.Errorf("%q is not a valid POSIX account name", value)
	}
	return nil
}

func ValidatePOSIXWorkRoot(value string) error {
	normalized := strings.TrimSpace(value)
	if path.Clean(normalized) != normalized || !validPOSIXWorkRoot.MatchString(normalized) {
		return fmt.Errorf("%q is not a safe absolute POSIX path; use letters, numbers, dots, underscores, and hyphens in each segment", value)
	}
	return nil
}

func RedactImageRef(value string) string {
	value = strings.TrimSpace(value)
	_, remote := remoteImageURL(value)
	if !remote {
		return value
	}
	return "<remote-image>"
}

func ImageIdentity(value, expectedSHA256 string) string {
	value = strings.TrimSpace(value)
	if _, remote := remoteImageURL(value); !remote {
		return value
	}
	checksum := strings.ToLower(strings.TrimSpace(expectedSHA256))
	checksum = strings.TrimPrefix(checksum, "sha256:")
	if len(checksum) == 64 {
		if _, err := hex.DecodeString(checksum); err == nil {
			return "remote:sha256:" + checksum[:12]
		}
	}
	return "<remote-image>"
}

func IsRemoteImageRef(value string) bool {
	_, remote := remoteImageURL(value)
	return remote
}

func SanitizeDiagnosticText(value string) string {
	var sanitized strings.Builder
	sanitized.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n' || r == '\t':
			sanitized.WriteRune(r)
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&sanitized, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&sanitized, `\u%04x`, r)
			default:
				fmt.Fprintf(&sanitized, `\U%08x`, r)
			}
		default:
			sanitized.WriteRune(r)
		}
	}
	return sanitized.String()
}

func remoteImageURL(value string) (*url.URL, bool) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		return parsed, true
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return nil, true
	}
	return nil, false
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func InstancesDir(stateRoot string) string {
	return filepath.Join(stateRoot, "instances")
}

func CacheDir(stateRoot string) string {
	return filepath.Join(stateRoot, "cache")
}

func DownloadsDir(stateRoot string) string {
	return filepath.Join(CacheDir(stateRoot), "downloads")
}

func ImagesDir(stateRoot string) string {
	return filepath.Join(CacheDir(stateRoot), "images")
}

func HelperDir(stateRoot string) string {
	return filepath.Join(stateRoot, "helper")
}

func InstanceDir(stateRoot, name string) string {
	return filepath.Join(InstancesDir(stateRoot), name)
}

func MetadataPath(stateRoot, name string) string {
	return filepath.Join(InstanceDir(stateRoot, name), MetadataFileName)
}

func PreparationPath(stateRoot, name string) string {
	return filepath.Join(InstanceDir(stateRoot, name), PreparationFileName)
}

func DiskPath(stateRoot, name string) string {
	return filepath.Join(InstanceDir(stateRoot, name), DiskFileName)
}

func SeedPath(stateRoot, name string) string {
	return filepath.Join(InstanceDir(stateRoot, name), SeedFileName)
}

func EFIPath(stateRoot, name string) string {
	return filepath.Join(InstanceDir(stateRoot, name), EFIFileName)
}

func ConsoleLogPath(stateRoot, name string) string {
	return filepath.Join(InstanceDir(stateRoot, name), ConsoleLogFileName)
}

func HelperLogPath(stateRoot, name string) string {
	return filepath.Join(InstanceDir(stateRoot, name), HelperLogFileName)
}

func IsRunningStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusStarting, StatusRunning:
		return true
	default:
		return false
	}
}

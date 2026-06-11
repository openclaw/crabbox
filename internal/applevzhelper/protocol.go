package applevzhelper

import (
	"path/filepath"
	"strings"
	"time"
)

const (
	ManagedHelperName = "crabbox-apple-vz-helper"

	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopping = "stopping"
	StatusStopped  = "stopped"
	StatusError    = "error"

	GuestSSHPort      uint32 = 22
	GuestVSOCKSSHPort uint32 = 2222

	MetadataFileName   = "instance.json"
	ConsoleLogFileName = "console.log"
	DiskFileName       = "disk.raw"
	SeedFileName       = "seed.img"
	EFIFileName        = "efi-variable-store.bin"
)

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

func IsRunningStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusStarting, StatusRunning:
		return true
	default:
		return false
	}
}

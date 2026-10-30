package utm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type macOSImage struct {
	GuestVersion     string `json:"guestVersion"`
	GuestBuild       string `json:"guestBuild"`
	HostVersion      string `json:"hostVersion"`
	CPUCount         int    `json:"cpuCount"`
	MemoryMiB        uint64 `json:"memoryMiB"`
	Supported        bool   `json:"supported"`
	AccountValidated bool   `json:"accountValidated"`
	Error            string `json:"error"`
}

var inspectMacOSImage = (*backend).nativeMacOSPreflight

func versionParts(s string) ([3]int, error) {
	var v [3]int
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return v, fmt.Errorf("invalid macOS version")
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return v, fmt.Errorf("invalid macOS version")
		}
		v[i] = n
	}
	return v, nil
}
func (image macOSImage) validate() error {
	if image.Error != "" {
		messages := map[string]string{
			"host-version":               "fresh macOS requires host macOS 27 or later; use a prepared template on older hosts",
			"host-architecture":          "fresh macOS requires an Apple Silicon host",
			"guest-version":              "fresh macOS requires a macOS 27 or later guest; older guests need a prepared template",
			"guest-newer-than-host":      "IPSW macOS version is newer than the host; update the host or choose an older compatible IPSW",
			"virtualization-unavailable": "Apple Virtualization is unavailable on this host",
			"account-invalid":            "generated guest account options failed native SDK validation",
			"image-incompatible":         "IPSW hardware model is incompatible with this host",
			"image-unreadable":           "native Virtualization could not read the IPSW",
			"resources-unavailable":      "restore image exceeds host CPU or memory limits",
		}
		if message, ok := messages[image.Error]; ok {
			return fmt.Errorf("UTM macOS preflight: %s", message)
		}
		return fmt.Errorf("UTM macOS native preflight failed")
	}
	guest, e1 := versionParts(image.GuestVersion)
	host, e2 := versionParts(image.HostVersion)
	if e1 != nil || e2 != nil || guest[0] < 27 || host[0] < 27 || !image.Supported || !image.AccountValidated || image.GuestBuild == "" || strings.ContainsAny(image.GuestBuild, "\r\n\x00") || image.CPUCount < 2 || image.MemoryMiB < 4096 {
		return fmt.Errorf("UTM macOS preflight returned incomplete or ineligible image/account metadata")
	}
	for i := range guest {
		if guest[i] > host[i] {
			return fmt.Errorf("UTM macOS restore image is newer than host")
		}
		if guest[i] < host[i] {
			break
		}
	}
	return nil
}

// Credential-bearing subprocess diagnostics never leave this boundary, even if
// an executable prints the password, quotes it, or embeds it in a wrapped error.
func (b *backend) privateCommand(ctx context.Context, operation string, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req.MaxCapturedOutputBytes = 64 << 10
	req.Stdout, req.Stderr = nil, nil
	result, err := b.rt.Exec.Run(ctx, req)
	if ctx.Err() != nil {
		return core.LocalCommandResult{}, fmt.Errorf("UTM %s: %w", operation, ctx.Err())
	}
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stderr) != "" {
		return core.LocalCommandResult{}, fmt.Errorf("UTM %s failed; credential-bearing diagnostics suppressed", operation)
	}
	return result, nil
}

func (b *backend) macOSInput() error {
	if b.cfg.UTM.Template != "" {
		return fmt.Errorf("utm.template and IPSW acquisition are mutually exclusive")
	}
	if b.cfg.Architecture != "" && b.cfg.Architecture != "arm64" {
		return fmt.Errorf("fresh macOS requires architecture arm64")
	}
	if b.cfg.UTM.IPSW == "" || !filepath.IsAbs(b.cfg.UTM.IPSW) {
		return fmt.Errorf("utm.ipsw must name an absolute local restore image")
	}
	info, err := os.Stat(b.cfg.UTM.IPSW)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("utm.ipsw must be an available regular local restore image")
	}
	f, err := os.Open(b.cfg.UTM.IPSW)
	if err != nil {
		return fmt.Errorf("utm.ipsw is unreadable")
	}
	return f.Close()
}
func (b *backend) doctorMacOS(ctx context.Context) (core.DoctorResult, error) {
	result := core.DoctorResult{Provider: "utm", Message: "fresh Apple macOS: native image/account inspection; temporary entitled helper requires full Xcode; no VM created or started", Checks: []core.DoctorCheck{}}
	add := func(name string, err error, ok string) {
		c := core.DoctorCheck{Check: name, Status: "ok", Message: ok}
		if err != nil {
			c.Status = "failed"
			c.Message = err.Error()
			result.Status = "failed"
		}
		result.Checks = append(result.Checks, c)
	}
	_, err := b.command(ctx, b.cfg.UTM.AppPath+"/Contents/MacOS/utmctl", []string{"--help"}, nil)
	add("cli", err, "bundled UTM CLI is available")
	_, err = b.inventory(ctx)
	add("application-control", err, "published UTM inventory is readable")
	_, err = b.storageRoot()
	add("storage-root", err, "owned VM bundle root is available")
	err = b.macOSInput()
	add("ipsw-input", err, "local operator-selected IPSW is readable; source is never modified or deleted")
	if err == nil {
		password, e := randomToken()
		if e != nil {
			return result, e
		}
		image, e := inspectMacOSImage(b, ctx, password)
		add("native-image-account", e, fmt.Sprintf("host macOS %s; compatible guest %s build %s; CPU=%d memoryMiB=%d; account validation completed", image.HostVersion, image.GuestVersion, image.GuestBuild, image.CPUCount, image.MemoryMiB))
	}
	result.Checks = append(result.Checks, core.DoctorCheck{Check: "workload-readiness", Status: "unchecked", Message: "installation, first-boot account/Remote Login, DHCP and authenticated key SSH require acquisition; no VM operations performed"})
	return result, nil
}

package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const architectureProbeTimeout = 15 * time.Second
const architectureProbeLimit = 256

var runArchitectureProbe = core.RunSSHOutputBounded

// uname describes the SSH execution environment, not bare-metal provenance.
const posixArchitectureProbe = `machine=$(uname -m 2>/dev/null) || machine=unknown
case "$machine" in
 x86_64|amd64) machine=amd64 ;;
 arm64|aarch64) machine=arm64 ;;
 *) machine=unknown ;;
esac
printf 'v1|%s|-|-|-\n' "$machine"`

const macArchitectureProbe = `machine=$(/usr/bin/uname -m 2>/dev/null) || machine=unknown
case "$machine" in
 x86_64|amd64) machine=amd64 ;;
 arm64|aarch64) machine=arm64 ;;
 *) machine=unknown ;;
esac
host=unknown
translated=unknown
if [ "$(/usr/bin/uname -s 2>/dev/null)" = Darwin ]; then
 arm=$(/usr/sbin/sysctl -n hw.optional.arm64 2>/dev/null) || arm=unknown
 cpu=$(/usr/sbin/sysctl -n hw.cputype 2>/dev/null) || cpu=unknown
 if [ "$arm" = 1 ]; then
  host=arm64
 elif [ "$cpu" = 7 ] || [ "$cpu" = 16777223 ]; then
  host=amd64
 fi
 rosetta=$(/usr/sbin/sysctl -n sysctl.proc_translated 2>/dev/null) || rosetta=unknown
 case "$rosetta" in
  1) translated=true; host=arm64 ;;
  0) translated=false ;;
  *) if [ "$host" = amd64 ] && [ "$machine" = amd64 ]; then translated=false; fi ;;
 esac
fi
printf 'v1|%s|%s|%s|%s\n' "$machine" "$host" "$machine" "$translated"`

// IsWow64Process2 returns native-machine independently of the PowerShell process.
// UNKNOWN processMachine does not prove native execution (notably x64 on ARM).
// ProcessArchitecture measures this process separately; OSArchitecture does not.
// Missing API/compiler/process-query access is unknown, with no env fallback.
const windowsArchitectureProbe = `$ErrorActionPreference = 'Stop'
try {
 Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class CrabboxArchitecture {
 [DllImport("kernel32.dll", SetLastError=true)]
 [return: MarshalAs(UnmanagedType.Bool)]
 public static extern bool IsWow64Process2(IntPtr process, out ushort processMachine, out ushort nativeMachine);
}
'@
 [ushort]$processMachine = 0
 [ushort]$nativeMachine = 0
 if (-not [CrabboxArchitecture]::IsWow64Process2([IntPtr]::new(-1), [ref]$processMachine, [ref]$nativeMachine)) { throw 'query failed' }
 function MachineName([ushort]$machine) {
  switch ($machine) { 34404 { 'amd64' } 43620 { 'arm64' } 332 { '386' } default { 'unknown' } }
 }
 $native = MachineName $nativeMachine
 $process = 'unknown'
 try {
 switch ([System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString()) {
  'X64' { $process = 'amd64' }
  'Arm64' { $process = 'arm64' }
  'X86' { $process = '386' }
 }
 } catch { $process = 'unknown' }
 $translated = 'unknown'
 if ($process -ne 'unknown' -and $native -ne 'unknown') {
  if ($process -ne $native -or $processMachine -ne 0) { $translated = 'true' } else { $translated = 'false' }
 }
 $architecture = $process
 if ($architecture -ne 'amd64' -and $architecture -ne 'arm64') { $architecture = 'unknown' }
 [Console]::WriteLine('v1|' + $architecture + '|' + $native + '|' + $process + '|' + $translated)
} catch {
 [Console]::WriteLine('v1|unknown|unknown|unknown|unknown')
}
`

type architectureObservation struct {
	architecture, host, process, translated string
}

func architectureProbe(target SSHTarget) (command, source, scope string) {
	if target.TargetOS == core.TargetWindows && target.WindowsMode == "normal" {
		return windowsArchitectureProbe, "ssh-iswow64process2", "powershell-process"
	}
	if target.TargetOS == core.TargetMacOS {
		return "/bin/sh -c " + core.ShellQuote(macArchitectureProbe), "ssh-uname-sysctl", "posix-process"
	}
	scope = "posix-environment"
	if target.TargetOS == core.TargetWindows {
		scope = "wsl-environment"
	}
	return "/bin/sh -c " + core.ShellQuote(posixArchitectureProbe), "ssh-uname", scope
}

func supportedArchitecture(value string) bool {
	return value == core.ArchitectureAMD64 || value == core.ArchitectureARM64
}

func parseArchitectureObservation(output string, detailed bool) (architectureObservation, error) {
	unknown := architectureObservation{architecture: "unknown"}
	fields := strings.Split(strings.TrimSpace(output), "|")
	if len(fields) != 5 || fields[0] != "v1" {
		return unknown, errors.New("malformed architecture evidence")
	}
	valid := func(s string) bool { return supportedArchitecture(s) || s == "unknown" }
	if !valid(fields[1]) {
		return unknown, errors.New("unsupported architecture evidence")
	}
	if !detailed {
		if fields[2] != "-" || fields[3] != "-" || fields[4] != "-" {
			return unknown, errors.New("malformed architecture evidence")
		}
		return architectureObservation{architecture: fields[1]}, nil
	}
	if (!valid(fields[2]) && fields[2] != "386") || (!valid(fields[3]) && fields[3] != "386") || (fields[4] != "true" && fields[4] != "false" && fields[4] != "unknown") {
		return unknown, errors.New("malformed architecture evidence")
	}
	if supportedArchitecture(fields[1]) && fields[1] != fields[3] {
		return unknown, errors.New("inconsistent process architecture evidence")
	}
	return architectureObservation{fields[1], fields[2], fields[3], fields[4]}, nil
}

func (b *staticLeaseBackend) observeArchitecture(ctx context.Context, lease *LeaseTarget) error {
	probeCtx, cancel := context.WithTimeout(ctx, architectureProbeTimeout)
	defer cancel()
	// Readiness selected this exact port. nil would silently re-enable port 22.
	lease.SSH.FallbackPorts = []string{}
	command, source, scope := architectureProbe(lease.SSH)
	output, err := runArchitectureProbe(probeCtx, lease.SSH, command, architectureProbeLimit)
	if probeCtx.Err() != nil {
		return fmt.Errorf("static SSH architecture probe: %w", probeCtx.Err())
	}
	// Only a pure successful-command overflow is unavailable evidence. A joined
	// cleanup/transport failure must still fail an unconstrained request.
	if err != nil && err != core.ErrSSHOutputLimit {
		return fmt.Errorf("static SSH architecture probe: %w", err)
	}
	detailed := source != "ssh-uname"
	observation, parseErr := parseArchitectureObservation(output, detailed)
	if err != nil {
		parseErr = errors.New("architecture evidence exceeded output limit")
	}
	if parseErr != nil {
		observation = architectureObservation{architecture: "unknown"}
	}
	if core.IsArchitectureExplicit(b.Cfg) {
		if parseErr != nil {
			return fmt.Errorf("static SSH architecture=%s assertion failed: %w", b.Cfg.Architecture, parseErr)
		}
		if !supportedArchitecture(observation.architecture) || observation.architecture != b.Cfg.Architecture ||
			(detailed && (observation.translated != "false" || observation.host != observation.process || !supportedArchitecture(observation.host))) {
			return fmt.Errorf("static SSH architecture=%s assertion failed: observed=%s host=%s process=%s translated=%s", b.Cfg.Architecture, observation.architecture, observation.host, observation.process, observation.translated)
		}
	}
	now := time.Now().UTC()
	if b.RT.Clock != nil {
		now = b.RT.Clock.Now().UTC()
	}
	clearArchitecture(&lease.Server)
	labels := lease.Server.Labels
	labels["architecture"] = observation.architecture
	labels["architecture_source"], labels["architecture_scope"] = source, scope
	// Label sanitization changes RFC3339 punctuation; milliseconds survive Touch.
	labels["architecture_version"], labels["architecture_observed_at"] = "1", strconv.FormatInt(now.UnixMilli(), 10)
	labels["architecture_endpoint"] = architectureEndpoint(lease.SSH)
	if detailed && parseErr == nil {
		labels["architecture_host"], labels["architecture_process"], labels["architecture_translated"] = observation.host, observation.process, observation.translated
	}
	if supportedArchitecture(observation.architecture) {
		lease.Server.ServerType.Architecture = observation.architecture
	}
	if parseErr != nil || !supportedArchitecture(observation.architecture) || (detailed && (observation.translated == "unknown" || !supportedArchitecture(observation.host))) {
		fmt.Fprintln(b.RT.Stderr, "warning: static SSH architecture evidence is unknown or unavailable; no native architecture assertion was made")
	}
	if detailed && supportedArchitecture(observation.host) && supportedArchitecture(observation.process) && observation.host != observation.process && observation.translated != "true" {
		fmt.Fprintln(b.RT.Stderr, "warning: static SSH host/process architecture contradicts translation evidence; no native architecture assertion was made")
	}
	fmt.Fprintf(b.RT.Stderr, "static SSH architecture observed=%s source=%s scope=%s version=1 observed_at=%s", observation.architecture, source, scope, labels["architecture_observed_at"])
	if detailed {
		fmt.Fprintf(b.RT.Stderr, " host=%s process=%s translated=%s", observation.host, observation.process, observation.translated)
	}
	fmt.Fprintln(b.RT.Stderr)
	return nil
}

func architectureEndpoint(target SSHTarget) string {
	// No credential material is persisted. Bind observations to the resolved route,
	// including user/port/target and transport selectors, not just a lease name.
	data, _ := json.Marshal([]string{target.Host, target.User, target.Port, target.TargetOS, target.WindowsMode, target.Key, target.CertificateFile, target.ProxyCommand, fmt.Sprint(target.SSHConfigProxy), target.HostKeyAlias, target.KnownHostsFile})
	digest := sha256.Sum256(data)
	// Stay below the existing 63-byte provider-label limit, including on Touch.
	return fmt.Sprintf("%x", digest[:24])
}

func clearArchitecture(server *Server) {
	server.Labels = shared.CloneLabels(server.Labels)
	delete(server.Labels, "architecture")
	for key := range server.Labels {
		if strings.HasPrefix(key, "architecture_") {
			delete(server.Labels, key)
		}
	}
	server.ServerType.Architecture = ""
}

func historicalArchitecture(server *Server, target SSHTarget) {
	labels := server.Labels
	_, source, scope := architectureProbe(target)
	observedAt, timeErr := strconv.ParseInt(labels["architecture_observed_at"], 10, 64)
	if labels["architecture_endpoint"] != architectureEndpoint(target) || labels["architecture_version"] != "1" || labels["architecture_source"] != source || labels["architecture_scope"] != scope || timeErr != nil || observedAt <= 0 {
		clearArchitecture(server)
		return
	}
	if supportedArchitecture(labels["architecture"]) {
		server.ServerType.Architecture = labels["architecture"]
	}
}

func (b *staticLeaseBackend) reportHistoricalArchitecture(server Server) {
	labels := server.Labels
	if labels["architecture_observed_at"] == "" {
		fmt.Fprintln(b.RT.Stderr, "static SSH architecture unknown (offline; no evidence for this endpoint)")
		return
	}
	fmt.Fprintf(b.RT.Stderr, "static SSH architecture historical=%s source=%s scope=%s observed_at=%s", labels["architecture"], labels["architecture_source"], labels["architecture_scope"], labels["architecture_observed_at"])
	if labels["architecture_host"] != "" {
		fmt.Fprintf(b.RT.Stderr, " host=%s process=%s translated=%s", labels["architecture_host"], labels["architecture_process"], labels["architecture_translated"])
	}
	fmt.Fprintln(b.RT.Stderr, " (offline; refreshed before execution)")
}

//go:build darwin && arm64

package applevmhelper

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const releaseVMDEntitlementsJSON = `{"com.apple.security.virtualization":true,"com.apple.security.network.client":true,"com.apple.security.network.server":true}`

type vmdCommandInvocation struct {
	Name string
	Args []string
}

type releaseVMDCommandStub struct {
	metadata               string
	actualEntitlementsJSON string
	notarizationErr        error
	plutilCalls            int
	invocations            []vmdCommandInvocation
}

func newReleaseVMDCommandStub() *releaseVMDCommandStub {
	return &releaseVMDCommandStub{
		metadata: strings.Join([]string{
			"Executable=/tmp/crabbox-apple-vm-vmd",
			"Identifier=" + ManagedVMDIdentifier,
			"Format=Mach-O thin (arm64)",
			"CodeDirectory v=20500 size=123 flags=0x10000(runtime) hashes=1+5 location=embedded",
			"Authority=" + vmdReleaseAuthority,
			"Authority=Developer ID Certification Authority",
			"Authority=Apple Root CA",
			"Timestamp=10 Jul 2026 at 17:00:00",
			"TeamIdentifier=" + vmdReleaseTeamID,
		}, "\n"),
		actualEntitlementsJSON: releaseVMDEntitlementsJSON,
	}
}

func (stub *releaseVMDCommandStub) run(stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	stub.invocations = append(stub.invocations, vmdCommandInvocation{Name: name, Args: append([]string(nil), args...)})
	if name == plutilBinary {
		stub.plutilCalls++
		if stub.plutilCalls%2 == 1 {
			return []byte(stub.actualEntitlementsJSON), nil, nil
		}
		return []byte(releaseVMDEntitlementsJSON), nil, nil
	}
	if name != codesignBinary {
		return nil, nil, errors.New("unexpected command")
	}
	if slices.Contains(args, "-dvvv") {
		return nil, []byte(stub.metadata), nil
	}
	if slices.Contains(args, "--entitlements") && slices.Contains(args, "--xml") {
		return []byte(HelperEntitlements), nil, nil
	}
	if slices.Contains(args, "--check-notarization") {
		return nil, nil, stub.notarizationErr
	}
	if slices.Contains(args, "--verify") {
		return nil, nil, nil
	}
	return nil, nil, errors.New("unexpected codesign invocation")
}

func TestEnsureVMDEmbeddedReleasePreservesBytesAndReverifiesCachedCopy(t *testing.T) {
	restoreVMDInstallGlobals(t)
	t.Setenv(VMDPathEnv, "")
	payload := []byte("signed-and-notarized-mach-o-bytes\x00unchanged")
	embeddedVMDPayloadFunc = func() []byte { return payload }
	embeddedVMDReleaseFunc = func() bool { return true }
	stub := newReleaseVMDCommandStub()
	runVMDCommand = stub.run

	stateRoot := t.TempDir()
	managedPath, err := ensureVMD(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installed, payload) {
		t.Fatalf("installed embedded daemon changed: got %x, want %x", installed, payload)
	}

	digestData, err := os.ReadFile(managedPath + managedDigestSuffix)
	if err != nil {
		t.Fatal(err)
	}
	var digests managedVMDDigests
	if err := json.Unmarshal(digestData, &digests); err != nil {
		t.Fatal(err)
	}
	if digests.SourceMode != string(vmdSourceEmbeddedRelease) ||
		digests.SourceSHA256 != digests.ManagedSHA256 ||
		digests.SourceSHA256 != sha256Hex(payload) {
		t.Fatalf("unexpected embedded daemon digests: %+v", digests)
	}

	cachedPath, err := ensureVMD(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if cachedPath != managedPath {
		t.Fatalf("cached path=%q, want %q", cachedPath, managedPath)
	}
	if got := countVMDCommands(stub.invocations, "--check-notarization"); got != 2 {
		t.Fatalf("online notarization checks=%d, want one before each use", got)
	}
	for _, invocation := range stub.invocations {
		if slices.Contains(invocation.Args, "--force") || slices.Contains(invocation.Args, "--sign") {
			t.Fatalf("embedded release daemon was re-signed: %v", invocation.Args)
		}
	}
	if !hasVMDRequirementInvocation(stub.invocations) {
		t.Fatal("release verification did not enforce the pinned identifier/team requirement")
	}
}

func TestEnsureVMDEmbeddedReleaseNotarizationFailureIsFailClosed(t *testing.T) {
	restoreVMDInstallGlobals(t)
	t.Setenv(VMDPathEnv, "")
	embeddedVMDPayloadFunc = func() []byte { return []byte("untrusted-embedded-payload") }
	embeddedVMDReleaseFunc = func() bool { return true }
	stub := newReleaseVMDCommandStub()
	stub.notarizationErr = errors.New("ticket unavailable")
	runVMDCommand = stub.run

	stateRoot := t.TempDir()
	_, err := ensureVMD(stateRoot)
	if err == nil || !strings.Contains(err.Error(), "notarization online") {
		t.Fatalf("ensureVMD error=%v, want online notarization failure", err)
	}
	entries, readErr := os.ReadDir(HelperDir(stateRoot))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ManagedVMDName+"-") {
			t.Fatalf("unverified embedded daemon escaped staging: %s", entry.Name())
		}
	}
	for _, invocation := range stub.invocations {
		if slices.Contains(invocation.Args, "--force") || slices.Contains(invocation.Args, "--sign") {
			t.Fatalf("verification failure fell back to ad-hoc signing: %v", invocation.Args)
		}
	}
}

func TestEnsureVMDOfficialReleaseMissingPayloadDoesNotUseDevelopmentFallback(t *testing.T) {
	restoreVMDInstallGlobals(t)
	t.Setenv(VMDPathEnv, "")
	embeddedVMDPayloadFunc = func() []byte { return nil }
	embeddedVMDReleaseFunc = func() bool { return true }
	sourceDir := t.TempDir()
	helperPath := filepath.Join(sourceDir, ManagedHelperName)
	if err := os.WriteFile(helperPath, []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, ManagedVMDName), []byte("development fallback"), 0o700); err != nil {
		t.Fatal(err)
	}
	helperExecutable = func() (string, error) { return helperPath, nil }
	commandCalls := 0
	runVMDCommand = func([]byte, string, ...string) ([]byte, []byte, error) {
		commandCalls++
		return nil, nil, nil
	}

	_, err := ensureVMD(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "official release helper has no embedded") {
		t.Fatalf("ensureVMD error=%v, want missing official payload", err)
	}
	if commandCalls != 0 {
		t.Fatalf("missing official payload ran %d signing/verification commands", commandCalls)
	}
}

func TestEnsureVMDSnapshotEmbeddedPayloadRetainsDevelopmentAdHocSigning(t *testing.T) {
	restoreVMDInstallGlobals(t)
	t.Setenv(VMDPathEnv, "")
	payload := []byte("credential-free-snapshot-vmd")
	embeddedVMDPayloadFunc = func() []byte { return payload }
	embeddedVMDReleaseFunc = func() bool { return false }
	signCalls := 0
	runVMDCommand = func(_ []byte, name string, args ...string) ([]byte, []byte, error) {
		if name != codesignBinary || !slices.Contains(args, "--force") || !slices.Contains(args, "--sign") {
			return nil, nil, errors.New("snapshot attempted official release verification")
		}
		signCalls++
		stagedPath := args[len(args)-1]
		file, err := os.OpenFile(stagedPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return nil, nil, err
		}
		_, writeErr := file.Write([]byte("-ad-hoc-signed"))
		closeErr := file.Close()
		return nil, nil, errors.Join(writeErr, closeErr)
	}

	managedPath, err := ensureVMD(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	managed, err := os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), payload...), []byte("-ad-hoc-signed")...)
	if !bytes.Equal(managed, want) {
		t.Fatalf("snapshot managed daemon=%q, want %q", managed, want)
	}
	if signCalls != 1 {
		t.Fatalf("snapshot ad-hoc sign calls=%d, want 1", signCalls)
	}
}

func TestVerifyEmbeddedReleaseVMDRejectsTrustPolicyMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*releaseVMDCommandStub)
		want   string
	}{
		{
			name: "identifier",
			mutate: func(stub *releaseVMDCommandStub) {
				stub.metadata = strings.Replace(stub.metadata, ManagedVMDIdentifier, "org.example.vmd", 1)
			},
			want: "signing identifier",
		},
		{
			name: "foundation authority",
			mutate: func(stub *releaseVMDCommandStub) {
				stub.metadata = strings.Replace(stub.metadata, vmdReleaseAuthority, "Developer ID Application: Example (EXAMPLE)", 1)
			},
			want: "OpenClaw Foundation",
		},
		{
			name: "team",
			mutate: func(stub *releaseVMDCommandStub) {
				stub.metadata = strings.Replace(stub.metadata, "TeamIdentifier="+vmdReleaseTeamID, "TeamIdentifier=EXAMPLE", 1)
			},
			want: "signing team",
		},
		{
			name: "hardened runtime",
			mutate: func(stub *releaseVMDCommandStub) {
				stub.metadata = strings.Replace(stub.metadata, "flags=0x10000(runtime)", "flags=0x0(none)", 1)
			},
			want: "hardened runtime",
		},
		{
			name: "timestamp",
			mutate: func(stub *releaseVMDCommandStub) {
				stub.metadata = strings.Replace(stub.metadata, "Timestamp=10 Jul 2026 at 17:00:00", "Timestamp=none", 1)
			},
			want: "secure timestamp",
		},
		{
			name: "exact entitlements",
			mutate: func(stub *releaseVMDCommandStub) {
				stub.actualEntitlementsJSON = `{"com.apple.security.virtualization":true,"unexpected":true}`
			},
			want: "entitlements do not exactly match",
		},
		{
			name: "online notarization",
			mutate: func(stub *releaseVMDCommandStub) {
				stub.notarizationErr = errors.New("notarization rejected")
			},
			want: "notarization online",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restoreVMDInstallGlobals(t)
			stub := newReleaseVMDCommandStub()
			test.mutate(stub)
			runVMDCommand = stub.run
			err := verifyEmbeddedReleaseVMD("/tmp/fake-vmd")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyEmbeddedReleaseVMD error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestEnsureVMDSourceSiblingRetainsDevelopmentAdHocSigning(t *testing.T) {
	restoreVMDInstallGlobals(t)
	t.Setenv(VMDPathEnv, "")
	embeddedVMDPayloadFunc = func() []byte { return nil }
	embeddedVMDReleaseFunc = func() bool { return false }
	sourceDir := t.TempDir()
	helperPath := filepath.Join(sourceDir, ManagedHelperName)
	sourcePath := filepath.Join(sourceDir, ManagedVMDName)
	if err := os.WriteFile(helperPath, []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePayload := []byte("source-development-vmd")
	if err := os.WriteFile(sourcePath, sourcePayload, 0o700); err != nil {
		t.Fatal(err)
	}
	helperExecutable = func() (string, error) { return helperPath, nil }
	signCalls := 0
	runVMDCommand = func(_ []byte, name string, args ...string) ([]byte, []byte, error) {
		if name != codesignBinary || !slices.Contains(args, "--force") || !slices.Contains(args, "--sign") {
			return nil, nil, errors.New("unexpected release verification for source payload")
		}
		signCalls++
		stagedPath := args[len(args)-1]
		file, err := os.OpenFile(stagedPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return nil, nil, err
		}
		_, writeErr := file.Write([]byte("-ad-hoc-signed"))
		closeErr := file.Close()
		return nil, nil, errors.Join(writeErr, closeErr)
	}

	managedPath, err := ensureVMD(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	managed, err := os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), sourcePayload...), []byte("-ad-hoc-signed")...)
	if !bytes.Equal(managed, want) {
		t.Fatalf("development managed daemon=%q, want %q", managed, want)
	}
	if signCalls != 1 {
		t.Fatalf("development ad-hoc sign calls=%d, want 1", signCalls)
	}
}

func TestEnsureVMDDevelopmentOverrideRemainsUnmanaged(t *testing.T) {
	restoreVMDInstallGlobals(t)
	override := filepath.Join(t.TempDir(), ManagedVMDName)
	if err := os.WriteFile(override, []byte("explicit override"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(VMDPathEnv, override)
	embeddedVMDPayloadFunc = func() []byte { return []byte("embedded development payload") }
	embeddedVMDReleaseFunc = func() bool { return false }
	runVMDCommand = func([]byte, string, ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("override must not invoke release verification")
	}

	got, err := ensureVMD(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != override {
		t.Fatalf("ensureVMD override=%q, want %q", got, override)
	}
}

func TestEnsureVMDOfficialReleaseRejectsExplicitOverride(t *testing.T) {
	restoreVMDInstallGlobals(t)
	override := filepath.Join(t.TempDir(), ManagedVMDName)
	if err := os.WriteFile(override, []byte("explicit override"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(VMDPathEnv, override)
	embeddedVMDPayloadFunc = func() []byte { return []byte("official payload") }
	embeddedVMDReleaseFunc = func() bool { return true }
	commandCalls := 0
	runVMDCommand = func([]byte, string, ...string) ([]byte, []byte, error) {
		commandCalls++
		return nil, nil, errors.New("official override must fail before commands")
	}

	_, err := ensureVMD(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), VMDPathEnv+" is not permitted") {
		t.Fatalf("ensureVMD error=%v, want official override rejection", err)
	}
	if commandCalls != 0 {
		t.Fatalf("official override invoked %d commands", commandCalls)
	}
}

func TestRunVMDExportWritesExactEmbeddedBytes(t *testing.T) {
	restoreVMDInstallGlobals(t)
	payload := []byte("exact-embedded-vmd\x00signature-bytes")
	embeddedVMDPayloadFunc = func() []byte { return payload }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if code := RunCLI([]string{"vmd-export"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("RunCLI vmd-export code=%d stderr=%q", code, stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), payload) {
		t.Fatalf("vmd-export changed payload: got %x, want %x", stdout.Bytes(), payload)
	}
	if stderr.Len() != 0 {
		t.Fatalf("vmd-export stderr=%q, want empty", stderr.String())
	}
}

func TestRunVMDExportFailsWithoutEmbeddedPayload(t *testing.T) {
	restoreVMDInstallGlobals(t)
	embeddedVMDPayloadFunc = func() []byte { return nil }
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if code := RunCLI([]string{"vmd-export"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("RunCLI vmd-export code=%d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("vmd-export wrote %d bytes without a payload", stdout.Len())
	}
	if !strings.Contains(stderr.String(), "payload is not embedded") {
		t.Fatalf("vmd-export stderr=%q, want missing-payload error", stderr.String())
	}
}

func TestRunVMDInfoReportsDeterministicReleaseTrustMode(t *testing.T) {
	payload := []byte("embedded-vmd-info")
	tests := []struct {
		name    string
		release bool
		want    string
	}{
		{
			name:    "credential-free snapshot",
			release: false,
			want:    `{"embedded":true,"sha256":"` + sha256Hex(payload) + `","releaseTrust":false,"trustPolicyVersion":0}` + "\n",
		},
		{
			name:    "official release",
			release: true,
			want:    `{"embedded":true,"sha256":"` + sha256Hex(payload) + `","releaseTrust":true,"trustPolicyVersion":1}` + "\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restoreVMDInstallGlobals(t)
			embeddedVMDPayloadFunc = func() []byte { return payload }
			embeddedVMDReleaseFunc = func() bool { return test.release }
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			if code := RunCLI([]string{"vmd-info"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("RunCLI vmd-info code=%d stderr=%q", code, stderr.String())
			}
			if got := stdout.String(); got != test.want {
				t.Fatalf("vmd-info=%q, want %q", got, test.want)
			}
		})
	}
}

func restoreVMDInstallGlobals(t *testing.T) {
	t.Helper()
	originalEmbeddedPayload := embeddedVMDPayloadFunc
	originalEmbeddedRelease := embeddedVMDReleaseFunc
	originalRunCommand := runVMDCommand
	originalHelperExecutable := helperExecutable
	originalCodesignBinary := codesignBinary
	originalPlutilBinary := plutilBinary
	t.Cleanup(func() {
		embeddedVMDPayloadFunc = originalEmbeddedPayload
		embeddedVMDReleaseFunc = originalEmbeddedRelease
		runVMDCommand = originalRunCommand
		helperExecutable = originalHelperExecutable
		codesignBinary = originalCodesignBinary
		plutilBinary = originalPlutilBinary
	})
}

func countVMDCommands(invocations []vmdCommandInvocation, arg string) int {
	count := 0
	for _, invocation := range invocations {
		if slices.Contains(invocation.Args, arg) {
			count++
		}
	}
	return count
}

func hasVMDRequirementInvocation(invocations []vmdCommandInvocation) bool {
	for _, invocation := range invocations {
		for _, arg := range invocation.Args {
			if strings.HasPrefix(arg, "-R=") &&
				strings.Contains(arg, ManagedVMDIdentifier) &&
				strings.Contains(arg, vmdReleaseTeamID) {
				return true
			}
		}
	}
	return false
}

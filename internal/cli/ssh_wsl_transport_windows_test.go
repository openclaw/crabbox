//go:build windows

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const fakeWSLStageHelper = "CRABBOX_FAKE_WSL_STAGE_HELPER"
const fakeWSLStageLauncherMode = "CRABBOX_FAKE_WSL_STAGE_LAUNCHER_MODE"

func init() {
	if os.Getenv(fakeWSLStageHelper) != "1" || !strings.EqualFold(filepath.Base(os.Args[0]), "wsl.exe") {
		return
	}
	if mode := os.Getenv(fakeWSLStageLauncherMode); mode != "" {
		runFakeWSLStageLauncher(mode)
		return
	}
	input := readFakeWSLStageInput()
	_ = os.WriteFile(os.Getenv("CRABBOX_FAKE_WSL_STAGE_INPUT"), input, 0o600)
	_, _ = os.Stdout.WriteString("stage-stdout\x00\xff\r\n")
	_, _ = os.Stderr.WriteString("stage-stderr\x00\xfe\n")
	os.Exit(23)
}

func readFakeWSLStageInput() []byte {
	if len(os.Args) != 15 || os.Args[1] != "--exec" || os.Args[2] != "sh" || os.Args[3] != "-c" || os.Args[4] != wslHelperBootstrap || len(strings.Join(os.Args[1:], " ")) >= wslStageLauncherCommandLimit {
		os.Exit(92)
	}
	count, _ := strconv.ParseInt(os.Args[6], 10, 64)
	preamble, err := strconv.Atoi(os.Args[7])
	if err != nil || preamble != 0 && preamble != 3 {
		os.Exit(92)
	}
	count += int64(preamble)
	if os.Args[8] == "run" {
		command, _ := strconv.ParseInt(os.Args[11], 10, 64)
		payload, _ := strconv.ParseInt(os.Args[12], 10, 64)
		count += command + payload
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, count))
	if err != nil || int64(len(input)) != count || preamble == 3 && !bytes.HasPrefix(input, []byte{239, 187, 191}) {
		os.Exit(93)
	}
	if log := os.Getenv("CRABBOX_FAKE_WSL_STAGE_INPUT"); log != "" {
		_ = os.WriteFile(log+"."+os.Args[8], input, 0600)
	}
	return input
}

func runFakeWSLStageLauncher(mode string) {
	role := ""
	if len(os.Args) > 8 {
		role = os.Args[8]
	}
	logPath := os.Getenv("CRABBOX_FAKE_WSL_STAGE_LAUNCHER_LOG")
	log := func(line string) {
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintln(file, line)
			_ = file.Close()
		}
	}
	switch role {
	case "run":
		if mode == "delayed-read" || mode == "delayed-control-completion" {
			delay := 3 * time.Second
			if mode == "delayed-control-completion" {
				delay = 11 * time.Second
			}
			time.Sleep(delay)
		}
		if mode != "main-no-read" {
			_ = readFakeWSLStageInput()
		}
		if mode == "delayed-read" {
			os.Exit(23)
		}
		if mode == "delayed-control-completion" {
			time.Sleep(wsl2SignalGrace)
			os.Exit(0)
		}
		_ = os.WriteFile(logPath+".main.pid", []byte(strconv.Itoa(os.Getpid())), 0o600)
		log("main-started")
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		time.Sleep(20 * time.Second)
	case "cleanup":
		_ = readFakeWSLStageInput()
		pid, _ := strconv.Atoi(readFakeWSLStageFile(logPath + ".main.pid"))
		log(fmt.Sprintf("cleanup-started:main-exited:%t", fakeWSLStageProcessExited(pid)))
		if mode == "cleanup-ignore" || mode == "cleanup-delay" {
			_ = os.WriteFile(logPath+".cleanup.pid", []byte(strconv.Itoa(os.Getpid())), 0o600)
			_ = os.Stdout.Close()
			_ = os.Stderr.Close()
			time.Sleep(20 * time.Second)
		}
	default:
		log("unexpected-role:" + role)
		os.Exit(91)
	}
	os.Exit(0)
}

func readFakeWSLStageFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func fakeWSLStageProcessExited(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err == windows.ERROR_INVALID_PARAMETER {
		return true
	}
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && result == windows.WAIT_OBJECT_0
}

func installFakeWSLStageExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "wsl.exe"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeWSLStageHelper, "1")
	return bin
}

func writeWSLStageReady(t *testing.T, home, nonce string, data []byte) string {
	t.Helper()
	root := filepath.Join(home, ".crabbox", "wsl-stage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, nonce+".ready")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func createWindowsStageTestDirectory(t *testing.T, path string) {
	t.Helper()
	security, _, err := privateWindowsSecurityAttributes(true)
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Elevated accounts can inherit Administrators ownership from TempDir.
	// Establish the fixture's owner and protected DACL atomically at creation.
	if err := windows.CreateDirectory(name, security); err != nil {
		t.Fatal(err)
	}
}

func newWindowsStageTestHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	createWindowsStageTestDirectory(t, home)
	return home
}

func setWindowsStageTestOwner(t *testing.T, path, owner string) {
	t.Helper()
	// Arbitrary fixture owners require restore privilege. Confine it to this
	// thread's temporary impersonation token; production never takes ownership.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.RevertToSelf(); err != nil {
			t.Fatal(err)
		}
	}()
	var token windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, true, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	privileges := windows.Tokenprivileges{PrivilegeCount: 1}
	name, err := windows.UTF16PtrFromString("SeRestorePrivilege")
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.LookupPrivilegeValue(nil, name, &privileges.Privileges[0].Luid); err != nil {
		t.Fatal(err)
	}
	privileges.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	if err := windows.AdjustTokenPrivileges(token, false, &privileges, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	sid, err := windows.StringToSid(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWSLStageRootPreparationSecuresInheritedWindowsACL(t *testing.T) {
	for _, test := range []struct {
		owner    string
		existing bool
	}{
		{},
		{existing: true},
		{owner: "S-1-5-32-544"},
		{owner: "S-1-5-32-544", existing: true},
		{owner: "S-1-5-18"},
		{owner: "S-1-5-18", existing: true},
	} {
		name := "new root"
		if test.existing {
			name = "reuse safe inherited root"
		}
		if test.owner != "" {
			name += " with HOME owner " + test.owner
		}
		t.Run(name, func(t *testing.T) {
			home := newWindowsStageTestHome(t)
			if test.owner != "" {
				setWindowsStageTestOwner(t, home, test.owner)
			}
			homeBefore := windowsStageSecuritySnapshot(t, home)
			parent := filepath.Join(home, ".crabbox")
			testSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			root := filepath.Join(home, ".crabbox", "wsl-stage")
			if test.existing {
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				user, err := currentWindowsUserSID()
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{parent, root} {
					if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user, nil, nil, nil); err != nil {
						t.Fatal(err)
					}
					descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
					if err != nil {
						t.Fatal(err)
					}
					control, _, err := descriptor.Control()
					if err != nil || control&windows.SE_DACL_PROTECTED != 0 {
						t.Fatalf("fixture must start with an unprotected DACL for %s: %v", path, err)
					}
					dacl, _, err := descriptor.DACL()
					if err != nil || dacl == nil || dacl.AceCount == 0 {
						t.Fatalf("fixture must start with inherited grants for %s: %v", path, err)
					}
					for index := uint16(0); index < dacl.AceCount; index++ {
						var ace *windows.ACCESS_ALLOWED_ACE
						if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
							t.Fatal(err)
						}
						if ace == nil || ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
							t.Fatalf("fixture grant must be inherited for %s", path)
						}
					}
				}
			}

			proofNonce := strings.Repeat("b", 32)
			// Observe actual persistence without replacing its behavior or relaxing raw snapshots.
			writes := filepath.Join(t.TempDir(), "acl-writes.txt")
			const persist = `[IO.Directory]::SetAccessControl($path, $acl)`
			script := decodePowerShellCommand(t, wslStageRootPreparationCommand(proofNonce))
			if strings.Count(script, persist) != 1 {
				t.Fatal("expected one ACL persistence site")
			}
			script = strings.Replace(script, persist,
				`[IO.File]::AppendAllText(`+psQuote(writes)+`, $path + [Environment]::NewLine); `+persist, 1)
			wantWrites := ""
			if test.existing {
				wantWrites = parent + "\r\n" + root + "\r\n"
			}
			checkWrites := func() {
				t.Helper()
				data, err := os.ReadFile(writes)
				if (err != nil && !os.IsNotExist(err)) || string(data) != wantWrites {
					t.Fatalf("ACL persistence paths=%q want=%q err=%v", data, wantWrites, err)
				}
			}
			output, err := runWSLStageRootScript(t, script)
			if err != nil || strings.TrimSpace(string(output)) != wslStagePreparationMarker+" "+proofNonce+" cmd" {
				t.Fatalf("ACL preparation marker=%q err=%v", output, err)
			}
			checkWrites()
			if got := windowsStageSecuritySnapshot(t, home); got != homeBefore {
				t.Fatal("preparation changed HOME ownership or ACL")
			}
			user, err := windows.GetCurrentProcessToken().GetTokenUser()
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{parent, root} {
				descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
					windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
				if err != nil {
					t.Fatal(err)
				}
				owner, _, err := descriptor.Owner()
				if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
					t.Fatalf("stage directory owner does not match the process identity: %v", err)
				}
				control, _, err := descriptor.Control()
				if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
					t.Fatalf("stage directory DACL is not protected: control=%v err=%v", control, err)
				}
			}
			for _, value := range []string{user.User.Sid.String(), "S-1-5-18", "S-1-5-32-544"} {
				sid, err := windows.StringToSid(value)
				if err != nil || !windowsPathGrantsSID(t, root, sid) {
					t.Fatalf("stage root does not retain required principal %q: %v", value, err)
				}
			}
			if windowsPathGrantsSID(t, root, testSID) {
				t.Fatal("stage root retained an unauthorized inherited principal")
			}
			if windowsPathGrantsSID(t, parent, testSID) {
				t.Fatal("stage parent retained an unauthorized principal capable of replacing the root")
			}
			proof := filepath.Join(root, "."+proofNonce+".proof")
			if data, readErr := os.ReadFile(proof); readErr != nil || string(data) != proofNonce || windowsPathGrantsSID(t, proof, testSID) {
				t.Fatalf("private authenticated route proof was missing, invalid, or accessible: err=%v", readErr)
			}
			stage := filepath.Join(root, strings.Repeat("a", 32)+".part")
			if err := os.WriteFile(stage, []byte("private stage payload"), 0o666); err != nil {
				t.Fatal(err)
			}
			if windowsPathGrantsSID(t, stage, testSID) || !windowsPathGrantsSID(t, stage, user.User.Sid) {
				t.Fatal("stage file did not inherit the verified private root ACL")
			}
			// Check normalization above on the first invocation, then safe reuse with existing files.
			paths := []string{home, parent, root, proof, stage}
			before := make([]string, len(paths))
			for i, path := range paths {
				before[i] = windowsStageSecuritySnapshot(t, path)
			}
			nextNonce := strings.Repeat("c", 32)
			output, err = runWSLStageRootScript(t, strings.ReplaceAll(script, proofNonce, nextNonce))
			if err != nil || strings.TrimSpace(string(output)) != wslStagePreparationMarker+" "+nextNonce+" cmd" {
				t.Fatalf("safe reuse marker=%q err=%v", output, err)
			}
			checkWrites()
			for i, path := range paths {
				if got := windowsStageSecuritySnapshot(t, path); got != before[i] {
					t.Fatalf("safe reuse changed ownership or ACL for %s: before=%q after=%q", path, before[i], got)
				}
			}
			for path, want := range map[string]string{proof: proofNonce, stage: "private stage payload"} {
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("safe reuse changed file %s: %q %v", path, data, err)
				}
			}
		})
	}
	for _, unsafe := range []string{"home", "parent", "root"} {
		t.Run("refuse unsafe existing "+unsafe+" with open handle", func(t *testing.T) {
			home := newWindowsStageTestHome(t)
			parent := filepath.Join(home, ".crabbox")
			root := filepath.Join(parent, "wsl-stage")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			user, err := currentWindowsUserSID()
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{parent, root} {
				if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			path := map[string]string{"home": home, "parent": parent, "root": root}[unsafe]
			testSID := makeWindowsTestParentPermissive(t, path)
			assertWindowsPathGrantsSID(t, path, testSID)
			// Retain the create/delete-child authority that an untrusted grant permits.
			name, err := windows.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := windows.CreateFile(name, windows.GENERIC_ALL,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if handle != windows.InvalidHandle {
					_ = windows.CloseHandle(handle)
				}
			}()
			witness := filepath.Join(root, "existing.part")
			if err := os.WriteFile(witness, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			paths := []string{home, parent, root, witness}
			before := make([]string, len(paths))
			for i, path := range paths {
				before[i] = windowsStageSecuritySnapshot(t, path)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			output, err := runWSLStageRootScript(t, decodePowerShellCommand(t, wslStageRootPreparationCommand(strings.Repeat("b", 32))))
			if err == nil || strings.TrimSpace(string(output)) != "WSL2 private stage preparation failed" {
				t.Fatalf("unsafe ACL was accepted or leaked details: output=%q err=%v", output, err)
			}
			// Keep stale authority through preparation, but release it before directory enumeration.
			if err := windows.CloseHandle(handle); err != nil {
				t.Fatal(err)
			}
			handle = windows.InvalidHandle
			for i, path := range paths {
				if got := windowsStageSecuritySnapshot(t, path); got != before[i] {
					t.Fatalf("refusal mutated ACL/owner for %s", path)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "existing.part" {
				t.Fatalf("refusal mutated staging files or proof: %v %v", entries, err)
			}
			if data, err := os.ReadFile(witness); err != nil || string(data) != "unchanged" {
				t.Fatalf("refusal changed existing file: %q %v", data, err)
			}
		})
	}
	t.Run("refuse unrelated HOME owner before mutation", func(t *testing.T) {
		home := newWindowsStageTestHome(t)
		setWindowsStageTestOwner(t, home, "S-1-5-21-100-200-300-400")
		before := windowsStageSecuritySnapshot(t, home)
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		output, err := runWSLStageRootScript(t, decodePowerShellCommand(t, wslStageRootPreparationCommand(strings.Repeat("b", 32))))
		if exitCode(err) != 74 || strings.TrimSpace(string(output)) != "WSL2 private stage preparation failed" {
			t.Fatalf("untrusted HOME owner accepted or leaked details: output=%q err=%v", output, err)
		}
		if got := windowsStageSecuritySnapshot(t, home); got != before {
			t.Fatal("refusal changed HOME ownership or ACL")
		}
		// PowerShell may create AppData in the fake profile; the entire staging namespace must stay absent.
		if _, err := os.Lstat(filepath.Join(home, ".crabbox")); !os.IsNotExist(err) {
			t.Fatalf("refusal created staging directories, proof, or payload: %v", err)
		}
	})
}

func windowsStageSecuritySnapshot(t *testing.T, path string) string {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor.String()
}

func TestWSLStageRootPreparationRejectsFilesAndReparsePoints(t *testing.T) {
	for _, test := range []struct {
		name     string
		parent   bool
		junction bool
	}{
		{name: "parent file", parent: true},
		{name: "stage root file"},
		{name: "parent junction", parent: true, junction: true},
		{name: "stage root junction", junction: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newWindowsStageTestHome(t)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			path := filepath.Join(home, ".crabbox")
			if !test.parent {
				createWindowsStageTestDirectory(t, path)
				path = filepath.Join(path, "wsl-stage")
			}
			if test.junction {
				outside := t.TempDir()
				if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", path, outside).CombinedOutput(); err != nil {
					t.Fatalf("create Windows junction: %v\n%s", err, output)
				}
			} else if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := runWSLStageRootScript(t, decodePowerShellCommand(t, wslStageRootPreparationCommand()))
			if err == nil || !bytes.Contains(output, []byte("WSL2 private stage preparation failed")) || bytes.Contains(output, []byte(path)) {
				t.Fatalf("unsafe root error leaked or succeeded: output=%q err=%v", output, err)
			}
		})
	}
}

func TestWSLStageLauncherVerifiesAndConsumesReadyFile(t *testing.T) {
	bin := installFakeWSLStageExecutable(t)
	nonce := strings.Repeat("a", 32)
	remote, payload := "printf exact", []byte{0, 1, 2, 255}
	spool, err := newWSLStageSpool(remote, payload, nil, int64(len(payload)), sshCommandLimit{execution: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	reader, err := spool.input.reset()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := append(append([]byte(wslLinuxHelper), remote...), payload...)
	for _, shell := range []wslStageShell{wslStageCMD, wslStagePowerShell} {
		t.Run(string(shell)+".exe default shell", func(t *testing.T) {
			home := newWindowsStageTestHome(t)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			inputLog := filepath.Join(t.TempDir(), "wsl-input")
			t.Setenv("CRABBOX_FAKE_WSL_STAGE_INPUT", inputLog)
			preparation := wslStageRootPreparationCommand(nonce)
			prepared, diagnostics, err := runWSLStageDefaultShell(t, shell, preparation, bin)
			if err != nil || len(diagnostics) != 0 || strings.TrimSpace(string(prepared)) != wslStagePreparationMarker+" "+nonce+" "+string(shell) {
				t.Fatalf("shell preparation stdout=%q stderr=%q err=%v", prepared, diagnostics, err)
			}
			proof, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(wslStageRoot), "."+nonce+".proof"))
			if err != nil || string(proof) != nonce {
				t.Fatalf("route proof=%q err=%v", proof, err)
			}
			ready := writeWSLStageReady(t, home, nonce, data)
			other := writeWSLStageReady(t, home, strings.Repeat("b", 32), data)
			launcher := wslStageLauncherCommand(nonce, spool.size, spool.digest(), shell)
			run := func() ([]byte, []byte, error) {
				return runWSLStageDefaultShell(t, shell, launcher, bin)
			}
			stdout, stderr, err := run()
			if exitCode(err) != 23 || string(stdout) != "stage-stdout\x00\xff\r\n" || string(stderr) != "stage-stderr\x00\xfe\n" {
				t.Fatalf("stdout=%q stderr=%q err=%v exit=%d", stdout, stderr, err, exitCode(err))
			}
			if _, err := os.Stat(ready); !os.IsNotExist(err) {
				t.Fatalf("ready stage survived DeleteOnClose: %v", err)
			}
			if preserved, err := os.ReadFile(other); err != nil || !bytes.Equal(preserved, data) {
				t.Fatalf("launcher changed another nonce's stage: %v", err)
			}
			got, err := os.ReadFile(inputLog)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, wantSuffix) && !bytes.Equal(got, append([]byte{239, 187, 191}, wantSuffix...)) {
				t.Fatalf("WSL stdin bytes=%d want exact suffix once (%d bytes)", len(got), len(wantSuffix))
			}
			// The same complete command must neither execute nor delete a replacement.
			_, replacement := newTestWSLStageSpool(t, []byte("replacement"))
			writeWSLStageReady(t, home, nonce, replacement)
			if err := os.Remove(inputLog); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err = run()
			if err == nil || len(stdout) != 0 || !bytes.Contains(stderr, []byte("WSL2 stage")) {
				t.Fatalf("same-nonce replacement stdout=%q stderr=%q err=%v", stdout, stderr, err)
			}
			if preserved, err := os.ReadFile(ready); err != nil || !bytes.Equal(preserved, replacement) {
				t.Fatalf("unowned replacement was deleted or modified: %v", err)
			}
			if _, err := os.Stat(inputLog); !os.IsNotExist(err) {
				t.Fatalf("replacement reached workload execution: %v", err)
			}
		})
	}
}

func TestWSLStageLauncherRejectsCorruptOrNonFileStage(t *testing.T) {
	home, nonce := t.TempDir(), strings.Repeat("b", 32)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	command := decodePowerShellCommand(t, wslStageLauncherCommand(nonce, 4, [sha256.Size]byte{}, wslStageCMD))
	for name, setup := range map[string]func(){
		"corrupt": func() { writeWSLStageReady(t, home, nonce, []byte("junk")) },
		"directory": func() {
			path := filepath.Join(home, ".crabbox", "wsl-stage", nonce+".ready")
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.RemoveAll(filepath.Join(home, ".crabbox"))
			setup()
			output, err := runWindowsPowerShellScript(t, command)
			if err == nil || !bytes.Contains(output, []byte("WSL2 stage")) {
				t.Fatalf("output=%q err=%v", output, err)
			}
		})
	}
}

func TestWSLStageLauncherRejectsSameNonceValidReplacement(t *testing.T) {
	home, nonce := t.TempDir(), strings.Repeat("c", 32)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	original, raw := newTestWSLStageSpool(t, []byte("original"))
	_, replacement := newTestWSLStageSpool(t, []byte("replaced"))
	for _, test := range []struct {
		name   string
		data   []byte
		mutate int
	}{
		{"valid envelope", replacement, -1},
		{"descriptor byte", raw, 40},
		{"blinder byte", raw, wslStageHeaderSize - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := bytes.Clone(test.data)
			if test.mutate >= 0 {
				data[test.mutate] ^= 1
			}
			ready := writeWSLStageReady(t, home, nonce, data)
			output, err := runWindowsPowerShellScript(t, decodePowerShellCommand(t, wslStageLauncherCommand(nonce, original.size, original.digest(), wslStageCMD)))
			if err == nil || !bytes.Contains(output, []byte("WSL2 stage digest mismatch")) {
				t.Fatalf("same-nonce replacement output=%q err=%v", output, err)
			}
			if preserved, readErr := os.ReadFile(ready); readErr != nil || !bytes.Equal(preserved, data) {
				t.Fatalf("unowned same-nonce replacement was deleted or modified: err=%v", readErr)
			}
		})
	}
}

func TestWSLStageLauncherIsFixedAndCarriesDigestBinding(t *testing.T) {
	home, nonce := t.TempDir(), strings.Repeat("c", 32)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	spool, raw := newTestWSLStageSpool(t, []byte("original"))
	sensitivePrefix := wslStageHeaderSize + int(binary.LittleEndian.Uint32(raw[8:])) + int(binary.LittleEndian.Uint32(raw[12:])) + 1
	for _, test := range []struct {
		name, suffix           string
		mutate, missing, probe bool
		size                   int
		blinder                bool
	}{
		{name: "native disposition declaration compiles and invokes", suffix: ".ready", size: len(raw)},
		{name: "incomplete partial uses exact prefix", suffix: ".part", size: 13},
		{name: "partial inside blinder", suffix: ".part", size: wslStageHeaderSize - 1},
		{name: "complete blinded descriptor", suffix: ".part", size: wslStageHeaderSize},
		{name: "sensitive prefix includes blinder", suffix: ".part", size: sensitivePrefix},
		{name: "changed blinder preserves sensitive partial", suffix: ".part", size: sensitivePrefix, mutate: true, blinder: true},
		{name: "changed partial survives", suffix: ".part", size: 13, mutate: true},
		{name: "changed ready survives", suffix: ".ready", size: len(raw), mutate: true},
		{name: "already absent", suffix: ".ready", size: len(raw), missing: true},
		{name: "route proof", suffix: ".proof", size: 32},
		{name: "same handle blocks replacement", suffix: ".ready", size: len(raw), probe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := nonce + test.suffix
			expected := bytes.Clone(raw[:test.size])
			if test.suffix == ".proof" {
				name = "." + nonce + ".proof"
				expected = []byte(nonce)
			}
			digest := sha256.Sum256(expected)
			data := bytes.Clone(expected)
			if test.mutate {
				index := len(data) - 1
				if test.blinder {
					index = wslStageHeaderSize - 1
				}
				data[index] ^= 1
			}
			path := filepath.Join(home, filepath.FromSlash(wslStageRoot), name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if !test.missing {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			command := wslStageFileCommand(nonce, name, int64(len(expected)), digest, true, wslStageCMD)
			if len(command) >= wslStageLauncherCommandLimit {
				t.Fatalf("launcher bytes=%d", len(command))
			}
			script := decodePowerShellCommand(t, command)
			if test.probe {
				script = strings.Replace(script, "$delete = [byte]1", `try { [IO.File]::Move($p,$p+'.replacement'); throw 'replacement unexpectedly succeeded' } catch [IO.IOException] { }
    $delete = [byte]1`, 1)
			}
			output, err := runWindowsPowerShellScript(t, script)
			if (err != nil) != test.mutate {
				t.Fatalf("discard %v: %s", err, output)
			}
			if test.mutate {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatal("replacement changed")
				}
				_ = os.Remove(path)
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("owned object survived: %v", err)
			}
		})
	}
	first := wslStageLauncherCommand(nonce, spool.size, spool.digest(), wslStageCMD)
	second := wslStageLauncherCommand(nonce, spool.size, sha256.Sum256([]byte("replacement")), wslStageCMD)
	if first == second {
		t.Fatal("launcher lost digest binding")
	}
}

func TestWSLStageLauncherConfirmsOriginalAndCleanupTermination(t *testing.T) {
	for _, test := range []struct {
		name         string
		mode         string
		mutation     string
		want         string
		wantCleanup  bool
		wantSurvivor string
	}{
		{name: "blocked Windows to WSL pipe", mode: "main-no-read", want: "WSL2 command timed out", wantCleanup: true},
		{
			name: "original ignores kill", mode: "main-ignore",
			mutation: `if($child -eq $process){throw "private-launcher-secret"}else{$child.Kill()}`,
			want:     "launcher termination unconfirmed", wantSurvivor: "main",
		},
		{
			name: "original delays kill", mode: "main-delay",
			mutation: `if($child -eq $process){Start-Sleep -Milliseconds 25};$child.Kill()`,
			want:     "WSL2 command timed out", wantCleanup: true,
		},
		{
			name: "cleanup ignores kill", mode: "cleanup-ignore",
			mutation: `if($child -eq $cleanup){throw "private-cleanup-secret"}else{$child.Kill()}`,
			want:     "cleanup launcher termination unconfirmed", wantCleanup: true, wantSurvivor: "cleanup",
		},
		{
			name: "cleanup delays kill", mode: "cleanup-delay",
			mutation: `if($child -eq $cleanup){Start-Sleep -Milliseconds 25};$child.Kill()`,
			want:     "WSL2 command cleanup failed", wantCleanup: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := installFakeWSLStageExecutable(t)
			home, nonce := t.TempDir(), strings.Repeat("e", 32)
			logPath := filepath.Join(t.TempDir(), "launcher.log")
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv(fakeWSLStageLauncherMode, test.mode)
			t.Setenv("CRABBOX_FAKE_WSL_STAGE_LAUNCHER_LOG", logPath)

			remote := "private-staged-owner-token"
			previous := wslWindowsOwner
			updated := previous
			if test.mutation != "" {
				updated = strings.Replace(previous, `$child.Kill()`, test.mutation, 1)
				if updated == previous {
					t.Fatal("exact process fixture injection missing")
				}
			}
			wslWindowsOwner = strings.NewReplacer("5000", "200", "10000", "600").Replace(updated)
			t.Cleanup(func() { wslWindowsOwner = previous })
			var payload []byte
			if test.mode == "main-no-read" {
				payload = make([]byte, 8<<20)
			}
			spool, err := newWSLStageSpool(remote, payload, nil, int64(len(payload)), sshCommandLimit{execution: 1500 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer spool.close()
			reader, err := spool.input.reset()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			ready := writeWSLStageReady(t, home, nonce, data)
			defer func() {
				for _, role := range []string{"main", "cleanup"} {
					if pid, err := strconv.Atoi(readFakeWSLStageFile(logPath + "." + role + ".pid")); err == nil && !fakeWSLStageProcessExited(pid) {
						process, findErr := os.FindProcess(pid)
						if findErr == nil {
							_ = process.Kill()
						}
					}
				}
			}()

			script := `[Environment]::CurrentDirectory=` + psQuote(bin) + `;` +
				decodePowerShellCommand(t, wslStageLauncherCommand(nonce, spool.size, spool.digest(), wslStageCMD))
			output, err := runWindowsPowerShellScript(t, script)
			logs := readFakeWSLStageFile(logPath)
			if err == nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("output=%q error=%v logs=%q want=%q", output, err, logs, test.want)
			}
			if test.mode == "main-no-read" && strings.Contains(string(output), "phase=execute") {
				t.Fatal("blocked pipe unexpectedly completed transfer")
			}
			if got := strings.Contains(logs, "cleanup-started:main-exited:true"); got != test.wantCleanup {
				t.Fatalf("fallback cleanup=%t want=%t logs=%q", got, test.wantCleanup, logs)
			}
			if strings.Contains(logs, "cleanup-started:main-exited:false") {
				t.Fatalf("fallback cleanup raced the exact original launcher: %q", logs)
			}
			if test.wantSurvivor != "" {
				pid, parseErr := strconv.Atoi(readFakeWSLStageFile(logPath + "." + test.wantSurvivor + ".pid"))
				if parseErr != nil || fakeWSLStageProcessExited(pid) {
					t.Fatalf("exact %s launcher did not exercise failed termination: pid=%d error=%v", test.wantSurvivor, pid, parseErr)
				}
			}
			for _, secret := range []string{remote, nonce, ready, "private-launcher-secret", "private-cleanup-secret"} {
				if strings.Contains(string(output), secret) {
					t.Fatalf("launcher diagnostic leaked %q: %q", secret, output)
				}
			}
		})
	}
}

// Run the literal production command through the default shell, with no exit fix.
func runWSLStageDefaultShell(t *testing.T, shell wslStageShell, command, directory string) ([]byte, []byte, error) {
	t.Helper()
	if command == "" || len(command) >= wslStageLauncherCommandLimit {
		t.Fatalf("command size=%d", len(command))
	}
	executable, args := "cmd.exe", []string{"/d", "/s", "/c", command}
	if shell == wslStagePowerShell {
		executable, args = "powershell.exe", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command}
	}
	cmd := exec.CommandContext(t.Context(), executable, args...)
	if shell == wslStageCMD {
		// cmd does not use CommandLineToArgvW. Go's default escaping would
		// insert literal backslashes around the production command's quotes.
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + command + `"`}
		if len(cmd.SysProcAttr.CmdLine) >= wslStageLauncherCommandLimit {
			t.Fatal("cmd invocation exceeds command budget")
		}
	}
	cmd.Dir = directory
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func runWSLStageRootScript(t *testing.T, script string) ([]byte, error) {
	t.Helper()
	stdout, stderr, err := runWSLStageDefaultShell(t, wslStageCMD, wslStagePowerShellCommand(script, wslStageCMD), "")
	return append(stdout, stderr...), err
}

func TestWSLStageFrameworkInputPreamble(t *testing.T) {
	testWSLStageFrameworkInput(t, "")
}

func TestWSLStageDelayedInitialHandoff(t *testing.T) {
	for _, mode := range []string{"delayed-read", "delayed-control-completion"} {
		t.Run(mode, func(t *testing.T) { testWSLStageFrameworkInput(t, mode) })
	}
}

func testWSLStageFrameworkInput(t *testing.T, mode string) {
	t.Helper()
	delayed := mode != ""
	bin := installFakeWSLStageExecutable(t)
	for _, bom := range []bool{false, true} {
		for _, cleanup := range []bool{false, true} {
			if delayed && cleanup {
				continue
			}
			t.Run(fmt.Sprintf("bom=%t/cleanup=%t", bom, cleanup), func(t *testing.T) {
				log := filepath.Join(t.TempDir(), "input")
				t.Setenv("CRABBOX_FAKE_WSL_STAGE_INPUT", log)
				t.Setenv("CRABBOX_FAKE_WSL_STAGE_LAUNCHER_LOG", log+".launcher")
				if cleanup {
					t.Setenv(fakeWSLStageLauncherMode, "main-delay")
				} else if delayed {
					t.Setenv(fakeWSLStageLauncherMode, mode)
				}
				limit := sshCommandLimit{}
				if delayed {
					limit = sshCommandLimit{execution: sshControlExecutionLimit, control: true}
				}
				spool, raw := newTestWSLStageSpoolWithLimit(t, []byte{0, 255, 13, 10}, limit)
				owner, helper, command, payload := decodeWSLStage(t, raw)
				if cleanup {
					binary.LittleEndian.PutUint64(raw[32:], 1500)
				} else if delayed {
					if binary.LittleEndian.Uint64(raw[32:]) != uint64(spool.timing.operation.Milliseconds()) {
						t.Fatal("native fixture lost generated operation guard")
					}
				}
				frame := append(append([]byte(helper), []byte(command)...), payload...)
				path := filepath.Join(t.TempDir(), "frame")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				flag := "$false"
				if bom {
					flag = "$true"
				}
				script := `if ($PSVersionTable.PSEdition -ne 'Desktop' -or $PSVersionTable.PSVersion.Major -ne 5 -or $PSVersionTable.PSVersion.Minor -ne 1) { throw 'requires Windows PowerShell 5.1 / Framework' }
[Console]::InputEncoding=[Text.UTF8Encoding]::new(` + flag + `)
[Environment]::CurrentDirectory=` + psQuote(bin) + `
$file=[IO.File]::OpenRead(` + psQuote(path) + `)
try {
  $descriptor = [IO.BinaryReader]::new($file).ReadBytes(` + fmt.Sprint(wslStageHeaderSize) + `)
  $file.Position += ` + fmt.Sprint(len(owner)) + `
  & ([ScriptBlock]::Create(` + psQuote(owner) + `)) $file $descriptor '` + strings.Repeat("a", 32) + `'
} finally { $file.Dispose() }`
				process := windowsPowerShellScriptCommand(t, script)
				// Encoding changes belong only to this disposable test console.
				process.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
				output, err := process.CombinedOutput()
				wantCode := 23
				if mode == "delayed-control-completion" {
					wantCode = 0
				}
				if !cleanup && exitCode(err) != wantCode || cleanup && (err == nil || !bytes.Contains(output, []byte("WSL2 command timed out"))) {
					t.Fatalf("Framework owner exit=%d output=%q err=%v", exitCode(err), output, err)
				}
				preamble := []byte(nil)
				if bom {
					preamble = []byte{239, 187, 191}
				}
				got, err := os.ReadFile(log + ".run")
				if err != nil || !bytes.Equal(got, append(bytes.Clone(preamble), frame...)) {
					t.Fatalf("Framework run bytes=%d want=%d err=%v", len(got), len(preamble)+len(frame), err)
				}
				if cleanup {
					got, err = os.ReadFile(log + ".cleanup")
					if err != nil || !bytes.Equal(got, append(bytes.Clone(preamble), []byte(helper)...)) {
						t.Fatalf("Framework cleanup bytes=%d err=%v", len(got), err)
					}
				}
			})
		}
	}
}

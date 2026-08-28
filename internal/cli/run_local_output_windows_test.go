//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestFailureBundleWindowsSecuresDirectoryBeforeTempAndPublishesHandle(t *testing.T) {
	// TempDir can be owned by Administrators on elevated Windows runners.
	dir := filepath.Join(t.TempDir(), "captures")
	if err := createPrivateRunOutputDir(dir); err != nil {
		t.Fatal(err)
	}
	testSID := makeWindowsTestParentPermissive(t, dir)
	assertWindowsPathGrantsSID(t, dir, testSID)
	output, err := prepareFailureBundleDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	assertWindowsPathPrivateFromSID(t, dir, true, testSID)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("directory preparation created names: entries=%v err=%v", entries, err)
	}

	path := filepath.Join(dir, "bundle.tar.gz")
	if err := os.WriteFile(path, []byte("previous bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := output.createTemp("bundle.tar.gz"); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(dir, output.name)
	assertWindowsPathPrivateFromSID(t, tempPath, false, testSID)
	if _, err := output.Write([]byte("original bundle")); err != nil {
		t.Fatal(err)
	}
	// Even the owner cannot remove the open temporary name and insert another
	// inode. This also protects against previously granted directory access.
	tempName, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.DeleteFile(tempName); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("temporary deletion should be denied by sharing mode: %v", err)
	}
	substitute := filepath.Join(dir, "substitute")
	if err := os.WriteFile(substitute, []byte("substitute bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(substitute, tempPath); err == nil {
		t.Fatal("substitute replaced the open temporary file")
	}
	// The owner never accepts a source path for publication. Even its tracked
	// cleanup name is not authority for the file to publish on Windows.
	output.name = "substitute"
	if err := output.publish("bundle.tar.gz"); err != nil {
		t.Fatal(err)
	}
	finalName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.DeleteFile(finalName); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("published bundle must remain protected until close: %v", err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original bundle" {
		t.Fatalf("published data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(substitute); err != nil || string(data) != "substitute bundle" {
		t.Fatalf("substitute was promoted: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary name remains after handle publication: %v", err)
	}
	assertWindowsPathPrivateFromSID(t, path, false, testSID)
}

func TestFailureBundleWindowsPreservesUnwritablePrivateDirectory(t *testing.T) {
	isolateTestUserDirs(t)
	t.Chdir(t.TempDir())
	dir := filepath.Join(".crabbox", "captures")
	if err := createPrivateRunOutputDir(dir); err != nil {
		t.Fatal(err)
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	// Keep metadata/security access but omit FILE_ADD_FILE. The caller must not
	// grant itself that missing right as a side effect of capturing a failure.
	access := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.DELETE)
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;0x%08x;;;%s)", user.String(), access, user.String()))
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := securePrivateWindowsPath(dir, true); err != nil {
			t.Error(err)
		}
	})
	before, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if probe, err := os.CreateTemp(dir, "probe-*"); err == nil {
		_ = probe.Close()
		t.Fatal("fixture did not deny file creation")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unexpected probe error: %v", err)
	}
	path, _, err := writeLocalFailureBundle("bundle.tar.gz", "", FailureCaptureMetadata{ExitCode: 23})
	if err != nil {
		t.Fatal(err)
	}
	state, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(state, "captures", "bundle.tar.gz") {
		t.Fatalf("path=%q state=%q", path, state)
	}
	if len(readTarGzContents(t, path)["crabbox-artifacts/crabbox-run.json"]) == 0 {
		t.Fatal("fallback bundle is missing metadata")
	}
	after, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatal("unwritable directory DACL was changed")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("unwritable directory gained a temporary name: entries=%v err=%v", entries, err)
	}
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatalf("failed preparation retained directory handles: %v", err)
	}
	if err := os.Rename(dir+"-moved", dir); err != nil {
		t.Fatal(err)
	}
}

func TestFailureBundleWindowsDirectoryOwnerPreventsReplacement(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%t", fail), func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, ".crabbox")
			dir := filepath.Join(parent, "captures")
			if err := createPrivateRunOutputDir(dir); err != nil {
				t.Fatal(err)
			}
			testSID := makeWindowsTestParentPermissive(t, parent)
			output, err := prepareFailureBundleDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			assertWindowsPathGrantsSID(t, parent, testSID)
			assertWindowsPathPrivateFromSID(t, dir, true, testSID)
			assertBound := func(stage string) {
				t.Helper()
				for _, target := range []string{parent, dir} {
					name, err := windows.UTF16PtrFromString(target)
					if err != nil {
						t.Fatal(err)
					}
					// An existing handle without delete sharing must deny the
					// DELETE access needed to move the directory out of the way.
					handle, err := windows.CreateFile(name, windows.DELETE,
						windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
						nil, windows.OPEN_EXISTING,
						windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
					if err == nil {
						_ = windows.CloseHandle(handle)
						t.Fatalf("%s: directory allowed delete access: %s", stage, target)
					}
					if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
						t.Fatalf("%s: expected retained directory sharing denial, got %v", stage, err)
					}
					if err := os.Rename(target, target+"-moved"); err == nil {
						t.Fatalf("%s: directory was replaced: %s", stage, target)
					}
					if _, err := os.Stat(target + "-moved"); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s: rename changed destination: %v", stage, err)
					}
				}
			}
			assertBound("prepared")
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("preparation created names: entries=%v err=%v", entries, err)
			}
			if err := output.createTemp("bundle.tar.gz"); err != nil {
				t.Fatal(err)
			}
			assertBound("created")
			assertWindowsPathPrivateFromSID(t, filepath.Join(dir, output.name), false, testSID)
			if _, err := output.Write([]byte("private bundle")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "bundle.tar.gz")
			if fail {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := output.publish("bundle.tar.gz"); (err != nil) != fail {
				t.Fatalf("publication error=%v, want failure=%t", err, fail)
			}
			assertBound("publication attempted")
			if fail {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := output.Close(); err != nil {
				t.Fatal(err)
			}
			for _, handle := range output.directory.handles {
				var info windows.ByHandleFileInformation
				if err := windows.GetFileInformationByHandle(handle, &info); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
					t.Fatalf("directory handle was not closed: %v", err)
				}
			}
			if fail {
				if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
					t.Fatalf("failed publication leaked temp: entries=%v err=%v", entries, err)
				}
			} else if data, err := os.ReadFile(path); err != nil || string(data) != "private bundle" {
				t.Fatalf("bundle=%q err=%v", data, err)
			}
			// Closing the owner, including the failure path, releases every ancestor.
			if err := os.Rename(dir, dir+"-moved"); err != nil {
				t.Fatalf("captures remains locked after close: %v", err)
			}
			if err := os.Rename(parent, parent+"-moved"); err != nil {
				t.Fatalf("parent remains locked after close: %v", err)
			}
		})
	}
}

func TestFailureBundleDestinationUnwritableWindows(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"permission", privateRunOutputWriteError{windows.ERROR_ACCESS_DENIED}, true},
		{"write protected", privateRunOutputWriteError{windows.ERROR_WRITE_PROTECT}, true},
		{"NT permission", privateRunOutputWriteError{windows.STATUS_ACCESS_DENIED}, true},
		{"NT write protected", privateRunOutputWriteError{windows.STATUS_MEDIA_WRITE_PROTECTED}, true},
		{"security failure", fmt.Errorf("verify DACL: %w", windows.ERROR_ACCESS_DENIED), false},
		{"reparse point", privateRunOutputWriteError{windows.STATUS_REPARSE_POINT_ENCOUNTERED}, false},
		{"disk full", privateRunOutputWriteError{windows.ERROR_DISK_FULL}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureBundleDestinationUnwritable(tc.err); got != tc.want {
				t.Fatalf("unwritable=%t want=%t error=%v", got, tc.want, tc.err)
			}
		})
	}
}

func TestPrivateRunOutputWindowsDoesNotInheritPermissiveDACL(t *testing.T) {
	for _, fixture := range []string{"new directory", "current owner without WRITE_OWNER", "different owner without WRITE_OWNER"} {
		t.Run(fixture, func(t *testing.T) {
			parent := t.TempDir()
			testSID := makeWindowsTestParentPermissive(t, parent)
			dir := filepath.Join(parent, "downloads")
			var before *windows.ByHandleFileInformation
			if fixture != "new directory" {
				if fixture == "current owner without WRITE_OWNER" {
					if err := createPrivateRunOutputDir(dir); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				user, err := currentWindowsUserSID()
				if err != nil {
					t.Fatal(err)
				}
				security, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
				if err != nil {
					t.Fatal(err)
				}
				owner, _, err := security.Owner()
				if err != nil || owner == nil {
					t.Fatalf("read fixture owner: %v", err)
				}
				// Elevated Windows runners create legacy directories owned by
				// Administrators. Do not change the owner to manufacture the case.
				if fixture == "different owner without WRITE_OWNER" && owner.Equals(user) {
					t.Skip("requires a default directory owner distinct from the current user")
				}
				access := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_GENERIC_EXECUTE | windows.DELETE | windows.WRITE_DAC)
				descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;0x%08x;;;%s)(A;OICI;GR;;;%s)", access, user.String(), testSID.String()))
				if err != nil {
					t.Fatal(err)
				}
				acl, _, err := descriptor.DACL()
				if err != nil {
					t.Fatal(err)
				}
				if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
					t.Fatal(err)
				}
				probe, err := openPrivateWindowsSecurityHandle(dir, true, windows.WRITE_OWNER)
				if err == nil {
					_ = windows.CloseHandle(probe)
					t.Fatal("fixture unexpectedly permits WRITE_OWNER")
				}
				if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					t.Fatalf("probe fixture ownership access: %v", err)
				}
				assertWindowsPathGrantsSID(t, dir, testSID)
				original, err := openPrivateWindowsSecurityHandle(dir, true, windows.READ_CONTROL)
				if err != nil {
					t.Fatal(err)
				}
				defer windows.CloseHandle(original)
				before = &windows.ByHandleFileInformation{}
				if err := windows.GetFileInformationByHandle(original, before); err != nil {
					t.Fatal(err)
				}
			}

			path := filepath.Join(dir, "proof.txt")
			if err := writeRunDownloadFile(path, []byte("private proof")); err != nil {
				logWindowsPrivateHandleDiagnostics(t, dir, true)
				t.Fatal(err)
			}
			assertWindowsPathPrivateFromSID(t, dir, true, testSID)
			assertWindowsPathPrivateFromSID(t, path, false, testSID)
			if before != nil {
				repaired, err := openPrivateWindowsSecurityHandle(dir, true, windows.READ_CONTROL)
				if err != nil {
					t.Fatal(err)
				}
				defer windows.CloseHandle(repaired)
				var after windows.ByHandleFileInformation
				if err := windows.GetFileInformationByHandle(repaired, &after); err != nil {
					t.Fatal(err)
				}
				if before.VolumeSerialNumber != after.VolumeSerialNumber || before.FileIndexHigh != after.FileIndexHigh || before.FileIndexLow != after.FileIndexLow {
					t.Fatal("ownership repair replaced the directory")
				}
			}

			if err := setWindowsPathPermissive(t, path, false, testSID); err != nil {
				t.Fatal(err)
			}
			assertWindowsPathGrantsSID(t, path, testSID)
			if err := writeRunDownloadFile(path, []byte("replacement proof")); err != nil {
				t.Fatal(err)
			}
			assertWindowsPathPrivateFromSID(t, path, false, testSID)
		})
	}
}

// Failed-boundary diagnostics only: open existing fixture objects without
// changing their security or contents, and never log SIDs or raw descriptors.
func logWindowsPrivateHandleDiagnostics(t *testing.T, path string, directory bool) {
	t.Helper()
	t.Logf("private-handle diagnostics: directory=%t", directory)
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	const comparisonShare = share | windows.FILE_SHARE_DELETE
	parentName, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		t.Logf("diagnostic parent encoding: %v", err)
		return
	}
	parent, err := windows.CreateFile(parentName, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.FILE_TRAVERSE,
		share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Logf("diagnostic parent open: %v", err)
		return
	}
	defer windows.CloseHandle(parent)
	if err := validatePrivateWindowsFileType(parent, true); err != nil {
		t.Errorf("diagnostic parent type: %v", err)
		return
	}
	options := uint32(windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_OPEN_FOR_BACKUP_INTENT)
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	baseAccess := uint32(windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.WRITE_DAC | windows.SYNCHRONIZE)
	if directory {
		options = windows.FILE_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_OPEN_FOR_BACKUP_INTENT
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
		baseAccess |= windows.FILE_TRAVERSE
	}
	openNT := func(root windows.Handle, name string, access, openOptions, shareMode uint32) (windows.Handle, error) {
		objectName, err := windows.NewNTUnicodeString(name)
		if err != nil {
			return windows.InvalidHandle, err
		}
		attributes := &windows.OBJECT_ATTRIBUTES{
			Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
			RootDirectory: root,
			ObjectName:    objectName,
			Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		}
		var handle windows.Handle
		err = windows.NtCreateFile(&handle, access, attributes, &windows.IO_STATUS_BLOCK{}, nil,
			windows.FILE_ATTRIBUTE_NORMAL, shareMode, windows.FILE_OPEN, openOptions, 0, 0)
		return handle, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Logf("diagnostic path encoding: %v", err)
		return
	}
	win32, err := windows.CreateFile(name, baseAccess, share, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		t.Logf("diagnostic CreateFile baseline access=%#x flags=%#x share=%#x: %v", baseAccess, flags, share, err)
		return
	}
	defer windows.CloseHandle(win32)
	if err := validatePrivateWindowsFileType(win32, directory); err != nil {
		t.Errorf("diagnostic fixture type: %v", err)
		return
	}
	var before windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(win32, &before); err != nil {
		t.Logf("diagnostic fixture identity: %v", err)
		return
	}
	checkIdentity := func(label string, handle windows.Handle, openErr error) {
		t.Helper()
		if openErr != nil {
			t.Logf("%s: open_error=%v", label, openErr)
			return
		}
		var after windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &after); err != nil {
			t.Errorf("%s: identity_error=%v", label, err)
			return
		}
		equal := before.VolumeSerialNumber == after.VolumeSerialNumber && before.FileIndexHigh == after.FileIndexHigh && before.FileIndexLow == after.FileIndexLow
		t.Logf("%s: open_ok=true identity_equal=%t directory=%t", label, equal, after.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0)
		if !equal {
			t.Errorf("%s: comparison opened a different fixture object", label)
		}
	}
	user, userErr := currentWindowsUserSID()
	descriptor, securityErr := windows.GetSecurityInfo(win32, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if userErr != nil || securityErr != nil {
		t.Logf("diagnostic security: user_error=%v descriptor_error=%v", userErr, securityErr)
	} else {
		owner, _, ownerErr := descriptor.Owner()
		control, _, controlErr := descriptor.Control()
		acl, _, aclErr := descriptor.DACL()
		t.Logf("diagnostic security: owner_present=%t owner_current=%t owner_error=%v protected_dacl=%t control_error=%v dacl_present=%t dacl_error=%v", owner != nil, owner != nil && owner.Equals(user), ownerErr, control&windows.SE_DACL_PROTECTED != 0, controlErr, acl != nil, aclErr)
		if acl != nil && aclErr == nil {
			t.Logf("diagnostic dacl: ace_count=%d (showing at most 8)", acl.AceCount)
			for index := uint32(0); index < uint32(acl.AceCount) && index < 8; index++ {
				var ace *windows.ACCESS_ALLOWED_ACE
				if err := windows.GetAce(acl, index, &ace); err != nil || ace == nil {
					t.Logf("diagnostic ace=%d: read_error=%v", index, err)
					continue
				}
				if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
					sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
					t.Logf("diagnostic ace=%d: type=%d flags=%#x mask=%#x current_user=%t", index, ace.Header.AceType, ace.Header.AceFlags, ace.Mask, sid.Equals(user))
				} else {
					t.Logf("diagnostic ace=%d: unparsed_type=%d", index, ace.Header.AceType)
				}
			}
		}
	}
	native, nativeErr := openNT(parent, filepath.Base(path), baseAccess, options|windows.FILE_SYNCHRONOUS_IO_NONALERT, share)
	checkIdentity("NtCreateFile retained baseline", native, nativeErr)
	if nativeErr == nil {
		defer windows.CloseHandle(native)
	}
	t.Logf("diagnostic flags: baseline_access=%#x baseline_share=%#x win32_flags=%#x baseline_nt_options=%#x comparison_nt_options=%#x comparison_share=%#x", baseAccess, share, flags, options|windows.FILE_SYNCHRONOUS_IO_NONALERT, options, comparisonShare)
	type handleProbe struct {
		label string
		open  func() (windows.Handle, error)
	}
	for _, access := range []uint32{windows.READ_CONTROL, windows.READ_CONTROL | windows.WRITE_DAC, windows.READ_CONTROL | windows.WRITE_OWNER, windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER} {
		access |= windows.FILE_READ_ATTRIBUTES
		probes := []handleProbe{
			{"CreateFile no-follow", func() (windows.Handle, error) { return openPrivateWindowsSecurityHandle(path, directory, access) }},
			{"ReOpenFile from CreateFile", func() (windows.Handle, error) { return reOpenPrivateWindowsHandle(win32, access, directory) }},
			{"NtCreateFile parent-relative", func() (windows.Handle, error) {
				return openNT(parent, filepath.Base(path), access, options, comparisonShare)
			}},
		}
		if nativeErr == nil {
			probes = append(probes,
				handleProbe{"ReOpenFile from NtCreateFile", func() (windows.Handle, error) { return reOpenPrivateWindowsHandle(native, access, directory) }},
				handleProbe{"NtCreateFile empty-name", func() (windows.Handle, error) { return openNT(native, "", access, options, comparisonShare) }})
		}
		for _, probe := range probes {
			handle, err := probe.open()
			checkIdentity(fmt.Sprintf("%s access=%#x", probe.label, access), handle, err)
			if err == nil {
				if err := windows.CloseHandle(handle); err != nil {
					t.Errorf("%s: close_error=%v", probe.label, err)
				}
			}
		}
	}
}

func TestManagedAttestKeyWindowsRepairsPermissiveDACL(t *testing.T) {
	setAttestTestHome(t)
	path, err := attestKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	testSID := makeWindowsTestParentPermissive(t, configRoot)

	first, err := ensureAttestKey()
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsPathPrivateFromSID(t, filepath.Dir(filepath.Dir(path)), true, testSID)
	assertWindowsPathPrivateFromSID(t, filepath.Dir(path), true, testSID)
	assertWindowsPathPrivateFromSID(t, path, false, testSID)

	if err := setWindowsPathPermissive(t, path, false, testSID); err != nil {
		t.Fatal(err)
	}
	assertWindowsPathGrantsSID(t, path, testSID)
	second, err := ensureAttestKey()
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatal("repair changed the managed attestation key")
	}
	assertWindowsPathPrivateFromSID(t, path, false, testSID)
}

func TestArtifactOutputWindowsPrivacyFollowsSignedURLs(t *testing.T) {
	signedFile := artifactFile{
		Kind:          "proof",
		Name:          "proof.txt",
		URL:           "https://bucket.s3.amazonaws.com/proof.txt?X-Amz-Signature=deadbeef&X-Amz-Expires=900",
		snapshotValid: true,
		snapshotHash:  strings.Repeat("a", 64),
		snapshotSize:  1,
	}
	publicFile := artifactFile{
		Kind:          "proof",
		Name:          "proof.txt",
		URL:           "https://artifacts.example.com/proof.txt",
		snapshotValid: true,
		snapshotHash:  strings.Repeat("b", 64),
		snapshotSize:  1,
	}

	t.Run("private temporary is restricted at creation", func(t *testing.T) {
		dir := t.TempDir()
		testSID := makeWindowsTestParentPermissive(t, dir)
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		file, err := openArtifactBundleTemp(root, ".private.crabbox-test", privateRunOutputFileMode, true)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		assertWindowsPathPrivateFromSID(t, filepath.Join(dir, ".private.crabbox-test"), false, testSID)
	})

	t.Run("signed manifest and summary", func(t *testing.T) {
		dir := t.TempDir()
		testSID := makeWindowsTestParentPermissive(t, dir)
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		manifestPath := filepath.Join(dir, artifactManifestFilename)
		if err := os.WriteFile(manifestPath, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := setWindowsPathPermissive(t, manifestPath, false, testSID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := writeArtifactManifest(root, artifactPublishOptions{
			Directory: dir,
			Storage:   "broker",
		}, []artifactFile{signedFile}); err != nil {
			t.Fatal(err)
		}
		assertWindowsPathPrivateFromSID(t, manifestPath, false, testSID)

		summaryPath := filepath.Join(dir, "published-artifacts.md")
		if err := writePrivateArtifactBundleFile(root, "published-artifacts.md", []byte("signed summary")); err != nil {
			t.Fatal(err)
		}
		assertWindowsPathPrivateFromSID(t, summaryPath, false, testSID)

		if err := setWindowsPathPermissive(t, summaryPath, false, testSID); err != nil {
			t.Fatal(err)
		}
		if err := writePrivateArtifactBundleFile(root, "published-artifacts.md", []byte("republished signed summary")); err != nil {
			t.Fatal(err)
		}
		assertWindowsPathPrivateFromSID(t, summaryPath, false, testSID)
	})

	t.Run("public manifest and summary stay shareable", func(t *testing.T) {
		dir := t.TempDir()
		testSID := makeWindowsTestParentPermissive(t, dir)
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		if _, _, err := writeArtifactManifest(root, artifactPublishOptions{
			Directory: dir,
			Storage:   "local",
		}, []artifactFile{publicFile}); err != nil {
			t.Fatal(err)
		}
		assertWindowsPathGrantsSID(t, filepath.Join(dir, artifactManifestFilename), testSID)

		if err := writeArtifactBundleFile(root, "published-artifacts.md", []byte("public summary"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertWindowsPathGrantsSID(t, filepath.Join(dir, "published-artifacts.md"), testSID)
	})
}

func makeWindowsTestParentPermissive(t *testing.T, path string) *windows.SID {
	t.Helper()
	testSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := setWindowsPathPermissive(t, path, true, testSID); err != nil {
		t.Fatal(err)
	}
	return testSID
}

func setWindowsPathPermissive(t *testing.T, path string, directory bool, testSID *windows.SID) error {
	t.Helper()
	user, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	var pinner runtime.Pinner
	pinner.Pin(user)
	pinner.Pin(testSID)
	defer pinner.Unpin()
	inheritance := uint32(0)
	if directory {
		inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	entries := []windows.EXPLICIT_ACCESS{
		windowsTestAccessEntry(user, inheritance),
		windowsTestAccessEntry(testSID, inheritance),
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

func windowsTestAccessEntry(sid *windows.SID, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

func assertWindowsPathPrivateFromSID(t *testing.T, path string, directory bool, testSID *windows.SID) {
	t.Helper()
	if err := verifyPrivateWindowsPath(path, directory); err != nil {
		t.Fatalf("%s is not current-user private: %v", path, err)
	}
	if windowsPathGrantsSID(t, path, testSID) {
		t.Fatalf("%s grants access to test principal %s", path, testSID.String())
	}
}

func assertWindowsPathGrantsSID(t *testing.T, path string, sid *windows.SID) {
	t.Helper()
	if !windowsPathGrantsSID(t, path, sid) {
		t.Fatalf("%s does not retain the shareable grant for test principal %s", path, sid.String())
	}
}

func windowsPathGrantsSID(t *testing.T, path string, want *windows.SID) bool {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read DACL for %s: %v", path, err)
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			t.Fatal(err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(want) {
			return true
		}
	}
	return false
}

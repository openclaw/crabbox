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
	parent := t.TempDir()
	testSID := makeWindowsTestParentPermissive(t, parent)

	path := filepath.Join(parent, "downloads", "proof.txt")
	if err := writeRunDownloadFile(path, []byte("private proof")); err != nil {
		t.Fatal(err)
	}
	assertWindowsPathPrivateFromSID(t, filepath.Dir(path), true, testSID)
	assertWindowsPathPrivateFromSID(t, path, false, testSID)

	if err := setWindowsPathPermissive(t, path, false, testSID); err != nil {
		t.Fatal(err)
	}
	assertWindowsPathGrantsSID(t, path, testSID)
	if err := writeRunDownloadFile(path, []byte("replacement proof")); err != nil {
		t.Fatal(err)
	}
	assertWindowsPathPrivateFromSID(t, path, false, testSID)
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

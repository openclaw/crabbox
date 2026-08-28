//go:build windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

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

package runnerfs

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func physicalParentFixture(t *testing.T) (*Root, string) {
	t.Helper()
	root, dir := testRoot(t)
	writeFixture(t, filepath.Join(dir, "junit.xml"), "PASS")
	writeFixture(t, filepath.Join(dir, "nested", "junit.xml"), "FAIL")
	if err := os.Mkdir(filepath.Join(dir, "nested", "child"), 0700); err != nil {
		t.Fatal(err)
	}
	symlinkFixture(t, "nested/child", filepath.Join(dir, "alias"))
	return root, root.path
}

func TestReadPreservesPhysicalParentResolution(t *testing.T) {
	root, dir := physicalParentFixture(t)
	for _, name := range []string{"alias/../junit.xml", dir + "/alias/../junit.xml"} {
		file, err := root.Read(name, 64)
		if err != nil || string(file.Data) != "FAIL" {
			t.Errorf("Read(%q)=%q, %v", name, file.Data, err)
		}
	}
	physical, err := OpenRoot(dir + "/alias/..")
	if err != nil {
		t.Fatal(err)
	}
	defer physical.Close()
	file, err := physical.Read("junit.xml", 64)
	if err != nil || string(file.Data) != "FAIL" {
		t.Fatalf("physical root=%q, %v", file.Data, err)
	}
}

func TestRelativeRootsUsePhysicalWorkingDirectory(t *testing.T) {
	base := t.TempDir()
	physical := filepath.Join(base, "physical", "project")
	logical := filepath.Join(base, "logical")
	writeFixture(t, filepath.Join(physical, "junit.xml"), "report")
	if err := os.Mkdir(logical, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(logical, "alias")
	symlinkFixture(t, physical, alias)
	t.Chdir(alias)
	t.Setenv("PWD", alias)
	root, err := OpenRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := root.Read(filepath.Join(physical, "junit.xml"), 64)
	if err != nil || string(file.Data) != "report" {
		t.Errorf("absolute report under relative root: data=%q err=%v", file.Data, err)
	}
	parent, err := OpenRoot("..")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	file, err = parent.Read("project/junit.xml", 64)
	if err != nil || string(file.Data) != "report" {
		t.Errorf("physical parent root: data=%q err=%v", file.Data, err)
	}
	target, err := ArchiveTarget("destination", "payload", false)
	want, resolveErr := filepath.EvalSymlinks(physical)
	if err != nil || resolveErr != nil || target != filepath.Join(want, "destination") {
		t.Errorf("relative copy target=%q want=%q err=%v resolve=%v", target, want, err, resolveErr)
	}
}

func TestReportsUseOpenedIdentityNotLexicalPath(t *testing.T) {
	root, dir := physicalParentFixture(t)
	results, err := root.CollectResults(t.Context(), ResultOptions{Paths: []string{"junit.xml", "alias/../junit.xml", dir + "/alias/../junit.xml"}, ExplicitMaxBytes: 64, ExplicitTotalBytes: 128})
	if err != nil || len(results.Files) != 2 {
		t.Fatalf("files=%v err=%v", results.Files, err)
	}
	if string(results.Files[0].Data) != "PASS" || string(results.Files[1].Data) != "FAIL" {
		t.Fatalf("wrong reports: %v", results.Files)
	}
}

func TestReportsKeepDistinctCaseSensitiveFiles(t *testing.T) {
	root, dir := testRoot(t)
	if runtime.GOOS == "windows" {
		if output, err := exec.CommandContext(t.Context(), "fsutil.exe", "file", "setCaseSensitiveInfo", dir, "enable").CombinedOutput(); err != nil {
			t.Skipf("case-sensitive Windows directory unavailable: %v: %s", err, output)
		}
	}
	writeFixture(t, filepath.Join(dir, "JUNIT.xml"), "PASS")
	file, err := os.OpenFile(filepath.Join(dir, "junit.xml"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		t.Skip("fixture filesystem is not case sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("FAIL"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	results, err := root.CollectResults(t.Context(), ResultOptions{Paths: []string{"JUNIT.xml", "junit.xml"}, ExplicitMaxBytes: 64, ExplicitTotalBytes: 128})
	if err != nil || len(results.Files) != 2 {
		t.Fatalf("files=%v err=%v", results.Files, err)
	}
}

func TestArchivePreservesPhysicalParentResolution(t *testing.T) {
	_, dir := physicalParentFixture(t)
	source, archive, err := CreateArchive(t.Context(), dir+"/alias/../junit.xml", CreateOptions{}, DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "FAIL" || source.Base != "junit.xml" {
		t.Fatalf("source=%v data=%q err=%v", source, data, err)
	}
	for _, suffix := range []string{"destination", "destination/"} {
		target, err := ArchiveTarget(dir+"/alias/../"+suffix, "copied.xml", true)
		expected := filepath.Join(dir, "nested", "destination")
		if err != nil || target != expected {
			t.Errorf("target=%q want=%q err=%v", target, expected, err)
		}
	}
}

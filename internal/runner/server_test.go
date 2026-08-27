package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

func testIdentity() Identity {
	return Identity{BuildID: "test-build", OS: runtime.GOOS, Arch: runtime.GOARCH, Protocol: runnerwire.Version}
}

func TestCollectRejectsExcessExplicitPathsBeforeIO(t *testing.T) {
	paths := make([]string, 4097)
	for i := range paths {
		paths[i] = "report.xml"
	}
	request := Request{BuildID: testIdentity().BuildID, Operation: Collect, Workdir: filepath.Join(t.TempDir(), "missing"), Paths: paths}
	t.Run("request", func(t *testing.T) {
		var wire bytes.Buffer
		if err := WriteRequest(&wire, request, 0, nil); err == nil || !strings.Contains(err.Error(), "explicit result path limit") || wire.Len() != 0 {
			t.Fatalf("request wrote %d bytes: %v", wire.Len(), err)
		}
	})
	t.Run("client", func(t *testing.T) {
		called := false
		client := Client{Identity: testIdentity(), Transport: func(context.Context, io.Reader, io.Writer) error { called = true; return nil }}
		_, err := client.Collect(t.Context(), request.Workdir, paths, false, "")
		if err == nil || !strings.Contains(err.Error(), "explicit result path limit") || called {
			t.Fatalf("transport called=%t err=%v", called, err)
		}
	})
	t.Run("server", func(t *testing.T) {
		var input, output bytes.Buffer
		if err := writeFrame(&input, runnerwire.Request, request, 0, nil); err != nil {
			t.Fatal(err)
		}
		if err := writeFrame(&input, runnerwire.End, nil, 0, nil); err != nil {
			t.Fatal(err)
		}
		if err := Serve(t.Context(), &input, &output, testIdentity()); err == nil || !strings.Contains(err.Error(), "explicit result path limit") {
			t.Fatalf("server did not reject count before filesystem access: %v", err)
		}
		_, err := ReadResponse(t.Context(), &output, testIdentity(), Collect, func(FileInfo, io.Reader) error { t.Error("server emitted file"); return nil })
		if err == nil || !strings.Contains(err.Error(), "explicit result path limit") {
			t.Fatalf("invalid refusal response: %v", err)
		}
	})
}

func TestCollectMaximumExplicitAndAutoFiles(t *testing.T) {
	root := t.TempDir()
	paths := make([]string, 4096)
	for i := range paths {
		paths[i] = fmt.Sprintf("r%04d", i)
		if err := os.WriteFile(filepath.Join(root, paths[i]), []byte("<testsuite/>"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range runnerfs.AutoMaxFiles {
		name := fmt.Sprintf("junit-%02d.xml", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("<testsuite/>"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	client := Client{Identity: testIdentity(), Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		return Serve(ctx, input, output, testIdentity())
	}}
	results, err := client.Collect(t.Context(), root, paths, true, "")
	if err != nil || len(results.Files) != len(paths)+runnerfs.AutoMaxFiles || len(results.Warnings) != 0 {
		t.Fatalf("files=%d warnings=%v err=%v", len(results.Files), results.Warnings, err)
	}
}

func TestResultWarningOverflowPreservesValidFiles(t *testing.T) {
	root := t.TempDir()
	paths := []string{"valid"}
	if err := os.WriteFile(filepath.Join(root, "valid"), []byte("<testsuite/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(root, "large")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(ExplicitMaxFileBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	for index := range 1400 {
		name := fmt.Sprintf("oversized-%04d", index)
		if err := os.Link(large, filepath.Join(root, name)); err != nil {
			t.Skipf("hardlink fixture unavailable: %v", err)
		}
		paths = append(paths, name)
	}
	client := Client{Identity: testIdentity(), Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		return Serve(ctx, input, output, testIdentity())
	}}
	result, err := client.Collect(t.Context(), root, paths, false, "")
	if err != nil || len(result.Files) != 1 || result.Files[0].Path != "valid" || len(result.Warnings) == 0 || !strings.Contains(result.Warnings[len(result.Warnings)-1].Message, "omitted") {
		t.Fatalf("files=%d warnings=%d err=%v", len(result.Files), len(result.Warnings), err)
	}
}

func TestRootExpansionIsSeparateFromDownloadRestrictions(t *testing.T) {
	if value, err := expandPath("/"); err != nil || value != "/" {
		t.Fatalf("root workspace/destination rejected: %q %v", value, err)
	}
	for _, name := range []string{"/", "~", "~/", "/path/.", "/path/..", "../"} {
		if _, err := runnerfs.DownloadSource(name); err == nil {
			t.Fatalf("unsafe download source accepted: %q", name)
		}
	}
}

func invoke(t *testing.T, request Request, payload []byte) ([]byte, error) {
	t.Helper()
	request.BuildID = testIdentity().BuildID
	var input, output bytes.Buffer
	if err := WriteRequest(&input, request, uint64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	err := Serve(t.Context(), &input, &output, testIdentity())
	return output.Bytes(), err
}

func TestServeResultsPreservesBinaryAndMarkerText(t *testing.T) {
	root := t.TempDir()
	data := []byte("\x00\xff\n__CRABBOX_RESULT_FILE__:outside\n__CRABBOX_RESULT_WARNING__:fake\nCBXR\n")
	if err := os.WriteFile(filepath.Join(root, "report"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	response, err := invoke(t, Request{Operation: Collect, Workdir: root, Paths: []string{"report"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var files []runnerfs.File
	outcome, err := ReadResponse(t.Context(), bytes.NewReader(response), testIdentity(), Collect, func(info FileInfo, body io.Reader) error {
		value, err := io.ReadAll(body)
		files = append(files, runnerfs.File{Path: info.Path, Data: value})
		return err
	})
	if err != nil || outcome.Published || len(files) != 1 || files[0].Path != "report" || !bytes.Equal(files[0].Data, data) {
		t.Fatalf("files=%v outcome=%v err=%v", files, outcome, err)
	}
}

func TestServeArchiveUploadDownloadUsesSameValidator(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX archive endpoint")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "run"), []byte("proof\x00\xff"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, archive, err := runnerfs.CreateArchive(t.Context(), source, runnerfs.CreateOptions{}, runnerfs.DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	payload, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	response, err := invoke(t, Request{Operation: Upload, Destination: target, Source: metadata}, payload)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := ReadResponse(t.Context(), bytes.NewReader(response), testIdentity(), Upload, nil)
	if err != nil || !outcome.Published {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	info, err := os.Stat(filepath.Join(target, "run"))
	if err != nil || info.Mode().Perm() != 0o751 {
		t.Fatalf("info=%v err=%v", info, err)
	}
	response, err = invoke(t, Request{Operation: Download, SourcePath: target + "/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(root, "download")
	var staged *runnerfs.StagedArchive
	_, err = ReadResponse(t.Context(), bytes.NewReader(response), testIdentity(), Download, func(info FileInfo, body io.Reader) error {
		if !info.Source.ContentsOnly || info.Source.Base != "target" {
			t.Fatalf("source=%v", info.Source)
		}
		var stageErr error
		staged, stageErr = runnerfs.StageArchive(t.Context(), body, download, runnerfs.ArchivePayloadRoot, runnerfs.ExtractOptions{}, runnerfs.DefaultArchiveLimits())
		return stageErr
	})
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	if err := staged.Publish(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(download, "run"))
	if err != nil || string(data) != "proof\x00\xff" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	info, err = os.Stat(filepath.Join(download, "run"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("info=%v err=%v", info, err)
	}
	if info, err := os.Stat(filepath.Join(download, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty=%v err=%v", info, err)
	}
}

func TestServeUploadNeverPublishesTruncatedRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX archive endpoint")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, archive, err := runnerfs.CreateArchive(t.Context(), source, runnerfs.CreateOptions{}, runnerfs.DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		t.Fatal(err)
	}
	var request bytes.Buffer
	if err := WriteRequest(&request, Request{BuildID: testIdentity().BuildID, Operation: Upload, Destination: target, Source: metadata}, uint64(info.Size()), archive); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []int{1, 10, int(info.Size())} {
		var response bytes.Buffer
		if err := Serve(t.Context(), bytes.NewReader(request.Bytes()[:request.Len()-missing]), &response, testIdentity()); err == nil {
			t.Fatal("truncated upload succeeded")
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "old" {
			t.Fatalf("target=%q err=%v", data, err)
		}
		entries, err := filepath.Glob(filepath.Join(root, ".crabbox-cp-*"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("staging leaked: %v err=%v", entries, err)
		}
	}
	var extraEnd bytes.Buffer
	if err := writeFrame(&extraEnd, runnerwire.End, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range [][]byte{{'x'}, extraEnd.Bytes()} {
		var response bytes.Buffer
		malformed := append(append([]byte(nil), request.Bytes()...), suffix...)
		if err := Serve(t.Context(), bytes.NewReader(malformed), &response, testIdentity()); err == nil {
			t.Fatal("upload with invalid stream suffix succeeded")
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "old" {
			t.Fatalf("invalid stream published: target=%q err=%v", data, err)
		}
	}
	var response bytes.Buffer
	aliased := bytes.Replace(request.Bytes(), []byte(`"base":`), []byte(`"Base":`), 1)
	if err := Serve(t.Context(), bytes.NewReader(aliased), &response, testIdentity()); err == nil {
		t.Fatal("nested case alias accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "old" {
		t.Fatalf("nested case alias published: target=%q err=%v", data, err)
	}
}

func TestReadResponseRequiresIdentityCompletionAndEnd(t *testing.T) {
	var complete bytes.Buffer
	if err := writeFrame(&complete, runnerwire.Hello, testIdentity(), 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(&complete, runnerwire.Result, Outcome{}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(&complete, runnerwire.End, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	for end := 0; end < complete.Len(); end++ {
		if _, err := ReadResponse(t.Context(), bytes.NewReader(complete.Bytes()[:end]), testIdentity(), Collect, nil); err == nil {
			t.Fatalf("truncation %d accepted", end)
		}
	}
	wrong := testIdentity()
	wrong.BuildID = "another-build"
	if _, err := ReadResponse(t.Context(), bytes.NewReader(complete.Bytes()), wrong, Collect, nil); err == nil {
		t.Fatal("wrong build accepted")
	}
	if _, err := ReadResponse(t.Context(), bytes.NewReader(append(complete.Bytes(), 'x')), testIdentity(), Collect, nil); err == nil {
		t.Fatal("trailing bytes accepted")
	}
}

func TestServeRejectsBuildMismatchBeforeEffects(t *testing.T) {
	var input, output bytes.Buffer
	if err := WriteRequest(&input, Request{BuildID: "wrong", Operation: Collect, Workdir: t.TempDir()}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := Serve(t.Context(), &input, &output, testIdentity()); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("err=%v", err)
	}
	_, err := ReadResponse(t.Context(), &output, testIdentity(), Collect, nil)
	var remote RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("err=%v", err)
	}
}

func TestServeCancelledBeforeOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var input, output bytes.Buffer
	if err := WriteRequest(&input, Request{BuildID: testIdentity().BuildID, Operation: Collect, Workdir: t.TempDir()}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := Serve(ctx, &input, &output, testIdentity()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

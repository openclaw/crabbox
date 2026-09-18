package runner

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func TestDevelopmentSourceNativeInvocation(t *testing.T) {
	target := runtimeartifact.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}
	artifact, err := DevelopmentSource().Open(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	dir := t.TempDir()
	name := "runner"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	program := filepath.Join(dir, name)
	data, err := io.ReadAll(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, data, 0700); err != nil {
		t.Fatal(err)
	}
	want := []byte("<testsuite name=\"owned\" tests=\"0\"/>\n")
	if err := os.WriteFile(filepath.Join(dir, "report.xml"), want, 0600); err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	id := artifact.Identity()
	if err := WriteRequest(&input, Request{BuildID: id.BuildID, Operation: Collect, Workdir: dir, Paths: []string{"report.xml"}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), program, Command, "serve")
	command.Dir = dir
	command.Env = []string{"HOME=" + dir, "USERPROFILE=" + dir}
	if value := os.Getenv("SYSTEMROOT"); value != "" {
		command.Env = append(command.Env, "SYSTEMROOT="+value)
	}
	command.Stdin = &input
	var output, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostic
	if err := command.Run(); err != nil {
		t.Fatalf("development runtime: %v; %s", err, diagnostic.String())
	}
	count := 0
	_, err = ReadResponse(t.Context(), &output, Identity{BuildID: id.BuildID, OS: target.OS, Arch: target.Arch, Protocol: 1}, Collect, func(info FileInfo, body io.Reader) error {
		count++
		got, err := io.ReadAll(body)
		if err == nil && (info.Path != "report.xml" || !bytes.Equal(got, want)) {
			t.Fatalf("report changed: %q %q", info.Path, got)
		}
		return err
	})
	if err != nil || count != 1 {
		t.Fatalf("development response: files=%d error=%v", count, err)
	}
}

func TestDevelopmentSourceSelectedTargetAndCache(t *testing.T) {
	developmentArtifacts.Lock()
	developmentArtifacts.artifacts = make(map[developmentArtifactKey][]byte)
	developmentArtifacts.Unlock()
	t.Setenv("GOFLAGS", "--invalid-development-test-flag")
	t.Setenv("GOWORK", "invalid-development-test-workspace")
	target := runtimeartifact.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}
	source := DevelopmentSource()
	first, err := source.Open(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstBytes, err := io.ReadAll(first)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := SourceID()
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := hex.DecodeString(sourceID); err != nil || len(decoded) != 32 {
		t.Fatalf("invalid source fingerprint %q", sourceID)
	}
	if first.Identity().Target != target || first.Identity().BuildID != sourceID {
		t.Fatalf("identity: %+v", first.Identity())
	}
	// A warm source opens an independent byte stream without a compiler on PATH.
	t.Setenv("PATH", t.TempDir())
	second, err := source.Open(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondBytes, err := io.ReadAll(second)
	if err != nil || !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("cached stream: %v", err)
	}
	other := runtimeartifact.Target{OS: "linux", Arch: "amd64"}
	if other == target {
		other.Arch = "arm64"
	}
	if _, err := source.Open(t.Context(), other); err == nil {
		t.Fatal("unbuilt target unexpectedly present in selected-target cache")
	}
}

func TestDevelopmentSourceCanceledAndUnsupported(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := DevelopmentSource().Open(ctx, Target{OS: "linux", Arch: "amd64"}); err != context.Canceled {
		t.Fatalf("canceled: %v", err)
	}
	if _, err := DevelopmentSource().Open(t.Context(), Target{OS: "freebsd", Arch: "amd64"}); err == nil {
		t.Fatal("unsupported target accepted")
	}
}

func TestSourceIDFromFSMatchesEmbeddedAndSelectsInputs(t *testing.T) {
	source := fstest.MapFS{}
	err := fs.WalkDir(helperSources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := helperSources.ReadFile(name)
		source[name] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want, err := SourceID()
	if err != nil {
		t.Fatal(err)
	}
	check := func() string {
		t.Helper()
		got, err := SourceIDFromFS(source)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := check(); got != want {
		t.Fatalf("disk fingerprint %s differs from embedded %s", got, want)
	}
	source["distribution.go"] = &fstest.MapFile{Data: []byte("excluded")}
	source["runnerfs/extra_test.go"] = &fstest.MapFile{Data: []byte("excluded")}
	if check() != want {
		t.Fatal("excluded files changed fingerprint")
	}
	for _, name := range []string{"development/go.mod.txt", "development/main.go.txt", "server.go", "runnerfs/new.go", "runnerwire/new.go"} {
		old, existed := source[name]
		source[name] = &fstest.MapFile{Data: []byte("changed")}
		if check() == want {
			t.Fatalf("%s did not change fingerprint", name)
		}
		if existed {
			source[name] = old
		} else {
			delete(source, name)
		}
	}
	source["server.go"] = &fstest.MapFile{Data: []byte(strings.Repeat("x", (4<<20)+1))}
	if _, err := SourceIDFromFS(source); err == nil {
		t.Fatal("oversize input accepted")
	}
}

func TestSourceIDFromFSMatchesWorkingSource(t *testing.T) {
	root, err := os.OpenRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got, err := SourceIDFromFS(root.FS())
	if err != nil {
		t.Fatal(err)
	}
	want, err := SourceID()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("source fingerprint %s differs from embedded %s", got, want)
	}
}

package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDevelopmentArtifactRunsWithoutSourceOrModuleDownloads(t *testing.T) {
	if testing.Short() {
		t.Skip("build and run embedded helper source")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("Go toolchain unavailable")
	}
	// These settings must not enter the generated build.
	t.Setenv("GOFLAGS", "-invalid-operator-flag")
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "missing.work"))
	t.Setenv("GOPROXY", "http://127.0.0.1:1")
	artifact, err := DevelopmentArtifact(t.Context(), Target{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "runner")
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(name, artifact.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	data := []byte("\x00\xff__CRABBOX_RESULT_FILE__:fake\nCBXR")
	if err := os.WriteFile(filepath.Join(root, "result"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	client := Client{Identity: artifact.Identity, Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		command := exec.CommandContext(ctx, name, "serve")
		command.Dir = t.TempDir()
		command.Stdin, command.Stdout = input, output
		return command.Run()
	}}
	results, err := client.Collect(t.Context(), root, []string{"result"}, false, "")
	if err != nil || len(results.Files) != 1 || !bytes.Equal(results.Files[0].Data, data) {
		t.Fatalf("results=%v err=%v", results, err)
	}
	var request bytes.Buffer
	if err := WriteRequest(&request, Request{BuildID: artifact.Identity.BuildID, Operation: Collect, Workdir: root, Paths: []string{"result"}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), name, "serve-base64")
	command.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(request.Bytes()))
	encoded, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var received []byte
	_, err = ReadResponse(t.Context(), base64.NewDecoder(base64.StdEncoding, bytes.NewReader(encoded)), artifact.Identity, Collect, func(_ FileInfo, body io.Reader) error { received, err = io.ReadAll(body); return err })
	if err != nil || !bytes.Equal(received, data) {
		t.Fatalf("base64 data=%q err=%v", received, err)
	}
}

func TestHelperBundleContainsNoDistributionOrDependencies(t *testing.T) {
	files, err := sourceFiles()
	if err != nil {
		t.Fatal(err)
	}
	for name := range files {
		if strings.HasSuffix(name, "_test.go") || strings.Contains(name, "bundle") || strings.Contains(name, "embedded") {
			t.Fatalf("unexpected bundled source %q", name)
		}
	}
	id, err := SourceID()
	if err != nil || len(id) != 64 {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if _, err := DevelopmentArtifact(t.Context(), Target{OS: "linux", Arch: "mips"}); err == nil {
		t.Fatal("unsupported target accepted")
	}
}

func TestEmbeddedArtifactDoesNotNeedOperatorGo(t *testing.T) {
	if embeddedRunnerBundle() == nil {
		t.Skip("runnerembed build required")
	}
	if BundleBuildID == "" {
		t.Fatal("embedded test must pin BundleBuildID")
	}
	t.Setenv("PATH", t.TempDir())
	var probe bytes.Buffer
	if err := VerifyEmbeddedRuntime(t.Context(), &probe); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Identity           Identity `json:"identity"`
		SHA256             string   `json:"sha256"`
		TrustPolicyVersion int      `json:"trustPolicyVersion"`
		Verified           bool     `json:"verified"`
	}
	if err := json.Unmarshal(probe.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	artifact, err := ArtifactFor(t.Context(), Target{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != artifact.Identity || result.SHA256 != artifact.SHA256 || !result.Verified || result.TrustPolicyVersion != 1 {
		t.Fatalf("incorrect release runner probe: %s", probe.Bytes())
	}
}

func TestEmbeddedRuntimeVerificationRejectsDevelopmentFallback(t *testing.T) {
	if embeddedRunnerBundle() != nil {
		t.Skip("development build required")
	}
	if err := VerifyEmbeddedRuntime(t.Context(), io.Discard); err == nil {
		t.Fatal("release probe silently used development fallback")
	}
}

func TestEmbeddedRuntimeVerificationRejectsWrongBuildID(t *testing.T) {
	if embeddedRunnerBundle() == nil {
		t.Skip("runnerembed build required")
	}
	previous := BundleBuildID
	t.Cleanup(func() { BundleBuildID = previous })
	BundleBuildID = strings.Repeat("0", 40)
	if err := VerifyEmbeddedRuntime(t.Context(), io.Discard); err == nil {
		t.Fatal("release probe accepted mismatched injected build identity")
	}
}

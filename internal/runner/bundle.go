package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

// Only the dependency-free helper implementation is included. In particular,
// distribution code and release payloads never recursively embed themselves.
//
//go:embed server.go protocol.go main.go client.go transport.go runnerfs/*.go runnerwire/*.go
var helperSources embed.FS

const helperModule = "module github.com/openclaw/crabbox\n\ngo 1.26.0\n"
const helperMain = `package main
import ("context"; "os"; "os/signal"; "syscall"; "github.com/openclaw/crabbox/internal/runner")
func main() {
 ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
 defer stop()
 os.Exit(runner.Main(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
`

type Target struct{ OS, Arch string }

func (target Target) String() string { return target.OS + "-" + target.Arch }

func (target Target) validate() error {
	if (target.OS != "darwin" && target.OS != "linux" && target.OS != "windows") || (target.Arch != "amd64" && target.Arch != "arm64") {
		return fmt.Errorf("unsupported runner target %s", target)
	}
	return nil
}

type Artifact struct {
	Identity Identity
	SHA256   string
	Data     []byte
}

func sourceFiles() (map[string][]byte, error) {
	files := map[string][]byte{"go.mod": []byte(helperModule), "cmd/crabbox-runner/main.go": []byte(helperMain)}
	err := fs.WalkDir(helperSources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data, err := helperSources.ReadFile(name)
		if err != nil {
			return err
		}
		files["internal/runner/"+name] = data
		return nil
	})
	return files, err
}

func SourceID() (string, error) {
	files, err := sourceFiles()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	// WalkDir sorts the embedded names. Hash the module and entrypoint as well.
	_, _ = hash.Write([]byte(helperModule))
	_, _ = hash.Write([]byte(helperMain))
	err = fs.WalkDir(helperSources, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data := files["internal/runner/"+name]
		_, _ = fmt.Fprintf(hash, "%d:%s:%d:", len(name), name, len(data))
		_, _ = hash.Write(data)
		return nil
	})
	return hex.EncodeToString(hash.Sum(nil)), err
}

var developmentArtifacts = struct {
	sync.Mutex
	artifacts map[Target]Artifact
}{artifacts: make(map[Target]Artifact)}

// DevelopmentArtifact compiles bundled source on the operator, never on the
// target. Official packaging embeds prebuilt artifacts instead. No dependencies
// are downloaded, and the user's Go workspace, flags and toolchain auto-upgrade
// settings cannot change this build.
func DevelopmentArtifact(ctx context.Context, target Target) (Artifact, error) {
	if err := target.validate(); err != nil {
		return Artifact{}, err
	}
	developmentArtifacts.Lock()
	defer developmentArtifacts.Unlock()
	if artifact, ok := developmentArtifacts.artifacts[target]; ok {
		return artifact, nil
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return Artifact{}, errors.New("this development CLI requires Go 1.26 or later on the operator to build its runner; use an official bundled CLI otherwise")
	}
	sourceID, err := SourceID()
	if err != nil {
		return Artifact{}, err
	}
	files, err := sourceFiles()
	if err != nil {
		return Artifact{}, err
	}
	stage, err := os.MkdirTemp("", "crabbox-runner-build-*")
	if err != nil {
		return Artifact{}, err
	}
	defer os.RemoveAll(stage)
	for name, data := range files {
		destination := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return Artifact{}, err
		}
		if err := os.WriteFile(destination, data, 0o600); err != nil {
			return Artifact{}, err
		}
	}
	binary := filepath.Join(stage, "runner")
	command := exec.CommandContext(ctx, goTool, "build", "-p", "1", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -X github.com/openclaw/crabbox/internal/runner.BuildID="+sourceID, "-o", binary, "./cmd/crabbox-runner")
	command.Dir = stage
	command.Env = helperBuildEnvironment(target)
	var output bytes.Buffer
	command.Stdout = &limitedOutput{writer: &output, remaining: 64 << 10}
	command.Stderr = command.Stdout
	if err := command.Run(); err != nil {
		return Artifact{}, fmt.Errorf("build development runner for %s (Go 1.26 or later required): %w: %s", target, err, strings.TrimSpace(output.String()))
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		return Artifact{}, err
	}
	digest := sha256.Sum256(data)
	artifact := Artifact{Identity: Identity{BuildID: sourceID, OS: target.OS, Arch: target.Arch, Protocol: runnerwire.Version}, SHA256: hex.EncodeToString(digest[:]), Data: data}
	developmentArtifacts.artifacts[target] = artifact
	return artifact, nil
}

func helperBuildEnvironment(target Target) []string {
	var result []string
	for _, name := range []string{"PATH", "HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "TMPDIR", "GOCACHE", "GOPATH", "GOROOT"} {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	return append(result, "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0", "GOOS="+target.OS, "GOARCH="+target.Arch)
}

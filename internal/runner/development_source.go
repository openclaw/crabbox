package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/openclaw/crabbox/internal/runner/runnerwire"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

// Only the dependency-free helper implementation is included. In particular,
// distribution code and release payloads never recursively embed themselves.
//
//go:embed server.go protocol.go main.go client.go transport.go runnerfs/*.go runnerwire/*.go development/*.txt
var helperSources embed.FS

type Target = runtimeartifact.Target

func sourceFiles() (map[string][]byte, error) { return sourceFilesFromFS(helperSources) }

// sourceFilesFromFS reads only the dependency-free helper inputs. Paths are
// relative to internal/runner, whether embedded or opened from frozen source.
func sourceFilesFromFS(source fs.FS) (map[string][]byte, error) {
	inputs := map[string]string{
		"go.mod":                     "development/go.mod.txt",
		"cmd/crabbox-runner/main.go": "development/main.go.txt",
	}
	for _, name := range []string{"server.go", "protocol.go", "main.go", "client.go", "transport.go"} {
		inputs["internal/runner/"+name] = name
	}
	for _, dir := range []string{"runnerfs", "runnerwire"} {
		f, err := source.Open(dir)
		if err != nil {
			return nil, err
		}
		entriesFile, ok := f.(fs.ReadDirFile)
		if !ok {
			f.Close()
			return nil, fmt.Errorf("source directory %s cannot be listed", dir)
		}
		entries, err := entriesFile.ReadDir(4097)
		f.Close()
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(entries) > 4096 {
			return nil, fmt.Errorf("too many entries in source directory %s", dir)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if !entry.Type().IsRegular() {
				return nil, fmt.Errorf("non-regular helper source %s/%s", dir, name)
			}
			inputs["internal/runner/"+dir+"/"+name] = dir + "/" + name
		}
	}
	files := make(map[string][]byte, len(inputs))
	total := 0
	for destination, name := range inputs {
		f, err := source.Open(name)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 4<<20 {
			f.Close()
			return nil, fmt.Errorf("invalid helper source %s", name)
		}
		data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		total += len(data)
		if len(data) > 4<<20 || total > 32<<20 {
			return nil, errors.New("helper source exceeds size limit")
		}
		files[destination] = data
	}
	return files, nil
}

func SourceID() (string, error) { return SourceIDFromFS(helperSources) }

// SourceIDFromFS fingerprints helper inputs without parsing or executing source.
// source must be rooted at internal/runner and confine access to that directory.
func SourceIDFromFS(source fs.FS) (string, error) {
	files, err := sourceFilesFromFS(source)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		data := files[name]
		_, _ = fmt.Fprintf(hash, "%d:%s:%d:", len(name), name, len(data))
		_, _ = hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type developmentArtifactKey struct {
	Target   Target
	SourceID string
}

var developmentArtifacts = struct {
	sync.Mutex
	artifacts map[developmentArtifactKey][]byte
}{artifacts: make(map[developmentArtifactKey][]byte)}

// DevelopmentSource explicitly opts the filesystem caller into local compilation.
// It never participates in supervisor fallback or official pack discovery.
func DevelopmentSource() runtimeartifact.Source { return developmentSource{} }

type developmentSource struct{}

// Open compiles only the requested target and returns an independently owned
// verified byte stream. Failed builds are not cached.
func (developmentSource) Open(ctx context.Context, target Target) (*runtimeartifact.Artifact, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if (target.OS != "darwin" && target.OS != "linux" && target.OS != "windows") || (target.Arch != "amd64" && target.Arch != "arm64") {
		return nil, fmt.Errorf("unsupported runner target %s/%s", target.OS, target.Arch)
	}
	sourceID, err := SourceID()
	if err != nil {
		return nil, err
	}
	key := developmentArtifactKey{Target: target, SourceID: sourceID}
	developmentArtifacts.Lock()
	defer developmentArtifacts.Unlock()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if data, ok := developmentArtifacts.artifacts[key]; ok {
		return runtimeartifact.NewFilesystemBytes(ctx, data, target, sourceID, strconv.Itoa(runnerwire.Version))
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return nil, errors.New("this development CLI requires Go 1.26 or later on the operator to build its runner; use an official bundled CLI otherwise")
	}
	files, err := sourceFiles()
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp("", "crabbox-runner-build-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	for name, data := range files {
		destination := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(destination, data, 0o600); err != nil {
			return nil, err
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
		return nil, errors.Join(context.Cause(ctx), fmt.Errorf("build development runner for %s/%s (Go 1.26 or later required): %w: %s", target.OS, target.Arch, err, strings.TrimSpace(output.String())))
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	artifact, err := runtimeartifact.NewFilesystemBytes(ctx, data, target, sourceID, strconv.Itoa(runnerwire.Version))
	if err != nil {
		return nil, err
	}
	developmentArtifacts.artifacts[key] = data
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

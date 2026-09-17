// Package runtimeartifact validates operator-provided local runtime artifact sets.
// A set binds exact controller and runtime bytes; its trusted producer, not this
// manifest, is responsible for building both from the same frozen inputs. Keep
// the local files unchanged while a returned artifact is in use.
package runtimeartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
)

const (
	maxManifestSize = 64 << 10
	maxMetadataRead = 8 << 20
	runtimePackage  = "github.com/openclaw/crabbox/cmd/crabbox-runtime"
)

// Target identifies a supported runtime platform.
type Target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// Identity describes the validated bytes. ProtocolVersion is the producer's
// claim; the caller must also verify the installed runtime's protocol handshake.
type Identity struct {
	Target          Target
	ProtocolVersion string
	Size            int64
	SHA256          string
}

// Artifact is the already-open validated executable, initially positioned at
// byte zero. Upload from this stream instead of reopening its manifest path.
// The caller must Close it. Metadata is inspected without executing the file.
type Artifact struct {
	file     *os.File
	identity Identity
}

func (a *Artifact) Read(p []byte) (int, error)                   { return a.file.Read(p) }
func (a *Artifact) Seek(offset int64, whence int) (int64, error) { return a.file.Seek(offset, whence) }
func (a *Artifact) Close() error                                 { return a.file.Close() }
func (a *Artifact) Identity() Identity                           { return a.identity }

type manifest struct {
	SchemaVersion    int     `json:"schemaVersion"`
	ProtocolVersion  string  `json:"protocolVersion"`
	ControllerSHA256 string  `json:"controllerSha256"`
	Artifacts        []entry `json:"artifacts"`
}

type entry struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// LocalSet is an immutable manifest and controller-binding snapshot for one
// operation. It holds no open files. Each Open verifies companion bytes against
// the frozen manifest; it neither rereads the manifest nor rehashes the controller.
// The trusted producer still owns common-source pairing and pack immutability.
type LocalSet struct {
	directory string
	manifest  *manifest
}

// OpenLocalSet snapshots an explicitly trusted manifest and verifies its binding
// to the actual controller executable, normally resolved by os.Executable. It
// records an absolute pack directory so later opens are independent of cwd.
func OpenLocalSet(ctx context.Context, manifestPath, controllerPath, expectedProtocol string) (*LocalSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if expectedProtocol == "" {
		return nil, fmt.Errorf("expected protocol must not be empty")
	}
	manifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime manifest: %w", err)
	}
	m, err := readManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("runtime manifest: %w", err)
	}
	if err := validateManifest(m, expectedProtocol); err != nil {
		return nil, err
	}
	controller, err := os.Open(controllerPath)
	if err != nil {
		return nil, fmt.Errorf("open controller: %w", err)
	}
	defer controller.Close()
	controllerHash, err := fileDigest(ctx, controller, 0)
	if err != nil {
		return nil, fmt.Errorf("hash controller: %w", err)
	}
	if controllerHash != m.ControllerSHA256 {
		return nil, fmt.Errorf("controller SHA-256 mismatch; produce an artifact set for this controller")
	}
	return &LocalSet{directory: filepath.Dir(manifestPath), manifest: m}, nil
}

// OpenLocal selects and verifies one companion through a fresh LocalSet. Use
// OpenLocalSet once when an operation needs multiple target opens. No network,
// compilation, provider access, or executable invocation occurs here. The caller
// owns target CPU compatibility with the producer's Go feature-level flags.
func OpenLocal(ctx context.Context, manifestPath, controllerPath string, target Target, expectedProtocol string) (*Artifact, error) {
	set, err := OpenLocalSet(ctx, manifestPath, controllerPath, expectedProtocol)
	if err != nil {
		return nil, err
	}
	return set.Open(ctx, target)
}

// Open verifies the current companion file's size, hash, and executable metadata
// against this snapshot and returns a new stream positioned at zero. Manifest
// edits cannot change the selection. The caller must Close every returned stream.
func (s *LocalSet) Open(ctx context.Context, target Target) (*Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.manifest == nil {
		return nil, fmt.Errorf("a verified local artifact set is required")
	}
	if !supported(target) {
		return nil, fmt.Errorf("unsupported runtime target %s/%s", target.OS, target.Arch)
	}
	var selected *entry
	for i := range s.manifest.Artifacts {
		e := &s.manifest.Artifacts[i]
		if (Target{e.OS, e.Arch}) == target {
			selected = e
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("runtime artifact missing for %s/%s", target.OS, target.Arch)
	}
	f, err := openPackFile(s.directory, selected.Path)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	digest, err := fileDigest(ctx, f, selected.Size)
	if err != nil {
		return nil, fmt.Errorf("verify artifact: %w", err)
	}
	if digest != selected.SHA256 {
		return nil, fmt.Errorf("artifact SHA-256 mismatch")
	}
	if err := inspectExecutable(ctx, f, target); err != nil {
		return nil, fmt.Errorf("inspect artifact: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	ok = true
	return &Artifact{file: f, identity: Identity{target, s.manifest.ProtocolVersion, selected.Size, digest}}, nil
}

func validateManifest(m *manifest, expectedProtocol string) error {
	if expectedProtocol == "" {
		return fmt.Errorf("expected protocol must not be empty")
	}
	if m.SchemaVersion != 1 {
		return fmt.Errorf("unsupported runtime schema version %d", m.SchemaVersion)
	}
	if m.ProtocolVersion != expectedProtocol {
		return fmt.Errorf("runtime protocol mismatch: got %q, want %q", m.ProtocolVersion, expectedProtocol)
	}
	if !validDigest(m.ControllerSHA256) {
		return fmt.Errorf("invalid controller SHA-256")
	}
	if len(m.Artifacts) == 0 {
		return fmt.Errorf("runtime manifest contains no artifacts")
	}
	seen := make(map[Target]bool)
	for i := range m.Artifacts {
		e := &m.Artifacts[i]
		t := Target{e.OS, e.Arch}
		if !supported(t) {
			return fmt.Errorf("unsupported artifact target %s/%s", t.OS, t.Arch)
		}
		if seen[t] {
			return fmt.Errorf("duplicate artifact target %s/%s", t.OS, t.Arch)
		}
		seen[t] = true
		if !validPackPath(e.Path) {
			return fmt.Errorf("invalid artifact path %q", e.Path)
		}
		if e.Size <= 0 || !validDigest(e.SHA256) {
			return fmt.Errorf("invalid artifact size or SHA-256 for %s/%s", t.OS, t.Arch)
		}
	}
	return nil
}

func openPackFile(dir, name string) (*os.File, error) {
	if !validPackPath(name) {
		return nil, fmt.Errorf("invalid artifact path %q", name)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open artifact directory: %w", err)
	}
	defer root.Close()
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if err != nil {
			return nil, fmt.Errorf("inspect artifact path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("artifact path must not contain symlinks")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("artifact file must be regular")
		}
	}
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	return f, nil
}

func supported(t Target) bool { return t.OS == "linux" && (t.Arch == "amd64" || t.Arch == "arm64") }
func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}
func validPackPath(s string) bool {
	return s != "." && fs.ValidPath(s) && !strings.ContainsAny(s, "\\:") && path.Clean(s) == s
}

func fileDigest(ctx context.Context, f *os.File, expectedSize int64) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("file must be regular")
	}
	if expectedSize > 0 && info.Size() != expectedSize {
		return "", fmt.Errorf("artifact size mismatch: got %d, want %d", info.Size(), expectedSize)
	}
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx, io.LimitReader(f, info.Size())})
	if err != nil {
		return "", err
	}
	if n != info.Size() {
		return "", fmt.Errorf("file size changed while hashing")
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
		return "", fmt.Errorf("file size changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type metadataReader struct {
	ctx       context.Context
	r         io.ReaderAt
	remaining int
}

func (r *metadataReader) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > r.remaining {
		return 0, fmt.Errorf("executable metadata exceeds read limit")
	}
	r.remaining -= len(p)
	return r.r.ReadAt(p, off)
}

func inspectExecutable(ctx context.Context, r io.ReaderAt, target Target) error {
	bounded := &metadataReader{ctx, r, maxMetadataRead}
	f, err := elf.NewFile(bounded)
	if err != nil {
		return fmt.Errorf("read ELF: %w", err)
	}
	defer f.Close()
	if err := validateELF(f, target); err != nil {
		return err
	}
	info, err := buildinfo.Read(bounded)
	if err != nil {
		return fmt.Errorf("read Go build metadata: %w", err)
	}
	return validateBuildInfo(info, target)
}

func validateELF(f *elf.File, target Target) error {
	machine := elf.EM_X86_64
	if target.Arch == "arm64" {
		machine = elf.EM_AARCH64
	}
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != machine {
		return fmt.Errorf("ELF architecture does not match %s", target.Arch)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("ELF is not an executable")
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("ELF requires an external interpreter")
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("read ELF dependencies: %w", err)
	}
	if len(libs) != 0 {
		return fmt.Errorf("ELF requires shared libraries")
	}
	return nil
}

func validateBuildInfo(info *debug.BuildInfo, target Target) error {
	if info.Path != runtimePackage {
		return fmt.Errorf("Go package mismatch: got %q, want %q", info.Path, runtimePackage)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		if _, exists := settings[s.Key]; exists {
			return fmt.Errorf("duplicate Go build setting %q", s.Key)
		}
		settings[s.Key] = s.Value
	}
	for key, want := range map[string]string{"GOOS": target.OS, "GOARCH": target.Arch, "CGO_ENABLED": "0"} {
		if settings[key] != want {
			return fmt.Errorf("Go build setting %s must be %q (got %q)", key, want, settings[key])
		}
	}
	return nil
}

func readManifest(name string) (*manifest, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("manifest must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestSize {
		return nil, fmt.Errorf("manifest exceeds %d bytes", maxManifestSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := checkJSONValue(decoder, 0); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing manifest data")
	}
	if err := exactFields(data, "schemaVersion", "protocolVersion", "controllerSha256", "artifacts"); err != nil {
		return nil, err
	}
	var raw struct {
		Artifacts []json.RawMessage `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for _, entry := range raw.Artifacts {
		if err := exactFields(entry, "os", "arch", "path", "size", "sha256"); err != nil {
			return nil, err
		}
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func exactFields(data []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range names {
		v, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("missing required field %q", name)
		}
		delete(fields, name)
	}
	for name := range fields {
		return fmt.Errorf("unknown field %q", name)
	}
	return nil
}

func checkJSONValue(d *json.Decoder, depth int) error {
	if depth > 16 {
		return fmt.Errorf("manifest nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := t.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			if seen[key] {
				return fmt.Errorf("duplicate field %q", key)
			}
			seen[key] = true
			if err := checkJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := checkJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

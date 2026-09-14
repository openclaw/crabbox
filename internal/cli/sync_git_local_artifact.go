package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Local seeds are complete histories, never depth-limited approximations. Zero
// maxBytes selects the hard bound; a positive value may only lower it. Both raw
// object bytes and the finished bundle must fit independently.
const (
	localGitSeedMaxBytes    = int64(512 << 20)
	localGitSeedMaxMetadata = 16 << 20
	localGitSeedMaxObjects  = 1_000_000
	localGitSeedTimeout     = 5 * time.Minute
)

type localGitSeedSelection struct{ Head, Tree, Base, BaseRef string }
type localGitSeedRef struct{ Name, OID string }
type localGitSeedArtifact struct {
	Path, Root, ObjectFormat, Digest string
	Refs                             []localGitSeedRef
	PackedBytes, ObjectBytes         int64
	ObjectCount                      int
	owner                            *gitOverlaySnapshot
}

func (a *localGitSeedArtifact) cleanup() error {
	if a.Root == "" {
		return nil
	}
	if a.owner == nil {
		return fmt.Errorf("missing local Git seed cleanup ownership for %q", a.Root)
	}
	if err := a.owner.cleanup(); err != nil {
		return err
	}
	*a = localGitSeedArtifact{}
	return nil
}

// boundedGitSeedWriter also bounds diagnostics without retaining repository
// content in errors. Returning an error terminates over-limit command output.
type boundedGitSeedWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *boundedGitSeedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("local Git seed byte limit exceeded")
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func localGitSeedCommand(ctx context.Context, root string, source bool, args ...string) *exec.Cmd {
	commandArgs := []string{"--no-optional-locks", "--no-replace-objects",
		"-c", "credential.helper=", "-c", "credential.interactive=never",
		"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never", "-c", "gc.auto=0"}
	cmd := exec.CommandContext(ctx, gitOverlayGitExecutable, append(commandArgs, args...)...)
	cmd.Dir = root
	cmd.Env = append(gitOverlayGitEnvironment(), "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_CONFIG_COUNT=0", "LC_ALL=C")
	if source {
		for _, entry := range repositoryGitEnvironment() {
			name, _, _ := strings.Cut(entry, "=")
			switch strings.ToUpper(name) {
			case "GIT_DIR", "GIT_WORK_TREE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM":
				cmd.Env = append(cmd.Env, entry)
			}
		}
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func localGitSeedOutput(ctx context.Context, root string, source bool, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, localGitSeedTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := localGitSeedCommand(ctx, root, source, args...)
	cmd.Stdout = &boundedGitSeedWriter{&out, localGitSeedMaxMetadata}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("local Git seed %s failed: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

func localGitSeedSourceOutput(ctx context.Context, root string, args ...string) (string, error) {
	return localGitSeedOutput(ctx, root, true, args...)
}

func localGitSeedInit(root, format string) error {
	if err := os.MkdirAll(filepath.Join(root, "objects", "info"), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "objects", "pack"), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "refs"), 0700); err != nil {
		return err
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	if format == "sha256" {
		config = "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectFormat = sha256\n"
	}
	if err := os.WriteFile(filepath.Join(root, "config"), []byte(config), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "HEAD"), []byte("ref: refs/heads/unborn\n"), 0600)
}

type localGitSeedObject struct {
	kind string
	size int64
}
type localGitSeedReader struct {
	cmd       *exec.Cmd
	in        io.WriteCloser
	out       *bufio.Reader
	format    string
	inspected int64
}

func newLocalGitSeedReader(ctx context.Context, root, format string) (*localGitSeedReader, error) {
	cmd := localGitSeedCommand(ctx, root, false, "cat-file", "--batch-command")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return nil, err
	}
	return &localGitSeedReader{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 65536), format: format}, nil
}
func (r *localGitSeedReader) close() {
	if r.cmd == nil {
		return
	}
	_ = r.in.Close()
	_ = r.cmd.Wait()
	r.cmd = nil
}
func (r *localGitSeedReader) request(command, oid string) (localGitSeedObject, error) {
	if _, err := io.WriteString(r.in, command+" "+oid+"\n"); err != nil {
		return localGitSeedObject{}, err
	}
	line, err := r.out.ReadSlice('\n')
	if err != nil {
		return localGitSeedObject{}, err
	}
	parts := strings.Fields(string(line))
	if len(parts) != 3 || parts[0] != oid {
		return localGitSeedObject{}, fmt.Errorf("missing or unreadable required local Git object %s", oid)
	}
	n, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || n < 0 {
		return localGitSeedObject{}, fmt.Errorf("invalid local Git object size")
	}
	return localGitSeedObject{parts[1], n}, nil
}
func (r *localGitSeedReader) contents(oid string, obj localGitSeedObject) ([]byte, error) {
	if obj.size > localGitSeedMaxMetadata || r.inspected > localGitSeedMaxBytes-obj.size {
		return nil, fmt.Errorf("local Git seed graph metadata limit exceeded")
	}
	r.inspected += obj.size
	actual, err := r.request("contents", oid)
	if err != nil {
		return nil, err
	}
	if actual != obj {
		return nil, fmt.Errorf("local Git object changed during preparation")
	}
	data := make([]byte, int(obj.size)+1)
	if _, err := io.ReadFull(r.out, data); err != nil {
		return nil, err
	}
	if data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("invalid local Git object framing")
	}
	return data[:len(data)-1], nil
}
func localGitSeedOID(oid, format string) bool {
	length := 40
	if format == "sha256" {
		length = 64
	}
	return len(oid) == length && oid == strings.ToLower(oid) && validGitObjectID(oid)
}

func localGitSeedTagTarget(data []byte, format string) (string, error) {
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	oid, ok := strings.CutPrefix(string(line), "object ")
	if !ok || !localGitSeedOID(oid, format) {
		return "", fmt.Errorf("invalid local Git tag target")
	}
	return oid, nil
}

func prepareLocalGitSeedArtifact(ctx context.Context, root string, selection localGitSeedSelection, maxBytes int64) (artifact localGitSeedArtifact, err error) {
	ctx, cancel := context.WithTimeout(ctx, localGitSeedTimeout)
	defer cancel()
	if maxBytes < 0 {
		return artifact, fmt.Errorf("local Git seed byte limit must not be negative")
	}
	if maxBytes == 0 || maxBytes > localGitSeedMaxBytes {
		maxBytes = localGitSeedMaxBytes
	}
	format, err := localGitSeedSourceOutput(ctx, root, "rev-parse", "--show-object-format")
	if err != nil {
		return artifact, err
	}
	if format != "sha1" && format != "sha256" {
		return artifact, fmt.Errorf("unsupported local Git object format")
	}
	for _, oid := range []string{selection.Head, selection.Tree} {
		if !localGitSeedOID(oid, format) {
			return artifact, fmt.Errorf("local Git seed requires exact HEAD and tree object IDs")
		}
	}
	if selection.Base != "" && !localGitSeedOID(selection.Base, format) {
		return artifact, fmt.Errorf("local Git seed requires an exact base object ID")
	}
	if selection.Base == "" && selection.BaseRef != "" {
		return artifact, fmt.Errorf("local Git base ref has no selected object")
	}
	refs := []localGitSeedRef{{localGitSeedHeadRef, selection.Head}}
	if selection.Base != "" {
		baseRef := selection.BaseRef
		if baseRef == "" {
			baseRef = "refs/crabbox/local-base"
		}
		if !strings.HasPrefix(baseRef, "refs/") || baseRef == refs[0].Name {
			return artifact, fmt.Errorf("invalid local Git base ref")
		}
		if _, err := localGitSeedSourceOutput(ctx, root, "check-ref-format", baseRef); err != nil {
			return artifact, fmt.Errorf("invalid local Git base ref")
		}
		refs = append(refs, localGitSeedRef{baseRef, selection.Base})
	}
	objects, err := localGitSeedSourceOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return artifact, err
	}
	if !filepath.IsAbs(objects) {
		return artifact, fmt.Errorf("local Git object directory must be absolute")
	}
	tags, err := localGitSeedSourceOutput(ctx, root, "for-each-ref", "--format=%(objectname) %(refname)", "refs/tags/")
	if err != nil {
		return artifact, err
	}
	owner, err := newGitOverlaySnapshot()
	if err != nil {
		return artifact, err
	}
	temp := owner.Root
	artifact.Root, artifact.owner = temp, &owner
	defer func() {
		if err != nil {
			if ctx.Err() != nil {
				err = errors.Join(err, ctx.Err())
			}
			if cleanupErr := artifact.cleanup(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("retained local Git seed at %q: %w", artifact.Root, cleanupErr))
			}
		}
	}()
	readerRoot := filepath.Join(temp, "reader")
	if err = localGitSeedInit(readerRoot, format); err != nil {
		return artifact, err
	}
	// The private reader has no shallow file, grafts, refs, or promisor config.
	// Alternates supply local objects only; their paths never enter the bundle.
	if err = os.WriteFile(filepath.Join(readerRoot, "objects", "info", "alternates"), []byte(strconv.Quote(filepath.ToSlash(objects))+"\n"), 0600); err != nil {
		return artifact, err
	}
	reader, err := newLocalGitSeedReader(ctx, readerRoot, format)
	if err != nil {
		return artifact, err
	}
	defer reader.close()
	selectedTree, err := localGitSeedOutput(ctx, readerRoot, false, "rev-parse", "--verify", selection.Head+"^{tree}")
	if err != nil {
		return artifact, err
	}
	if selectedTree != selection.Tree {
		return artifact, fmt.Errorf("local Git selected HEAD tree does not match")
	}
	walkArgs := []string{"rev-list", "--objects", "--no-object-names", selection.Head}
	if selection.Base != "" {
		walkArgs = append(walkArgs, selection.Base)
	}
	walkArgs = append(walkArgs, "--")
	closure, err := localGitSeedOutput(ctx, readerRoot, false, walkArgs...)
	if err != nil {
		return artifact, fmt.Errorf("enumerate complete required local Git objects: %w", err)
	}
	selected := make(map[string]localGitSeedObject)
	commits := make(map[string]bool)
	var objectBytes int64
	for _, oid := range strings.Fields(closure) {
		if !localGitSeedOID(oid, format) {
			return artifact, fmt.Errorf("invalid local Git closure object ID")
		}
		if _, exists := selected[oid]; exists {
			continue
		}
		if len(selected) >= localGitSeedMaxObjects {
			return artifact, fmt.Errorf("local Git seed object count limit exceeded")
		}
		obj, readErr := reader.request("info", oid)
		if readErr != nil {
			return artifact, readErr
		}
		if obj.size > maxBytes-objectBytes {
			return artifact, fmt.Errorf("local Git seed uncompressed object byte limit exceeded")
		}
		if obj.kind != "commit" && obj.kind != "tree" && obj.kind != "blob" {
			return artifact, fmt.Errorf("unexpected object type in local Git history")
		}
		selected[oid] = obj
		objectBytes += obj.size
		if obj.kind == "commit" {
			commits[oid] = true
		}
	}
	if !commits[selection.Head] || (selection.Base != "" && !commits[selection.Base]) {
		return artifact, fmt.Errorf("local Git seed HEAD/base must identify commits")
	}
	refIndexes := make(map[string]int, len(refs))
	for i, ref := range refs {
		refIndexes[ref.Name] = i
	}
	// Only tags recursively peeled to the selected ancestry union are advertised.
	// Unrelated tags do not widen the transported history.
	baseTagResolved := !strings.HasPrefix(selection.BaseRef, "refs/tags/")
	for _, line := range strings.Split(tags, "\n") {
		if line == "" {
			continue
		}
		oid, name, ok := strings.Cut(line, " ")
		if !ok || !localGitSeedOID(oid, format) || !strings.HasPrefix(name, "refs/tags/") {
			return artifact, fmt.Errorf("invalid local Git tag ref")
		}
		chain := make(map[string]localGitSeedObject)
		peeled := oid
		for !commits[peeled] {
			if len(chain) >= localGitSeedMaxObjects || len(chain)+len(selected) >= localGitSeedMaxObjects {
				return artifact, fmt.Errorf("local Git seed tag chain limit exceeded")
			}
			if _, seen := chain[peeled]; seen {
				return artifact, fmt.Errorf("invalid local Git tag chain")
			}
			obj, readErr := reader.request("info", peeled)
			if readErr != nil {
				return artifact, readErr
			}
			if obj.kind != "tag" {
				break
			}
			data, readErr := reader.contents(peeled, obj)
			if readErr != nil {
				return artifact, readErr
			}
			target, readErr := localGitSeedTagTarget(data, format)
			if readErr != nil {
				return artifact, readErr
			}
			chain[peeled] = obj
			peeled = target
		}
		if !commits[peeled] {
			continue
		}
		for tagOID, obj := range chain {
			if _, exists := selected[tagOID]; exists {
				continue
			}
			if obj.size > maxBytes-objectBytes {
				return artifact, fmt.Errorf("local Git seed uncompressed object byte limit exceeded")
			}
			selected[tagOID] = obj
			objectBytes += obj.size
		}
		if i, exists := refIndexes[name]; exists {
			// A tag-valued base pins its peeled commit while retaining the exact
			// annotated tag ref, including nested annotation objects.
			if name != selection.BaseRef || peeled != selection.Base {
				return artifact, fmt.Errorf("local Git base and tag refs conflict")
			}
			refs[i].OID = oid
			baseTagResolved = true
		} else {
			refIndexes[name] = len(refs)
			refs = append(refs, localGitSeedRef{name, oid})
		}
	}
	if !baseTagResolved {
		return artifact, fmt.Errorf("local Git base tag no longer identifies the selected commit")
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	oids := make([]string, 0, len(selected))
	for oid := range selected {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	path := filepath.Join(temp, "seed.bundle")
	bundle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return artifact, err
	}
	defer bundle.Close()
	writer := &boundedGitSeedWriter{bundle, maxBytes}
	var header strings.Builder
	fmt.Fprintf(&header, "# v3 git bundle\n@object-format=%s\n", format)
	for _, ref := range refs {
		fmt.Fprintf(&header, "%s %s\n", ref.OID, ref.Name)
	}
	header.WriteByte('\n')
	if _, err = io.WriteString(writer, header.String()); err != nil {
		return artifact, err
	}
	packOffset := int64(header.Len())
	pack := localGitSeedCommand(ctx, readerRoot, false, "pack-objects", "--stdout", "--no-reuse-delta", "--no-reuse-object", "--window=0", "--threads=1")
	pack.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	pack.Stdout = writer
	if err = pack.Run(); err != nil {
		return artifact, fmt.Errorf("pack complete local Git objects: %w", err)
	}
	stat, err := bundle.Stat()
	if err != nil {
		return artifact, err
	}
	verifyRoot := filepath.Join(temp, "verify")
	if err = localGitSeedInit(verifyRoot, format); err != nil {
		return artifact, err
	}
	if _, err = bundle.Seek(packOffset, io.SeekStart); err != nil {
		return artifact, err
	}
	verify := localGitSeedCommand(ctx, verifyRoot, false, "index-pack", "--strict", "--stdin")
	verify.Stdin = bundle
	if err = verify.Run(); err != nil {
		return artifact, fmt.Errorf("verify standalone local Git object closure: %w", err)
	}
	verifiedReader, err := newLocalGitSeedReader(ctx, verifyRoot, format)
	if err != nil {
		return artifact, err
	}
	defer verifiedReader.close()
	for _, ref := range refs {
		if _, err = verifiedReader.request("info", ref.OID); err != nil {
			return artifact, fmt.Errorf("verify selected local Git ref: %w", err)
		}
	}
	verifiedReader.close()
	if _, err = bundle.Seek(0, io.SeekStart); err != nil {
		return artifact, err
	}
	digest := sha256.New()
	if _, err = io.Copy(digest, bundle); err != nil {
		return artifact, err
	}
	if err = bundle.Close(); err != nil {
		return artifact, err
	}
	// Reader processes must release their files before private storage is removed,
	// including on native Windows. Closing twice is harmless.
	reader.close()
	if err = owner.cleanupRoot.root.RemoveAll("reader"); err != nil {
		return artifact, err
	}
	if err = owner.cleanupRoot.root.RemoveAll("verify"); err != nil {
		return artifact, err
	}
	return localGitSeedArtifact{Path: path, Root: temp, ObjectFormat: format, Digest: hex.EncodeToString(digest.Sum(nil)), Refs: refs, PackedBytes: stat.Size(), ObjectBytes: objectBytes, ObjectCount: len(selected), owner: &owner}, nil
}

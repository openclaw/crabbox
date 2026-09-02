package blacksmith

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const blacksmithCollectionTimeout = 30 * time.Second

// This receipt correlates one native invocation; it is not remote attestation.
type blacksmithArtifactReceipt struct {
	nonce       string
	stage       int // 0: startup, 1: workload, 2: collection, 3: complete
	code        int
	collectCode int
	exitedAt    time.Time
	payload     bytes.Buffer
	onExit      func()
	now         func() time.Time
	output      io.Writer
}

func newBlacksmithArtifactReceipt(now func() time.Time) (*blacksmithArtifactReceipt, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return &blacksmithArtifactReceipt{nonce: hex.EncodeToString(nonce[:]), now: now}, nil
}

func (r *blacksmithArtifactReceipt) command(req RunRequest, budget time.Duration) string {
	// Keep control bytes escaped in source: native --debug may echo this string.
	format := `\036CRABBOX_BS_` + r.nonce + ":"
	child := "printf '" + format + "start\\037' || exit 7\n" + blacksmithCommandString(req.Command, req.ShellMode)
	globs := append(append([]string{}, req.ArtifactGlobs...), req.RequiredArtifactGlobs...)
	collector := core.DelegatedRunArtifactScript(req.RequiredArtifactGlobs, globs, core.DelegatedRunArtifactDefaultMaxFiles, core.DelegatedRunArtifactDefaultMaxBytes)
	for _, marker := range []string{core.DelegatedRunArtifactBeginMarker, core.DelegatedRunArtifactEndMarker} {
		collector = strings.ReplaceAll(collector, marker, `\137`+marker[1:])
	}
	// Neither child runs in a conditional: bash errexit, exit, exec, and traps
	// remain child-owned. The supervisor never changes its initial remote cwd.
	body := "command -v timeout >/dev/null 2>&1 || exit 7\n" +
		"bash -c " + shellQuote(child) + "\n" +
		"workload_code=$?\n" +
		"printf '" + format + "exit:%d\\037' \"$workload_code\" || exit 7\n" +
		"collection_code=0\n" +
		"if [ \"$workload_code\" -lt 128 ]; then\n" +
		"timeout --kill-after=1s " + shellQuote(fmt.Sprintf("%gs", budget.Seconds())) + " bash -c " + shellQuote(collector) + " 2>&1\n" +
		"collection_code=$?\nfi\n" +
		"printf '" + format + "end:%d\\037' \"$collection_code\" || exit 7\nexit 0"
	return "/bin/sh -c " + shellQuote(body)
}

func (r *blacksmithArtifactReceipt) record(record string) error {
	value, ok := strings.CutPrefix(record, "CRABBOX_BS_"+r.nonce+":")
	if !ok {
		return errors.New("unexpected artifact receipt")
	}
	if value == "start" && r.stage == 0 {
		r.stage = 1
		return nil
	}
	kind, number, _ := strings.Cut(value, ":")
	code, err := strconv.Atoi(number)
	if err != nil || code < 0 || code > 255 || strconv.Itoa(code) != number {
		return errors.New("malformed artifact receipt")
	}
	switch {
	case kind == "exit" && r.stage == 1:
		r.stage, r.code, r.exitedAt = 2, code, r.now()
		if r.onExit != nil {
			r.onExit()
		}
	case kind == "end" && r.stage == 2:
		r.stage, r.collectCode = 3, code
	default:
		return errors.New("unordered or duplicate artifact receipt")
	}
	return nil
}

func (r *blacksmithArtifactReceipt) data(data []byte) error {
	if r.stage == 3 {
		return nil
	}
	if r.stage != 2 {
		_, err := r.output.Write(data)
		return err
	}
	limit := blacksmithArtifactOutputCaptureLimit(core.DelegatedRunArtifactDefaultMaxBytes)
	if int64(r.payload.Len()+len(data)) > limit {
		return errors.New("artifact output too large")
	}
	r.payload.Write(data)
	begin := bytes.Index(r.payload.Bytes(), []byte(core.DelegatedRunArtifactBeginMarker+"\n"))
	diagnosticBytes := r.payload.Len()
	if begin >= 0 {
		diagnosticBytes = begin
		if end := bytes.Index(r.payload.Bytes(), []byte(core.DelegatedRunArtifactEndMarker+"\n")); end >= 0 {
			diagnosticBytes += r.payload.Len() - end - len(core.DelegatedRunArtifactEndMarker+"\n")
		}
	}
	if int64(diagnosticBytes) > blacksmithArtifactDiagnosticCaptureBytes {
		return errors.New("artifact diagnostics too large")
	}
	return nil
}

// Strip all control frames, including stale/malformed ones. Once invalid, drop
// the rest of this stream, so truncated or overflowing archives cannot leak.
type blacksmithControlDemux struct {
	pending []byte
	inFrame bool
	err     error
	data    func([]byte) error
	record  func(string) error
	cancel  context.CancelCauseFunc
}

func (d *blacksmithControlDemux) fail(err error) {
	if d.err == nil {
		d.err = err
		d.pending = nil
		d.cancel(err)
	}
}

func (d *blacksmithControlDemux) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 && d.err == nil {
		if !d.inFrame {
			i := bytes.IndexByte(data, '\x1e')
			if i < 0 {
				i = len(data)
			}
			if err := d.data(data[:i]); err != nil {
				d.fail(err)
				break
			}
			data = data[i:]
			if len(data) == 0 {
				break
			}
			d.inFrame = true
			data = data[1:]
		}
		i := bytes.IndexByte(data, '\x1f')
		if i < 0 {
			i = len(data)
		}
		if len(d.pending)+i > 128 {
			d.fail(errors.New("artifact receipt too large"))
			break
		}
		d.pending = append(d.pending, data[:i]...)
		data = data[i:]
		if len(data) > 0 {
			err := d.record(string(d.pending))
			d.inFrame, d.pending = false, nil
			data = data[1:]
			if err != nil {
				d.fail(err)
			}
		}
	}
	return n, nil
}

func (d *blacksmithControlDemux) finish() {
	if d.inFrame {
		d.fail(errors.New("truncated artifact receipt"))
	}
}

func (r *blacksmithArtifactReceipt) archive() ([]byte, string, error) {
	output := r.payload.String()
	// Never include unvalidated payload in errors, even if collection failed
	// after writing only an archive prefix or base64 fragment.
	if r.collectCode != 0 {
		if r.collectCode == 8 {
			return nil, "", exit(7, "collection exited 8: missing required artifact")
		}
		return nil, "", exit(7, "collection exited %d", r.collectCode)
	}
	if strings.Count(output, core.DelegatedRunArtifactBeginMarker+"\n") != 1 || strings.Count(output, core.DelegatedRunArtifactEndMarker+"\n") != 1 {
		return nil, "", exit(7, "collection returned an invalid archive envelope")
	}
	archive, diagnostic, err := blacksmithExtractArtifactArchive(output, core.DelegatedRunArtifactDefaultMaxBytes)
	if err != nil {
		return nil, "", err
	}
	if int64(len(diagnostic)) > blacksmithArtifactDiagnosticCaptureBytes {
		return nil, "", exit(7, "artifact diagnostics too large")
	}
	return archive, "", nil
}

// A missing exit record must not turn the collector's archive into console
// output. Reserve the archive envelope even outside the collection phase.
type blacksmithArchiveGuard struct {
	output  io.Writer
	pending []byte
}

func (g *blacksmithArchiveGuard) Write(data []byte) (int, error) {
	const marker = "__CRABBOX_ARTIFACT_"
	g.pending = append(g.pending, data...)
	if i := bytes.Index(g.pending, []byte(marker)); i >= 0 {
		_, err := g.output.Write(g.pending[:i])
		g.pending = nil
		return len(data), errors.Join(err, errors.New("archive outside collection receipt"))
	}
	keep := 0
	for n := 1; n < len(marker) && n <= len(g.pending); n++ {
		if bytes.Equal(g.pending[len(g.pending)-n:], []byte(marker[:n])) {
			keep = n
		}
	}
	_, err := g.output.Write(g.pending[:len(g.pending)-keep])
	g.pending = bytes.Clone(g.pending[len(g.pending)-keep:])
	return len(data), err
}

func (g *blacksmithArchiveGuard) flush() {
	_, _ = g.output.Write(g.pending)
	g.pending = nil
}

// Called only inside the original shared claim fence. There is no follow-up
// native run, route resolution, sync, or stopped-lease recovery.
func (b *blacksmithBackend) runArtifactTestbox(ctx context.Context, req RunRequest, leaseID string, phases *core.CommandPhaseTracker, stdoutExtra, stderrExtra io.Writer, budget time.Duration) (code int, ended time.Time, collected core.DelegatedRunArtifactResult, artifactErr error) {
	r, err := newBlacksmithArtifactReceipt(b.rt.Clock.Now)
	if err != nil {
		return 2, time.Time{}, collected, err
	}
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return 2, time.Time{}, collected, err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var timer *time.Timer
	r.onExit = func() {
		timer = time.AfterFunc(budget, func() { cancel(errors.New("artifact collection timed out")) })
	}
	stdout, stdoutPhase := commandPhaseWriter(mergeWriters(b.rt.Stdout, stdoutExtra), phases)
	stderr, stderrPhase := commandPhaseWriter(mergeWriters(b.rt.Stderr, stderrExtra), phases)
	var out, diagnostic *blacksmithControlDemux
	var outGuard, errGuard *blacksmithArchiveGuard
	filter := func(stdout, stderr io.Writer) (io.Writer, io.Writer) {
		outGuard, errGuard = &blacksmithArchiveGuard{output: stdout}, &blacksmithArchiveGuard{output: stderr}
		r.output = outGuard
		out = &blacksmithControlDemux{data: r.data, record: r.record, cancel: cancel}
		diagnostic = &blacksmithControlDemux{
			data:   func(p []byte) error { _, err := errGuard.Write(p); return err },
			record: func(string) error { return errors.New("artifact receipt on wrong stream") }, cancel: cancel,
		}
		return out, diagnostic
	}
	args := blacksmithRunArgs(b.cfg, leaseID, keyPath, []string{r.command(req, budget)}, req.DebugSync || b.cfg.Blacksmith.Debug, true)
	native, syncTimeout, nativeErr := b.runCommandWithSyncGuardFiltered(runCtx, args, stdout, stderr, true, req.Repo.Root, filter)
	if timer != nil {
		defer timer.Stop()
	}
	out.finish()
	diagnostic.finish()
	outGuard.flush()
	errGuard.flush()
	stdoutPhase.Flush()
	stderrPhase.Flush()
	ended = r.exitedAt
	code = native.ExitCode
	if syncTimeout {
		code = 124
	}
	if nativeErr != nil || code != 0 || runCtx.Err() != nil || r.stage != 3 || out.err != nil || diagnostic.err != nil {
		if code == 0 {
			code = 7
		}
		// An observed nonzero child status remains primary even if subsequent
		// collection or transport fails. It never authorizes publication alone.
		if r.stage >= 2 && r.code != 0 {
			code = r.code
		}
		return code, ended, collected, exit(7, "native run did not complete a clean artifact protocol; artifacts withheld")
	}
	code = r.code
	if code >= 128 {
		return code, ended, collected, nil
	}
	archive, output, err := r.archive()
	if err != nil {
		return code, ended, collected, err
	}
	path := core.LocalRunArtifactPath(req.Repo.Root, "", leaseID, "blacksmith-artifacts.tgz")
	if err := writeBlacksmithRunArchive(runCtx, path, archive); err != nil {
		return code, ended, collected, exit(2, "blacksmith artifact write: %v", err)
	}
	collected.Output = output
	collected.Artifacts = []core.RunArtifact{{Kind: "artifact-glob", Path: path, Bytes: len(archive)}}
	return code, ended, collected, nil
}

func writeBlacksmithRunArchive(ctx context.Context, path string, archive []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".blacksmith-artifacts-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(archive)
	if err := errors.Join(writeErr, file.Close(), ctx.Err()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

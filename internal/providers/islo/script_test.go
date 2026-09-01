package islo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestIsloScriptArchiveCarriesScriptVerbatim(t *testing.T) {
	data := []byte("node --version\nnpm --version\n")
	spec := &core.RunScriptSpec{Data: data, RemotePath: ".crabbox/scripts/abc123-script.sh"}
	archive, err := isloScriptArchive(spec)
	if err != nil {
		t.Fatalf("isloScriptArchive: %v", err)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	if header.Name != ".crabbox/scripts/abc123-script.sh" {
		t.Fatalf("name=%q", header.Name)
	}
	if header.Mode != 0o700 {
		t.Fatalf("mode=%o want 700", header.Mode)
	}
	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("script body=%q want %q", got, data)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("expected a single entry, got err=%v", err)
	}
}

func TestIsloScriptArchiveIsDeterministic(t *testing.T) {
	spec := &core.RunScriptSpec{Data: []byte("echo hi\n"), RemotePath: ".crabbox/scripts/a-script.sh"}
	first, err := isloScriptArchive(spec)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := isloScriptArchive(spec)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	a, _ := io.ReadAll(first)
	b, _ := io.ReadAll(second)
	if string(a) != string(b) {
		t.Fatal("packing the same script twice produced different bytes")
	}
}

func TestIsloScriptArchiveRejectsEscapingPaths(t *testing.T) {
	for _, remote := range []string{"/etc/passwd", "../outside.sh", "", "   "} {
		spec := &core.RunScriptSpec{Data: []byte("echo hi\n"), RemotePath: remote}
		if _, err := isloScriptArchive(spec); err == nil {
			t.Fatalf("remote=%q was accepted; want rejection", remote)
		}
	}
}

func TestIsloScriptArchiveRejectsEmptyScript(t *testing.T) {
	if _, err := isloScriptArchive(&core.RunScriptSpec{RemotePath: "a.sh"}); err == nil {
		t.Fatal("empty script was accepted")
	}
	if _, err := isloScriptArchive(nil); err == nil {
		t.Fatal("nil spec was accepted")
	}
}

func TestIsloScriptCommandPassesPathAsArgument(t *testing.T) {
	spec := &core.RunScriptSpec{RemotePath: ".crabbox/scripts/a b;rm -rf.sh"}
	got := isloScriptCommand(spec, nil)
	want := []string{"bash", "-lc", `exec bash "$@"`, "bash", ".crabbox/scripts/a b;rm -rf.sh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%q want %q", got, want)
	}
	// The hostile path must never appear inside the -c program text, which is
	// what would let the shell reinterpret it.
	if strings.Contains(got[2], "rm -rf") {
		t.Fatalf("script path leaked into the shell program: %q", got[2])
	}
}

func TestIsloScriptCommandHonoursShebang(t *testing.T) {
	spec := &core.RunScriptSpec{RemotePath: "s.sh", Shebang: true}
	got := isloScriptCommand(spec, nil)
	if got[2] != `exec "$@"` {
		t.Fatalf("shebang script should exec directly, got %q", got[2])
	}
}

func TestIsloScriptCommandAppendsArgs(t *testing.T) {
	spec := &core.RunScriptSpec{RemotePath: "s.sh"}
	got := isloScriptCommand(spec, []string{"--flag", "value"})
	if !reflect.DeepEqual(got[4:], []string{"s.sh", "--flag", "value"}) {
		t.Fatalf("args not forwarded: %q", got)
	}
}

const isloScriptRemoveCommand = `rm -f -- "$1"`

func isloScriptTestBackend(stderr io.Writer) *isloBackend {
	return &isloBackend{rt: Runtime{Stdout: io.Discard, Stderr: stderr}}
}

func isloScriptTestRequest(remote string) RunRequest {
	return RunRequest{Script: &core.RunScriptSpec{Data: []byte("echo hi\n"), RemotePath: remote}}
}

// A cancelled or timed-out run must still remove the script it uploaded: the
// sandbox may be kept or reused, so a script left behind is script content
// persisting past the run. Cleanup therefore has to reach the provider on a
// live context even though the context the run was given is already dead.
func TestIsloRunScriptRemovesScriptAfterRunContextCancelled(t *testing.T) {
	withIsloCleanupTimeout(t, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeIsloSyncClient{
		// Any exec handed a dead context fails, exactly as the real client
		// does once it binds that context to its HTTP request.
		rejectCanceledContext: true,
		// The workload dies mid-flight and cancellation is what ends it.
		execErrOnCommand:         context.Canceled,
		execErrOnCommandContains: `exec bash "$@"`,
		execErrOnCommandHook:     cancel,
	}
	var stderr bytes.Buffer
	backend := isloScriptTestBackend(&stderr)

	code, err := backend.runScript(ctx, client, "crabbox-test", "/workspace/repo", isloScriptTestRequest(".crabbox/scripts/abc-script.sh"), nil, "runner")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runScript err=%v, want context.Canceled", err)
	}
	if code != 1 {
		t.Fatalf("exit code=%d, want the code the workload exec reported (1)", code)
	}
	if !client.commandContains(isloScriptRemoveCommand) {
		t.Fatalf("cleanup exec was never issued after cancellation; commands=%q", client.prepareCommands)
	}
	state, ok := client.execCtxStateContaining(isloScriptRemoveCommand)
	if !ok {
		t.Fatal("cleanup exec context was not recorded")
	}
	if state.err != nil {
		t.Fatalf("cleanup ran on an already-dead context: %v", state.err)
	}
	if !state.hasDeadline {
		t.Fatal("cleanup context has no deadline; best-effort cleanup must stay bounded")
	}
	if remaining := time.Until(state.deadline); remaining <= 0 {
		t.Fatalf("cleanup context deadline already passed (%s remaining)", remaining)
	}
}

// A failed upload can still have landed the file (the failure may be in the
// response, not the extraction), so cleanup is attempted; and the upload error
// still reaches the caller unchanged.
func TestIsloRunScriptRemovesScriptAfterFailedUpload(t *testing.T) {
	uploadErr := errors.New("islo upload boom")
	client := &fakeIsloSyncClient{uploadErr: uploadErr}
	var stderr bytes.Buffer
	backend := isloScriptTestBackend(&stderr)

	code, err := backend.runScript(context.Background(), client, "crabbox-test", "/workspace/repo", isloScriptTestRequest(".crabbox/scripts/abc-script.sh"), nil, "")

	if !errors.Is(err, uploadErr) {
		t.Fatalf("runScript err=%v, want the upload error", err)
	}
	if code != 7 {
		t.Fatalf("exit code=%d, want 7", code)
	}
	if !client.commandContains(isloScriptRemoveCommand) {
		t.Fatalf("cleanup exec was never issued after a failed upload; commands=%q", client.prepareCommands)
	}
	if client.commandContains(`exec bash "$@"`) {
		t.Fatal("the script was executed even though its upload failed")
	}
}

// A path that never passed validation must never be handed to `rm` in the
// sandbox, and no upload was issued for it either, so there is nothing to
// clean up.
func TestIsloRunScriptSkipsCleanupForRejectedPath(t *testing.T) {
	client := &fakeIsloSyncClient{}
	var stderr bytes.Buffer
	backend := isloScriptTestBackend(&stderr)

	code, err := backend.runScript(context.Background(), client, "crabbox-test", "/workspace/repo", isloScriptTestRequest("/etc/cron.d/payload"), nil, "")

	if err == nil {
		t.Fatal("an absolute script path was accepted")
	}
	if code != 7 {
		t.Fatalf("exit code=%d, want 7", code)
	}
	if len(client.prepareCommands) != 0 {
		t.Fatalf("a rejected path reached the sandbox: %q", client.prepareCommands)
	}
}

// removeRunScript is the only place that builds an `rm` argv for the sandbox,
// so it validates the path itself rather than trusting its caller.
func TestIsloRemoveRunScriptRefusesUnvalidatedPath(t *testing.T) {
	client := &fakeIsloSyncClient{}
	var stderr bytes.Buffer
	backend := isloScriptTestBackend(&stderr)

	backend.removeRunScript(client, "crabbox-test", "/workspace/repo", &core.RunScriptSpec{RemotePath: "/etc/cron.d/payload"}, "")

	if len(client.prepareCommands) != 0 {
		t.Fatalf("an unvalidated path was handed to the sandbox: %q", client.prepareCommands)
	}
	if !strings.Contains(stderr.String(), "skipped run script cleanup") {
		t.Fatalf("stderr=%q, want a skipped-cleanup warning", stderr.String())
	}
}

package islo

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"reflect"
	"strings"
	"testing"

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

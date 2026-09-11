package islo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

// This runs the adapter's archive, chown, shell and cleanup requests as real
// local processes using the current test account. It does not contact Islo or
// test user switching or Tailnet enrollment; request tests pin the API users.
func TestIsloRunScriptLocalExecution(t *testing.T) {
	workload, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroup(workload.Username)
	if err != nil || group.Gid != workload.Gid {
		t.Skip("requires a primary group named after the current account")
	}
	workloadName := workload.Username
	if workloadName == isloAdminUser {
		// Exercise ownership repair under root too: numeric IDs name the same
		// account but do not trigger the administrative-user fast path.
		workloadName = workload.Uid
	}
	for _, tool := range []string{"bash", "tar", "chown", "stat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("requires %s", tool)
		}
	}

	workspace := filepath.Join(t.TempDir(), "workspace with 'quotes';literal")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, shebang := range []bool{false, true} {
		t.Run(fmt.Sprintf("shebang=%t", shebang), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			backend := &isloBackend{rt: Runtime{Stdout: &stdout, Stderr: &stderr}}
			body := "printf 'uid=%s\\nmode=%s\\narg=%s\\nenv=%s\\n' \"$(id -u)\" \"$(stat -c %a \"$0\")\" \"$1\" \"$SCRIPT_TEST_VALUE\"\nprintf 'stderr-line\\n' >&2\nexit 23\n"
			if shebang {
				body = "#!/bin/sh\n" + body
			}
			spec := &core.RunScriptSpec{
				Data: []byte(body), RemotePath: ".crabbox/scripts/a b'c;literal.sh", Shebang: shebang,
			}
			client := &localIsloScriptClient{t: t, workspace: workspace}
			code, err := backend.runScript(t.Context(), client, "local-test", workspace,
				RunRequest{Script: spec, Command: []string{"literal; $argument"}},
				map[string]string{"SCRIPT_TEST_VALUE": "multi\nline"}, workloadName)
			if err != nil || code != 23 {
				t.Fatalf("runScript=(%d, %v); stderr=%q", code, err, stderr.String())
			}
			want := "uid=" + workload.Uid + "\nmode=700\narg=literal; $argument\nenv=multi\nline\n"
			if stdout.String() != want || stderr.String() != "stderr-line\n" {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if _, err := os.Lstat(filepath.Join(workspace, spec.RemotePath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("script remains after cleanup: %v", err)
			}
			t.Logf("local uid=%s; mode=700; literal argv and multiline env preserved; stdout/stderr separate; exit=23; script removed", workload.Uid)
		})
	}
}

type localIsloScriptClient struct {
	isloAPI
	t         *testing.T
	workspace string
}

func (c *localIsloScriptClient) UploadArchive(ctx context.Context, _ string, workspace string, archive io.Reader) error {
	if workspace != c.workspace {
		return fmt.Errorf("unexpected archive workspace %q", workspace)
	}
	cmd := exec.CommandContext(ctx, "tar", "-xzf", "-", "-C", workspace)
	cmd.Stdin = archive
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("extract: %w: %s", err, output)
	}
	return nil
}

func (c *localIsloScriptClient) ExecStream(ctx context.Context, _ string, req *gosdk.ExecRequest, stdout, stderr io.Writer) (int, error) {
	c.t.Helper()
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = "/" // Deliberately differs from the configured workspace.
	if req.Workdir != nil {
		cmd.Dir = *req.Workdir
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/tmp"}
	for key, value := range req.Env {
		if value != nil {
			cmd.Env = append(cmd.Env, key+"="+*value)
		}
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	c.t.Logf("local exec requested-user=%s workspace=%q command=%s", *req.User, cmd.Dir, req.Command[0])
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

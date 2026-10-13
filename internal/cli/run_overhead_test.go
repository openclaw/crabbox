//go:build !windows

package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoSyncScriptBatchesPreparation(t *testing.T) {
	p, backend, dir := setupSSHScriptRun(t)
	backend.lease.SSH.AuthSecret = false
	backend.lease.SSH.User = "fixture"
	const source = "printf 'batched-output\\n'\n"
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(source)}
	err := app.runCommand(t.Context(), []string{"--provider", p.Spec().Name, "--no-sync", "--keep", "--script-stdin"})
	if ExitCodeForError(err, 0) != 0 || stdout.String() != "batched-output\n" {
		t.Fatalf("exit/output: %v %q\n%s", err, stdout.String(), stderr.String())
	}
	log, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
	if err != nil {
		t.Fatal(err)
	}
	prepared := 0
	for _, command := range strings.Split(string(log), "\n") {
		for {
			_, payload, found := strings.Cut(command, `payload_b64="`)
			if !found {
				break
			}
			payload, _, _ = strings.Cut(payload, `"`)
			decoded, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				t.Fatal(err)
			}
			command = string(decoded)
		}
		if strings.Contains(command, "sync-fingerprint") {
			prepared++
			if !strings.Contains(command, "mkdir -p") || !strings.Contains(command, "cat >") || !strings.Contains(command, ".crabbox/scripts/") {
				t.Fatal("workspace preparation did not share the upload session")
			}
		}
	}
	if prepared != 1 {
		t.Fatalf("preparation sessions=%d, want one", prepared)
	}
	uploads, _ := filepath.Glob(filepath.Join(dir, "remote workspace", "*", "*", ".crabbox", "scripts", "*"))
	if len(uploads) != 1 {
		t.Fatalf("uploads=%v", uploads)
	}
	data, err := os.ReadFile(uploads[0])
	if err != nil || string(data) != source {
		t.Fatalf("retained script: %q %v", data, err)
	}
}

func TestBatchedUploadKeepsPreparationStdinEmpty(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "stdin reader", true: "preparation failure"}[fail], func(t *testing.T) {
			_, backend, dir := setupSSHScriptRun(t)
			backend.lease.SSH.AuthSecret = false
			backend.lease.SSH.User = "fixture"
			workdir := filepath.Join(dir, "upload ' workspace")
			spec, err := loadRunScript("", true, strings.NewReader("echo retained-bytes\n"))
			if err != nil {
				t.Fatal(err)
			}
			prepare := "cat >/dev/null; " + remoteMkdir(workdir)
			if fail {
				prepare += " && false"
			}
			err = uploadRunScriptAfter(t.Context(), backend.lease.SSH, workdir, spec, prepare)
			path := filepath.Join(workdir, filepath.FromSlash(spec.RemotePath))
			if fail {
				if err == nil {
					t.Fatal("failed preparation admitted upload")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("upload after failure: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, spec.Data) {
				t.Fatalf("script stdin was consumed: %q %v", data, err)
			}
		})
	}
}

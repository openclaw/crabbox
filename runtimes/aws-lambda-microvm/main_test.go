package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadAndExec(t *testing.T) {
	s := &server{execSlot: make(chan struct{}, 1)}
	archive := "/tmp/crabbox-sync-a1b2c3.tgz"
	t.Cleanup(func() { _ = os.Remove(archive) })
	uploadReq := httptest.NewRequest(http.MethodPut, "/v1/files?path="+archive, strings.NewReader("archive-data"))
	uploadRes := httptest.NewRecorder()
	s.upload(uploadRes, uploadReq)
	if uploadRes.Code != http.StatusNoContent {
		t.Fatalf("upload status=%d body=%s", uploadRes.Code, uploadRes.Body.String())
	}
	data, err := os.ReadFile(archive)
	if err != nil || string(data) != "archive-data" {
		t.Fatalf("uploaded data=%q err=%v", data, err)
	}

	workdir := t.TempDir()
	payload, _ := json.Marshal(execRequest{Command: `printf "$RUNNER_TEST"; printf stderr-proof >&2; exit 7`, Workdir: workdir, Env: map[string]string{"RUNNER_TEST": "stdout-proof"}})
	execReq := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(payload))
	execRes := httptest.NewRecorder()
	s.exec(execRes, execReq)
	if execRes.Code != http.StatusOK {
		t.Fatalf("exec status=%d body=%s", execRes.Code, execRes.Body.String())
	}
	var stdout, stderr string
	exitCode := -1
	scanner := bufio.NewScanner(execRes.Body)
	for scanner.Scan() {
		var event streamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Stream == "stdout" {
			stdout += event.Data
		}
		if event.Stream == "stderr" {
			stderr += event.Data
		}
		if event.ExitCode != nil {
			exitCode = *event.ExitCode
		}
	}
	if stdout != "stdout-proof" || stderr != "stderr-proof" || exitCode != 7 {
		t.Fatalf("stdout=%q stderr=%q exit=%d", stdout, stderr, exitCode)
	}
	if _, err := os.Stat(filepath.Join(workdir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("unexpected command side effect: %v", err)
	}
}

func TestUploadRejectsPathsOutsideDedicatedTempNames(t *testing.T) {
	for _, value := range []string{"/etc/passwd", "/tmp/not-crabbox.tgz", "/tmp/crabbox-sync-../x.tgz"} {
		if _, err := uploadPath(value); err == nil {
			t.Fatalf("uploadPath(%q) unexpectedly succeeded", value)
		}
	}
}

func TestExecKeepsScriptSeparateFromCommandStdin(t *testing.T) {
	s := &server{execSlot: make(chan struct{}, 1)}
	payload, _ := json.Marshal(execRequest{
		Command: "read line || true\nprintf after",
		Workdir: t.TempDir(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(payload))
	res := httptest.NewRecorder()
	s.exec(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	var stdout string
	scanner := bufio.NewScanner(res.Body)
	for scanner.Scan() {
		var event streamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Stream == "stdout" {
			stdout += event.Data
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if stdout != "after" {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestUploadReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	s := &server{execSlot: make(chan struct{}, 1)}
	target := "/tmp/crabbox-sync-deadbeef.tgz"
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(target)
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(target) })
	req := httptest.NewRequest(http.MethodPut, "/v1/files?path="+target, strings.NewReader("upload"))
	res := httptest.NewRecorder()
	s.upload(res, req)
	if res.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "safe" {
		t.Fatalf("victim=%q err=%v", data, err)
	}
	data, err = os.ReadFile(target)
	if err != nil || string(data) != "upload" {
		t.Fatalf("target=%q err=%v", data, err)
	}
}

func TestValidateExecRequest(t *testing.T) {
	if err := validateExecRequest(execRequest{Command: "true", Workdir: "/workspace", Env: map[string]string{"OK_NAME": "value"}}); err != nil {
		t.Fatal(err)
	}
	if err := validateExecRequest(execRequest{Command: "true", Workdir: "relative"}); err == nil {
		t.Fatal("relative workdir accepted")
	}
	if err := validateExecRequest(execRequest{Command: "true", Workdir: "/workspace", Env: map[string]string{"BAD-NAME": "value"}}); err == nil {
		t.Fatal("invalid environment name accepted")
	}
}

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/islo"
)

type runRequest struct {
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
}

type runResponse struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

var runMu sync.Mutex

func main() {}

// CrabboxMobileRun runs the real Crabbox Go CLI entrypoint inside the app
// process. The request and response are JSON so Swift can keep a tiny C bridge:
//
//   request:  {"args":["run","--provider","islo","--no-sync","--","uname","-a"],"env":{"ISLO_API_KEY":"..."}}
//   response: {"exitCode":0,"stdout":"...","stderr":"..."}
//
// iOS apps cannot spawn a separate `crabbox` process, but this function executes
// the same internal CLI package and the mobile-safe islo provider compiled into
// an iOS static library.
//
//export CrabboxMobileRun
func CrabboxMobileRun(raw *C.char) *C.char {
	req := runRequest{}
	if raw != nil {
		if err := json.Unmarshal([]byte(C.GoString(raw)), &req); err != nil {
			return cJSON(runResponse{ExitCode: 2, Error: fmt.Sprintf("decode request: %v", err)})
		}
	}
	if len(req.Args) > 0 && req.Args[0] == "crabbox" {
		req.Args = req.Args[1:]
	}
	if len(req.Args) == 0 {
		return cJSON(runResponse{ExitCode: 2, Error: "missing crabbox command"})
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	runMu.Lock()
	defer runMu.Unlock()
	restore := applyEnv(req.Env)
	defer restore()

	var stdout, stderr bytes.Buffer
	err := (cli.App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  bytes.NewBufferString(req.Stdin),
	}).Run(ctx, req.Args)

	resp := runResponse{
		ExitCode: 0,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
	if err != nil {
		var exit cli.ExitError
		if cli.AsExitError(err, &exit) {
			resp.ExitCode = exit.Code
			if exit.Message != "" {
				if resp.Stderr != "" && resp.Stderr[len(resp.Stderr)-1] != '\n' {
					resp.Stderr += "\n"
				}
				resp.Stderr += exit.Message + "\n"
			}
		} else {
			resp.ExitCode = 1
			resp.Error = err.Error()
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		resp.ExitCode = 124
		resp.Error = "crabbox command timed out"
	}
	return cJSON(resp)
}

//export CrabboxMobileFree
func CrabboxMobileFree(ptr *C.char) {
	if ptr != nil {
		C.free(unsafe.Pointer(ptr))
	}
}

func applyEnv(env map[string]string) func() {
	type previous struct {
		value string
		ok    bool
	}
	old := map[string]previous{}
	for key, value := range env {
		if key == "" {
			continue
		}
		current, ok := os.LookupEnv(key)
		old[key] = previous{value: current, ok: ok}
		_ = os.Setenv(key, value)
	}
	return func() {
		for key, item := range old {
			if item.ok {
				_ = os.Setenv(key, item.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
}

func cJSON(resp runResponse) *C.char {
	data, err := json.Marshal(resp)
	if err != nil {
		data = []byte(`{"exitCode":1,"error":"encode response"}`)
	}
	return C.CString(string(data))
}

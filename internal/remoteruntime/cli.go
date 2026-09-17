// Package remoteruntime implements the credential-free remote command runtime.
// It does not load CLI configuration or resolve providers.
package remoteruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const Command = "__remote-control"
const Protocol = "CBX-REMOTE-1"
const MaxCleanupWait = 10 * time.Second
const failureCode = 74

// Identity is the runtime capability handshake; executable byte verification
// remains the installer's responsibility.
type Identity struct {
	Protocol string `json:"protocol"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Runnable bool   `json:"runnable"`
}

type request struct {
	nonce                    string
	commandBytes, inputBytes int64
	idle, grace, execution   time.Duration
	preflight, watchInput    bool
}

// RunCLI is an internal entry point. Command bytes and workload input arrive on
// stdin, never in arguments; stdout and stderr belong exclusively to the worker.
func RunCLI(ctx context.Context, args []string, stdin, stdout, stderr *os.File) int {
	if len(args) == 1 && args[0] == "identity" {
		if err := json.NewEncoder(stdout).Encode(Identity{Protocol, runtime.GOOS, runtime.GOARCH, runtime.GOOS == "linux"}); err != nil {
			return failureCode
		}
		return 0
	}
	if len(args) == 2 && args[0] == "guard" && args[1] == Protocol {
		return runGuard()
	}
	if len(args) > 0 && args[0] == "control" {
		req, err := parseControlRequest(args)
		if err != nil {
			fmt.Fprintln(stderr, "remote runtime:", err)
			return failureCode
		}
		controlCtx, cancel := context.WithTimeout(ctx, req.timeout)
		defer cancel()
		if err := control(controlCtx, req.nonce, req.action, stdout); err != nil {
			fmt.Fprintln(stderr, "remote runtime:", err)
			return failureCode
		}
		return 0
	}
	req, err := parseRequest(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return failureCode
	}
	code, err := run(ctx, req, stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "remote runtime:", err)
		return failureCode
	}
	return code
}

type controlRequest struct {
	nonce, action string
	timeout       time.Duration
}

func parseControlRequest(args []string) (controlRequest, error) {
	invalid := errors.New("invalid remote runtime control request")
	if len(args) < 4 || args[0] != "control" || args[1] != Protocol || !validNonce(args[2]) {
		return controlRequest{}, invalid
	}
	req := controlRequest{nonce: args[2], action: args[3], timeout: 15 * time.Second}
	if req.action != "cleanup" {
		if len(args) != 4 || req.action != "observe" && req.action != "cancel" && req.action != "retire" {
			return controlRequest{}, invalid
		}
		return req, nil
	}
	if len(args) != 5 || args[4] == "" || strings.Trim(args[4], "0123456789") != "" {
		return controlRequest{}, invalid
	}
	millis, err := strconv.ParseInt(args[4], 10, 64)
	if err != nil || millis <= 0 || millis > MaxCleanupWait.Milliseconds() {
		return controlRequest{}, invalid
	}
	req.timeout = time.Duration(millis) * time.Millisecond
	return req, nil
}

func parseRequest(args []string) (request, error) {
	var req request
	invalid := errors.New("invalid remote runtime request")
	if len(args) != 10 || args[0] != "run" || args[1] != Protocol {
		return req, invalid
	}
	req.nonce = args[2]
	if !validNonce(req.nonce) {
		return request{}, invalid
	}
	values := make([]int64, 5)
	for i, text := range args[3:8] {
		if text == "" || strings.Trim(text, "0123456789") != "" {
			return request{}, invalid
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return request{}, invalid
		}
		values[i] = value
	}
	req.commandBytes, req.inputBytes = values[0], values[1]
	if req.commandBytes > 64<<20 || req.inputBytes > 1<<40 || req.commandBytes+req.inputBytes > 1<<40 {
		return request{}, invalid
	}
	const maxMilliseconds = int64((1<<63 - 1) / time.Millisecond)
	for _, value := range values[2:] {
		if value > maxMilliseconds {
			return request{}, invalid
		}
	}
	req.idle, req.grace, req.execution = time.Duration(values[2])*time.Millisecond, time.Duration(values[3])*time.Millisecond, time.Duration(values[4])*time.Millisecond
	if req.idle == 0 || req.grace == 0 {
		return request{}, invalid
	}
	switch args[8] {
	case "command":
	case "preflight":
		req.preflight = true
	default:
		return request{}, invalid
	}
	switch args[9] {
	case "finite":
	case "watched":
		req.watchInput = true
	default:
		return request{}, invalid
	}
	return req, nil
}

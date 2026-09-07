package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// A fresh marker pair proves that setup reached the workload and that its
// status was observed. The enclosing SSH command exits zero only on completion;
// an SSH failure never authorizes evidence, even if a marker was received.
type runExitWitness struct {
	prefix  []byte
	pending []byte
	output  io.Writer
	started bool
	exited  bool
	invalid bool
	code    int
}

func newRunExitWitness(output io.Writer) (*runExitWitness, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	return &runExitWitness{prefix: []byte("\x1eCRABBOX_RUN_" + nonce + ":"), output: output}, nil
}

func (w *runExitWitness) Write(data []byte) (int, error) {
	w.pending = append(w.pending, data...)
	for len(w.pending) > 0 {
		index := bytes.Index(w.pending, w.prefix)
		if index < 0 {
			keep := 0
			for n := 1; n < len(w.prefix) && n <= len(w.pending); n++ {
				if bytes.Equal(w.pending[len(w.pending)-n:], w.prefix[:n]) {
					keep = n
				}
			}
			if _, err := w.output.Write(w.pending[:len(w.pending)-keep]); err != nil {
				return 0, err
			}
			w.pending = bytes.Clone(w.pending[len(w.pending)-keep:])
			break
		}
		if index > 0 {
			if _, err := w.output.Write(w.pending[:index]); err != nil {
				return 0, err
			}
			w.pending = w.pending[index:]
		}
		end := bytes.IndexByte(w.pending[len(w.prefix):], '\x1f')
		if end < 0 && len(w.pending) <= len(w.prefix)+16 {
			break
		}
		if end < 0 {
			w.invalid = true
			w.pending = w.pending[len(w.prefix):]
			continue
		}
		record := string(w.pending[len(w.prefix) : len(w.prefix)+end])
		w.pending = w.pending[len(w.prefix)+end+1:]
		if record == "start" && !w.started && !w.exited {
			w.started = true
		} else if value, ok := strings.CutPrefix(record, "exit:"); ok && !w.exited {
			code, err := strconv.Atoi(value)
			if err != nil || code < 0 || code > 255 || strconv.Itoa(code) != value {
				w.invalid = true
			} else {
				w.code, w.exited = code, true
			}
		} else {
			w.invalid = true
		}
	}
	return len(data), nil
}

func (w *runExitWitness) finish(ctx context.Context, transportCode int, transportErr error) (int, error, bool) {
	if len(w.pending) > 0 {
		w.invalid = true
		if _, err := w.output.Write(w.pending); err != nil && transportErr == nil {
			transportErr = err
		}
		w.pending = nil
	}
	if transportCode != 0 || transportErr != nil || ctx.Err() != nil {
		if transportCode == 0 {
			if transportErr == nil {
				transportErr = ctx.Err()
			}
			transportCode = 7
		}
		return transportCode, transportErr, false
	}
	if w.invalid || !w.exited {
		return 7, exit(7, "SSH completion did not include a valid owned workload exit marker; failure downloads skipped"), false
	}
	if !w.started && w.code == 0 {
		return 7, exit(7, "SSH setup did not reach the workload; failure downloads skipped"), false
	}
	return w.code, nil, w.started && w.code != 0
}

func (w *runExitWitness) command(workdir string, env map[string]string, envFiles []string, command []string, shell bool, script *RunScriptSpec) string {
	var body string
	var args []string
	if script != nil {
		body = `exec "$@"`
		if !script.Shebang {
			args = append(args, "bash")
		}
		args = append(args, script.RemotePath)
		args = append(args, command...)
	} else if shell {
		body = strings.Join(command, " ")
	} else {
		body, args = `exec "$@"`, command
	}
	start := "printf " + shellQuote(string(w.prefix)+"start\x1f") + " >&2; "
	inner := "bash -lc " + shellQuote(remoteBashLoginScript(workdir, "{ "+start+"\n"+body+"\n}")) + " bash"
	for _, arg := range args {
		inner += " " + shellQuote(arg)
	}
	var b strings.Builder
	writeRemoteCommandPrefix(&b, workdir, env, envFiles)
	// The parent does not load shell profiles or run in a conditional context:
	// the inner login shell retains ordinary startup, exec, and errexit behavior.
	outer := `"$@"; result=$?; printf ` + shellQuote(string(w.prefix)+"exit:%d\x1f") + ` "$result" >&2; exit 0`
	b.WriteString("/bin/sh -c " + shellQuote(outer) + " sh " + inner)
	return b.String()
}

func validateFailureDownloadTarget(target SSHTarget, downloads []string) error {
	if len(downloads) > 0 && target.TargetOS != targetLinux {
		return exit(2, "--download-on-failure requires an ordinary SSH-backed Linux target")
	}
	return nil
}

func collectFailureDownloads(ctx context.Context, target SSHTarget, workdir string, downloads []string, output io.Writer) {
	for _, spec := range downloads {
		fileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		count, local, err := downloadRemoteFile(fileCtx, target, workdir, spec)
		cancel()
		if err != nil {
			fmt.Fprintf(output, "warning: --download-on-failure: %v\n", err)
			continue
		}
		fmt.Fprintf(output, "downloaded after failure %s bytes=%d\n", local, count)
	}
}

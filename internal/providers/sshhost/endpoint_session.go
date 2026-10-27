package sshhost

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/crypto/ssh"
)

type endpointPTYRequest struct {
	Term                         string
	Columns, Rows, Width, Height uint32
	Modes                        string
}

func endpointShell(ctx context.Context, command string, shell bool) *exec.Cmd {
	if runtime.GOOS == "windows" {
		if shell {
			return exec.CommandContext(ctx, "cmd.exe", "/d")
		}
		return exec.CommandContext(ctx, "cmd.exe", "/d", "/s", "/c", command)
	}
	if shell {
		return exec.CommandContext(ctx, "/bin/sh")
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

// Requests and process completion are handled together so resizing and channel
// closure remain effective while a shell is running. This code never changes
// identity: the endpoint already runs inside the isolated borrower account.
func serveEndpointSession(ctx context.Context, workRoot string, stream ssh.Channel, requests <-chan *ssh.Request) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer stream.Close()
	var terminal *os.File
	defer func() {
		if terminal != nil {
			terminal.Close()
		}
	}()
	var requested *endpointPTYRequest
	var finished <-chan error
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-finished:
			code := uint32(0)
			if err != nil {
				code = 1
				var ee *exec.ExitError
				if errors.As(err, &ee) && ee.ExitCode() > 0 {
					code = uint32(ee.ExitCode())
				}
			}
			stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
			return
		case req, ok := <-requests:
			if !ok {
				return
			}
			switch req.Type {
			case "pty-req":
				var p endpointPTYRequest
				valid := finished == nil && requested == nil && endpointPTYSupported() && ssh.Unmarshal(req.Payload, &p) == nil && len(p.Term) <= 256 && p.Columns <= 65535 && p.Rows <= 65535
				if valid {
					requested = &p
				}
				req.Reply(valid, nil)
			case "window-change":
				var size struct{ Columns, Rows, Width, Height uint32 }
				valid := ssh.Unmarshal(req.Payload, &size) == nil && terminal != nil && size.Columns <= 65535 && size.Rows <= 65535
				if valid {
					valid = resizeEndpointPTY(terminal, uint16(size.Rows), uint16(size.Columns)) == nil
				}
				req.Reply(valid, nil)
			case "shell", "exec":
				var input struct{ Command string }
				valid := finished == nil
				if req.Type == "exec" {
					valid = valid && ssh.Unmarshal(req.Payload, &input) == nil && len(input.Command) <= 1024*1024
				} else {
					valid = valid && len(req.Payload) == 0
				}
				if !valid {
					req.Reply(false, nil)
					continue
				}
				command := endpointShell(ctx, input.Command, req.Type == "shell")
				command.Dir = workRoot
				command.WaitDelay = 5 * time.Second
				var err error
				var outputDone chan struct{}
				if requested != nil {
					command.Env = append(os.Environ(), "TERM="+requested.Term)
					terminal, err = startEndpointPTY(command, requested)
					if err == nil {
						tty := terminal
						outputDone = make(chan struct{})
						go func() { io.Copy(stream, tty); close(outputDone) }()
						go func() { io.Copy(tty, stream) }()
					}
				} else {
					var stdin io.WriteCloser
					stdin, err = command.StdinPipe()
					if err == nil {
						command.Stdout, command.Stderr = stream, stream.Stderr()
						err = command.Start()
						if err == nil {
							go func() { io.Copy(stdin, stream); stdin.Close() }()
						} else {
							stdin.Close()
						}
					}
				}
				req.Reply(err == nil, nil)
				if err != nil {
					return
				}
				done := make(chan error, 1)
				finished = done
				go func() {
					err := command.Wait()
					if outputDone != nil {
						select {
						case <-outputDone:
						case <-ctx.Done():
						case <-time.After(time.Second):
						}
					}
					done <- err
				}()
			default:
				req.Reply(false, nil)
			}
		}
	}
}

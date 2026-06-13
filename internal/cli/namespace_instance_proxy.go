package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type namespaceProxyOutput struct {
	Endpoint string `json:"endpoint"`
}

func (a App) namespaceInstanceProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("__namespace-instance-proxy", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	nsc := fs.String("nsc", "nsc", "nsc CLI path")
	endpoint := fs.String("endpoint", "", "Namespace endpoint")
	region := fs.String("region", "", "Namespace region")
	keychain := fs.String("keychain", "", "Namespace keychain")
	if err := fs.Parse(args); err != nil {
		return exit(2, "%v", err)
	}
	if fs.NArg() != 1 {
		return exit(2, "namespace instance proxy requires an instance id")
	}

	dir, err := os.MkdirTemp("", "crabbox-namespace-proxy-*")
	if err != nil {
		return exit(2, "create Namespace proxy temp directory: %v", err)
	}
	defer os.RemoveAll(dir)
	outputPath := filepath.Join(dir, "proxy.json")
	nscArgs := make([]string, 0, 12)
	for _, item := range []struct {
		flag  string
		value string
	}{
		{"--endpoint", *endpoint},
		{"--region", *region},
		{"--keychain", *keychain},
	} {
		if strings.TrimSpace(item.value) != "" {
			nscArgs = append(nscArgs, item.flag, item.value)
		}
	}
	nscArgs = append(nscArgs, "proxy", fs.Arg(0), "--service", "ssh", "--once", "--output", "json", "--output_to", outputPath)

	cmd := exec.CommandContext(ctx, *nsc, nscArgs...)
	cmd.Stdout = io.Discard
	cmd.Stderr = a.Stderr
	if err := cmd.Start(); err != nil {
		return exit(2, "start nsc proxy: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	proxy, processExited, err := waitForNamespaceProxy(ctx, outputPath, done, 30*time.Second)
	if err != nil {
		if !processExited {
			_ = cmd.Process.Kill()
			<-done
		}
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxy.Endpoint)
	if err != nil {
		_ = cmd.Process.Kill()
		<-done
		return exit(2, "connect Namespace proxy %s: %v", proxy.Endpoint, err)
	}

	processErr, processExited, copyErr := copyNamespaceProxyStreams(ctx, conn, a.input(), a.Stdout, done)
	if ctx.Err() != nil {
		if !processExited {
			_ = cmd.Process.Kill()
			<-done
		}
		return ctx.Err()
	}
	if processErr != nil && copyErr == nil {
		return exit(exitCode(processErr), "nsc proxy exited: %v", processErr)
	}
	if copyErr != nil && !errors.Is(copyErr, net.ErrClosed) {
		return exit(2, "Namespace proxy stream: %v", copyErr)
	}
	return nil
}

type namespaceProxyCopyResult struct {
	output bool
	err    error
}

func copyNamespaceProxyStreams(ctx context.Context, conn net.Conn, input io.Reader, output io.Writer, done <-chan error) (error, bool, error) {
	copyDone := make(chan namespaceProxyCopyResult, 2)
	go func() {
		_, copyErr := io.Copy(conn, input)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyDone <- namespaceProxyCopyResult{err: copyErr}
	}()
	go func() {
		_, copyErr := io.Copy(output, conn)
		copyDone <- namespaceProxyCopyResult{output: true, err: copyErr}
	}()

	var processErr error
	processExited := false
	var streamErr error
	processDone := done
	for {
		select {
		case result := <-copyDone:
			if result.err != nil && !errors.Is(result.err, net.ErrClosed) && streamErr == nil {
				streamErr = result.err
			}
			if !result.output {
				if result.err != nil {
					_ = conn.Close()
				}
				continue
			}
			_ = conn.Close()
			if processDone != nil {
				select {
				case processErr = <-processDone:
					processExited = true
				case <-ctx.Done():
				}
			}
			return processErr, processExited, streamErr
		case processErr = <-processDone:
			processExited = true
			processDone = nil
		case <-ctx.Done():
			_ = conn.Close()
			return processErr, processExited, ctx.Err()
		}
	}
}

func waitForNamespaceProxy(ctx context.Context, path string, done <-chan error, timeout time.Duration) (namespaceProxyOutput, bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var outputErr error
	for {
		if data, err := os.ReadFile(path); err == nil {
			var output namespaceProxyOutput
			if err := json.Unmarshal(data, &output); err != nil {
				outputErr = exit(5, "parse nsc proxy output: %v", err)
			} else if strings.TrimSpace(output.Endpoint) == "" {
				outputErr = exit(5, "nsc proxy output omitted endpoint")
			} else if _, _, err := net.SplitHostPort(output.Endpoint); err != nil {
				outputErr = exit(5, "invalid nsc proxy endpoint %q: %v", output.Endpoint, err)
			} else {
				return output, false, nil
			}
		}
		select {
		case err := <-done:
			if outputErr != nil {
				return namespaceProxyOutput{}, true, outputErr
			}
			if err == nil {
				return namespaceProxyOutput{}, true, exit(5, "nsc proxy exited before publishing an endpoint")
			}
			return namespaceProxyOutput{}, true, exit(exitCode(err), "nsc proxy exited before publishing an endpoint: %v", err)
		case <-ticker.C:
		case <-deadline.C:
			if outputErr != nil {
				return namespaceProxyOutput{}, false, outputErr
			}
			return namespaceProxyOutput{}, false, exit(5, "timed out waiting for nsc proxy endpoint")
		case <-ctx.Done():
			return namespaceProxyOutput{}, false, ctx.Err()
		}
	}
}

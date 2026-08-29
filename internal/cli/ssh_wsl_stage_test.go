package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func TestWSLStageEnvelopeIsFiniteBlindedAndLauncherSafe(t *testing.T) {
	entropy := bytes.Repeat([]byte{0xa5}, 32)
	oldEntropy := wslStageEntropy
	wslStageEntropy = bytes.NewReader(entropy)
	t.Cleanup(func() { wslStageEntropy = oldEntropy })

	command := "printf private-command"
	payload := []byte("private-input")
	spool, err := newWSLStageSpool(command, payload, nil, int64(len(payload)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	reader, err := spool.input.reset()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) != int(spool.size) || string(frame[:8]) != "CBXWSL3!" ||
		!bytes.Equal(frame[48:80], entropy) || !bytes.HasSuffix(frame, append([]byte(command), payload...)) {
		t.Fatalf("invalid envelope shape size=%d frame=%d", spool.size, len(frame))
	}
	digest := sha256.Sum256(frame)
	launcher := wslStageFileCommand(strings.Repeat("a", 32), strings.Repeat("a", 32)+".ready", spool.size, digest, false, wslStageCMD)
	if launcher == "" || len(launcher) >= wslStageLauncherMax {
		t.Fatalf("launcher length=%d", len(launcher))
	}
	for _, secret := range []string{command, string(payload), base64String(entropy)} {
		if strings.Contains(launcher, secret) {
			t.Fatalf("launcher contains private envelope bytes %q", secret)
		}
	}
}

func TestWSLStageEnvelopeRejectsUnboundedOrChangedInput(t *testing.T) {
	source := bytes.NewReader([]byte("input"))
	if _, err := newWSLStageSpool("true", nil, source, 4, 0); err == nil {
		t.Fatal("accepted changed input size")
	}
	if _, err := newWSLStageSpool(strings.Repeat("x", wslStageMaxCommand+1), nil, bytes.NewReader(nil), 0, 0); err == nil {
		t.Fatal("accepted oversized command")
	}
	if _, err := newWSLStageSpool("true", nil, bytes.NewReader(nil), 0, -time.Second); err == nil {
		t.Fatal("accepted negative execution limit")
	}
}

func TestWSLStagePrefixDigestObservesCleanupContext(t *testing.T) {
	spool := testWSLStageSpool(t, "true", bytes.Repeat([]byte("x"), 1<<20))
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("cleanup deadline"))
	if _, err := spool.prefixDigestContext(ctx, spool.size); err == nil || !strings.Contains(err.Error(), "cleanup deadline") {
		t.Fatalf("prefix digest ignored cleanup context: %v", err)
	}
}

func TestWSLStageUsesPrivateAliasOnly(t *testing.T) {
	session := &sshTransportSession{configPath: filepath.Join("private", "ssh_config"), destination: sshTransportHostAlias}
	target := SSHTarget{User: "secret-user", Host: "private.example", ProxyCommand: "secret-proxy"}
	for _, args := range [][]string{
		wslStageSubsystemArgs(session, "2", "1"),
		wslStageCommandArgs(session, "fixed-launcher", "2", "1"),
	} {
		joined := strings.Join(args, "\x00")
		for _, secret := range []string{target.User, target.Host, target.ProxyCommand} {
			if strings.Contains(joined, secret) {
				t.Fatalf("argv leaked %q: %q", secret, args)
			}
		}
		if !slices.Contains(args, sshTransportHostAlias) || !slices.Contains(args, "-F") {
			t.Fatalf("argv does not use private route: %q", args)
		}
	}
}

func TestWSLStageSSHChildOmitsRouteSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("portable child fixture uses a POSIX executable")
	}
	bin := t.TempDir()
	ssh := filepath.Join(bin, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\n[ -z \"$CRABBOX_ROUTE_SECRET\" ] || exit 91\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_ROUTE_SECRET", "private-environment")
	session := &sshTransportSession{configPath: filepath.Join("private", "ssh_config"), destination: sshTransportHostAlias}
	target := SSHTarget{
		User: "private-user", Host: "private.example", ProxyCommand: "private-proxy",
		ChildEnvDenylist: []string{"CRABBOX_ROUTE_SECRET"},
	}
	var output bytes.Buffer
	if err := runWSLStageSSH(context.Background(), target, session, "fixed-launcher", "2", "1", &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{target.User, target.Host, target.ProxyCommand, "private-environment"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("child argv/environment leaked %q: %q", secret, output.String())
		}
	}
}

func TestWSLStageOnlyReplacesWorkspaceOwnedWorkloads(t *testing.T) {
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: "wsl2"}
	raw, err := prepareSSHTransport(target, workspaceOwnerRemotePreparation{command: "raw"}, nil, nil, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if raw.stage != nil || raw.command == "" {
		t.Fatalf("ordinary transport changed: %#v", raw)
	}
	owned, err := prepareSSHTransport(target, workspaceOwnerRemotePreparation{command: "owned", ownerExpanded: true}, nil, nil, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.close()
	if owned.stage == nil || owned.command != "" || owned.input != nil {
		t.Fatalf("owned workload was not staged exactly once: %#v", owned)
	}
}

func TestWSLStagePublishesOneExclusiveEnvelopeAfterRouteProof(t *testing.T) {
	harness := newMemorySFTPHarness(t)
	nonce := strings.Repeat("b", 32)
	spool := testWSLStageSpool(t, "printf ok", []byte("input"))
	harness.putFile(t, wslStageRoot+"/."+nonce+".proof", []byte(nonce))
	restore := installMemoryWSLSFTP(t, harness)
	defer restore()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	published, err := spool.upload(ctx, cancel, SSHTarget{}, &sshTransportSession{}, nonce, "2", "1")
	if err != nil || !published {
		t.Fatalf("published=%t err=%v", published, err)
	}
	frame := harness.readFile(t, wslStageRoot+"/"+nonce+".ready")
	if int64(len(frame)) != spool.size || sha256.Sum256(frame) != spool.digest {
		t.Fatal("upload digest does not bind the published finite envelope")
	}
	harness.requireAbsent(t, wslStageRoot+"/."+nonce+".proof")
	harness.requireAbsent(t, wslStageRoot+"/"+nonce+".part")
}

func TestWSLStageRejectsInvalidProofBeforeEnvelopeWrite(t *testing.T) {
	harness := newMemorySFTPHarness(t)
	nonce := strings.Repeat("d", 32)
	spool := testWSLStageSpool(t, "printf ok", []byte("input"))
	harness.putFile(t, wslStageRoot+"/."+nonce+".proof", []byte("wrong"))
	restore := installMemoryWSLSFTP(t, harness)
	defer restore()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	published, err := spool.upload(ctx, cancel, SSHTarget{}, &sshTransportSession{}, nonce, "2", "1")
	if err == nil || published {
		t.Fatalf("published=%t err=%v", published, err)
	}
	harness.requireAbsent(t, wslStageRoot+"/"+nonce+".part")
	harness.requireAbsent(t, wslStageRoot+"/"+nonce+".ready")
}

func TestWSLStageExclusiveCreateCollisionIsTerminal(t *testing.T) {
	harness := newMemorySFTPHarness(t)
	nonce := strings.Repeat("e", 32)
	spool := testWSLStageSpool(t, "printf ok", nil)
	harness.putFile(t, wslStageRoot+"/."+nonce+".proof", []byte(nonce))
	harness.putFile(t, wslStageRoot+"/"+nonce+".part", []byte("foreign"))
	restore := installMemoryWSLSFTP(t, harness)
	defer restore()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	published, err := spool.upload(ctx, cancel, SSHTarget{}, &sshTransportSession{}, nonce, "2", "1")
	var retryable retryableWSLStageError
	if err == nil || published || errors.As(err, &retryable) {
		t.Fatalf("published=%t err=%v", published, err)
	}
	if got := string(harness.readFile(t, wslStageRoot+"/"+nonce+".part")); got != "foreign" {
		t.Fatalf("exclusive collision was changed: %q", got)
	}
}

func TestWSLStageCollisionAndRenameAmbiguityPreserveEvidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		preexisting bool
		failRename  bool
	}{
		{name: "collision", preexisting: true},
		{name: "rename-ambiguous", failRename: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newMemorySFTPHarness(t)
			nonce := strings.Repeat("c", 32)
			spool := testWSLStageSpool(t, "printf ok", nil)
			harness.putFile(t, wslStageRoot+"/."+nonce+".proof", []byte(nonce))
			if test.preexisting {
				harness.putFile(t, wslStageRoot+"/"+nonce+".ready", []byte("foreign"))
			}
			restore := installMemoryWSLSFTP(t, harness)
			defer restore()
			oldPublish := publishWSLStage
			if test.failRename {
				publishWSLStage = func(*sftp.Client, string, string) error { return io.ErrUnexpectedEOF }
			}
			t.Cleanup(func() { publishWSLStage = oldPublish })

			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			published, err := spool.upload(ctx, cancel, SSHTarget{}, &sshTransportSession{}, nonce, "2", "1")
			if err == nil || published || exitCode(err) == 255 {
				t.Fatalf("published=%t err=%v", published, err)
			}
			if len(harness.readFile(t, wslStageRoot+"/"+nonce+".part")) != int(spool.size) {
				t.Fatal("terminal publication state was speculatively deleted")
			}
		})
	}
}

func TestWSLStageExecutionLossIsNeverReplayable(t *testing.T) {
	harness := newMemorySFTPHarness(t)
	restoreSFTP := installMemoryWSLSFTP(t, harness)
	defer restoreSFTP()
	oldSession, oldRun := newWSLStageSession, runWSLStageCommand
	newWSLStageSession = func(context.Context, SSHTarget, bool) (*sshTransportSession, error) {
		return &sshTransportSession{destination: sshTransportHostAlias}, nil
	}
	var calls []string
	runWSLStageCommand = func(_ context.Context, _ SSHTarget, _ *sshTransportSession, command, _, _ string, stdout, _ io.Writer) error {
		calls = append(calls, command)
		if strings.Contains(decodeWSLStagePowerShellCommand(t, command), wslStagePreparationReady) {
			nonce := extractPreparedNonce(t, command)
			harness.putFile(t, wslStageRoot+"/."+nonce+".proof", []byte(nonce))
			_, _ = io.WriteString(stdout, wslStagePreparationReady+" "+nonce+" cmd\n")
			return nil
		}
		return retryableWSLStageError{errors.New("test exit")}
	}
	t.Cleanup(func() {
		newWSLStageSession = oldSession
		runWSLStageCommand = oldRun
	})
	spool := testWSLStageSpool(t, "printf once", nil)
	err := spool.run(context.Background(), SSHTarget{}, "2", "1", io.Discard, io.Discard)
	var exitErr ExitError
	if err == nil || !AsExitError(err, &exitErr) || exitErr.Code != 7 || len(calls) != 2 {
		t.Fatalf("calls=%d err=%v", len(calls), err)
	}
}

func TestWSLSFTPClassificationRequiresExactOpenSSHRejection(t *testing.T) {
	if !wslSFTPRejected.MatchString("subsystem request failed on channel 0\r\n") {
		t.Fatal("expected exact OpenSSH rejection to match")
	}
	for _, diagnostic := range []string{
		"permission denied",
		"subsystem request failed on channel x",
		"prefix subsystem request failed on channel 0",
	} {
		if wslSFTPRejected.MatchString(diagnostic) {
			t.Fatalf("misclassified diagnostic %q", diagnostic)
		}
	}
}

func TestCopyWSLStageUsesProgressNotTotalElapsed(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 96<<10)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var output bytes.Buffer
	writer := wslStageWriterFunc(func(chunk []byte) (int, error) {
		time.Sleep(10 * time.Millisecond)
		return output.Write(chunk)
	})
	start := time.Now()
	if err := copyWSLStage(ctx, writer, bytes.NewReader(data), int64(len(data)), 25*time.Millisecond, cancel); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond || !bytes.Equal(output.Bytes(), data) {
		t.Fatalf("elapsed=%s bytes=%d", elapsed, output.Len())
	}
}

func TestCopyWSLStageStallCancelsBoundedUpload(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	writer := wslStageWriterFunc(func([]byte) (int, error) {
		<-ctx.Done()
		return 0, context.Cause(ctx)
	})
	start := time.Now()
	err := copyWSLStage(ctx, writer, bytes.NewReader([]byte("x")), 1, 20*time.Millisecond, cancel)
	if err == nil || !strings.Contains(err.Error(), "made no progress") || time.Since(start) > time.Second {
		t.Fatalf("unbounded stall err=%v elapsed=%s", err, time.Since(start))
	}
}

func base64String(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

type wslStageWriterFunc func([]byte) (int, error)

func (f wslStageWriterFunc) Write(data []byte) (int, error) { return f(data) }

func testWSLStageSpool(t *testing.T, command string, payload []byte) *wslStageSpool {
	t.Helper()
	spool, err := newWSLStageSpool(command, payload, nil, int64(len(payload)), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.close() })
	return spool
}

type memorySFTPHarness struct {
	t        *testing.T
	handlers sftp.Handlers
}

func newMemorySFTPHarness(t *testing.T) *memorySFTPHarness {
	t.Helper()
	h := &memorySFTPHarness{t: t, handlers: sftp.InMemHandler()}
	client, closeClient := h.client(t)
	if err := client.Mkdir(".crabbox"); err != nil {
		t.Fatal(err)
	}
	if err := client.Mkdir(wslStageRoot); err != nil {
		t.Fatal(err)
	}
	closeClient()
	return h
}

func (h *memorySFTPHarness) client(t *testing.T) (*sftp.Client, func()) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, h.handlers)
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	return client, func() {
		_ = client.Close()
		_ = server.Close()
		<-done
	}
}

func (h *memorySFTPHarness) open(_ context.Context, _ SSHTarget, _ *sshTransportSession, _, _ string) (*sftp.Client, *wslSFTPProcess, error) {
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, h.handlers)
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		return nil, nil, err
	}
	return client, &wslSFTPProcess{waitFn: func() error {
		_ = server.Close()
		err := <-done
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}}, nil
}

func (h *memorySFTPHarness) putFile(t *testing.T, path string, data []byte) {
	t.Helper()
	client, closeClient := h.client(t)
	defer closeClient()
	file, err := client.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func (h *memorySFTPHarness) readFile(t *testing.T, path string) []byte {
	t.Helper()
	client, closeClient := h.client(t)
	defer closeClient()
	file, err := client.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return data
}

func (h *memorySFTPHarness) requireAbsent(t *testing.T, path string) {
	t.Helper()
	client, closeClient := h.client(t)
	defer closeClient()
	if _, err := client.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s err=%v, want absent", path, err)
	}
}

func installMemoryWSLSFTP(t *testing.T, harness *memorySFTPHarness) func() {
	t.Helper()
	old := openWSLStageSFTP
	openWSLStageSFTP = harness.open
	return func() { openWSLStageSFTP = old }
}

func extractPreparedNonce(t *testing.T, command string) string {
	t.Helper()
	decoded := decodeWSLStagePowerShellCommand(t, command)
	index := strings.Index(decoded, ".proof")
	if index < 32 {
		t.Fatalf("proof nonce missing from %q", decoded)
	}
	return decoded[index-32 : index]
}

func decodeWSLStagePowerShellCommand(t *testing.T, command string) string {
	t.Helper()
	const marker = "FromBase64String('"
	start := strings.Index(command, marker)
	if start < 0 {
		t.Fatalf("encoded script missing from %q", command)
	}
	start += len(marker)
	end := strings.Index(command[start:], "')")
	if end < 0 {
		t.Fatalf("encoded script terminator missing from %q", command)
	}
	data, err := base64.StdEncoding.DecodeString(command[start : start+end])
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

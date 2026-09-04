package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/pkg/sftp"
)

type delayedWriteConn struct {
	net.Conn
	ctx   context.Context
	delay time.Duration
}

func (c delayedWriteConn) Write(p []byte) (int, error) {
	timer := time.NewTimer(c.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
	return c.Conn.Write(p)
}

type slowWriter struct {
	delay time.Duration
	bytes.Buffer
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.Buffer.Write(p)
}

type stallAfterStageWriteConn struct {
	net.Conn
	ctx  context.Context
	path string
}

func (c stallAfterStageWriteConn) Write(p []byte) (int, error) {
	if info, err := os.Stat(c.path); err == nil && info.Size() > 0 {
		<-c.ctx.Done()
	}
	return c.Conn.Write(p)
}

type wslSFTPRequestConn struct {
	net.Conn
	readsBeforeWrite atomic.Int32
	readsAfterWrite  atomic.Int32
	wrote            bool
	beforeClose      func()

	header      [4]byte
	headerBytes int
	remaining   uint32
}

func (c *wslSFTPRequestConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	for _, value := range p[:n] {
		if c.remaining == 0 {
			c.header[c.headerBytes] = value
			c.headerBytes++
			if c.headerBytes == len(c.header) {
				c.remaining = binary.BigEndian.Uint32(c.header[:])
				c.headerBytes = 0
			}
			continue
		}
		if c.remaining == binary.BigEndian.Uint32(c.header[:]) {
			switch value {
			case 9, 10:
				return 0, errors.New("Windows OpenSSH rejects SFTP chmod/setstat")
			case 5:
				if c.wrote {
					c.readsAfterWrite.Add(1)
				} else {
					c.readsBeforeWrite.Add(1)
				}
			case 6:
				c.wrote = true
			case 4:
				if c.wrote && c.beforeClose != nil {
					c.beforeClose()
					c.beforeClose = nil
				}
			}
		}
		c.remaining--
	}
	return n, err
}

func startLoopbackWSLSFTPSubsystem(ctx context.Context, root string, wrap func(net.Conn) net.Conn) (io.Reader, io.WriteCloser, func() error, error) {
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(wslStageRoot)), 0o700); err != nil {
		return nil, nil, nil, err
	}
	clientConn, serverConn := net.Pipe()
	server, err := sftp.NewServer(wrap(serverConn), sftp.WithServerWorkingDirectory(root))
	if err != nil {
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	go func() {
		<-ctx.Done()
		_ = clientConn.Close()
		_ = serverConn.Close()
	}()
	return clientConn, clientConn, func() error { return <-done }, nil
}

func newLoopbackSFTPClient(t *testing.T, root string) *sftp.Client {
	return newLoopbackSFTPClientWithServerConn(t, root, func(conn net.Conn) net.Conn { return conn })
}

func newLoopbackSFTPClientWithServerConn(t *testing.T, root string, wrap func(net.Conn) net.Conn) *sftp.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	output, input, wait, err := startLoopbackWSLSFTPSubsystem(ctx, root, wrap)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	client, err := sftp.NewClientPipe(output, input)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer cancel()
		_ = client.Close()
		done := make(chan error, 1)
		go func() { done <- wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("SFTP server: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("SFTP server did not stop")
		}
	})
	return client
}

func stubWSLStageRoutePreparation(t *testing.T, prepare func(context.Context, SSHTarget, string, string) error) {
	t.Helper()
	previous, oldDiscard, oldProbe := prepareWSLStageRoute, discardWSLStageFile, probeWSLStageRoute
	probeWSLStageRoute = func(context.Context, SSHTarget, string, string) error { return nil }
	prepareWSLStageRoute = func(ctx context.Context, target SSHTarget, connect, attempts string) (wslStageShell, error) {
		return wslStageCMD, prepare(ctx, target, connect, attempts)
	}
	discardWSLStageFile = func(ctx context.Context, target SSHTarget, nonce, name string, size int64, digest [sha256.Size]byte, connect string) error {
		if strings.HasSuffix(name, ".proof") {
			return nil
		}
		return oldDiscard(ctx, target, nonce, name, size, digest, connect)
	}
	t.Cleanup(func() { prepareWSLStageRoute, discardWSLStageFile, probeWSLStageRoute = previous, oldDiscard, oldProbe })
}

func TestWSLStageBudgetScalesForOneUpload(t *testing.T) {
	const largeEnvelope = int64(8_395_990)
	tests := []struct {
		name       string
		floor      time.Duration
		idle       time.Duration
		frameBytes int64
		want       time.Duration
	}{
		{name: "tiny", floor: 2 * time.Minute, idle: 15 * time.Second, frameBytes: 1, want: 2 * time.Minute},
		{name: "one mibibyte", floor: 2 * time.Minute, idle: 15 * time.Second, frameBytes: 1 << 20, want: 2 * time.Minute},
		{name: "large binary envelope", floor: 2 * time.Minute, idle: 15 * time.Second, frameBytes: largeEnvelope, want: 4 * time.Minute},
		{name: "workspace owner small frame", floor: sshTransportTiming(sshCommandLimit{execution: sshControlExecutionLimit, control: true}).stage, idle: 2 * time.Second, frameBytes: 64 << 10, want: sshTransportTiming(sshCommandLimit{execution: sshControlExecutionLimit, control: true}).stage},
		{name: "overflow", floor: 2 * time.Minute, idle: time.Duration(1 << 62), frameBytes: 3 << 20, want: time.Duration(1<<63 - 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := wslStageBudget(test.floor, test.idle, test.frameBytes); got != test.want {
				t.Fatalf("budget=%s want=%s", got, test.want)
			}
		})
	}
}

func TestWSLStageCleanupBudgetUsesShortBoundAfterParentCancellation(t *testing.T) {
	if got := wslStageCleanupBudget(t.Context()); got != wslStageCleanupTimeout {
		t.Fatalf("active cleanup budget=%s", got)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if got := wslStageCleanupBudget(cancelled); got != wslStageCanceledCleanupTimeout {
		t.Fatalf("cancelled cleanup budget=%s", got)
	}
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if got := wslStageCleanupBudget(expired); got != wslStageCanceledCleanupTimeout {
		t.Fatalf("expired cleanup budget=%s", got)
	}
}

func TestWSLStageRootPreparationUsesPinnedNoInputRouteAndFixedMarker(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	nonce := strings.Repeat("a", 32)
	t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", wslStagePreparationMarker+" "+nonce+" powershell")
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "2299", FallbackPorts: []string{}, NoControlMaster: true}
	if shell, err := prepareWSLStageRoot(context.WithValue(t.Context(), wslStageRouteProofKey{}, nonce), target, "10", "3"); err != nil || shell != wslStagePowerShell {
		t.Fatal(err)
	}
	command, input := readWorkspaceOwnerSSHCall(t, dir, 1)
	if command != wslStageRootPreparationCommand(nonce) || input != "" {
		t.Fatalf("ACL prep command matches=%t stdin=%q", command == wslStageRootPreparationCommand(nonce), input)
	}
	args, err := os.ReadFile(filepath.Join(dir, "1.args"))
	if err != nil {
		t.Fatal(err)
	}
	if text := "\n" + string(args); !strings.Contains(text, "\n-n\n") || !strings.Contains(text, "\n-p\n2299\n") {
		t.Fatalf("ACL prep was not no-input on the selected route: %q", text)
	}
	requireWorkspaceOwnerSSHNoMux(t, dir, 1)
	requireWorkspaceOwnerSSHOptions(t, dir, 1, "10", "3")
}

func TestWSLStageRootPreparationRedactsFailureDetails(t *testing.T) {
	installWorkspaceOwnerRecordingSSH(t)
	const secret = "private-stage-path-user-and-sid"
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "1")
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CODE", "74")
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_STDOUT", secret)
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_STDERR", secret)
	_, err := prepareWSLStageRoot(t.Context(), SSHTarget{User: secret, Host: "127.0.0.1", Port: "22", AuthSecret: true}, "10", "3")
	var retryable retryableWSLStageError
	if err == nil || errors.As(err, &retryable) || strings.Contains(err.Error(), secret) ||
		!strings.Contains(err.Error(), "prepare private WSL2 stage failed") {
		t.Fatalf("private ACL failure leaked or became retryable: %v", err)
	}
}

func TestWSLStageRootPreparationTimeoutAllowsFallbackWhileParentLives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("route timeout fixture requires a POSIX SSH executable")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec sleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	parent, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := prepareWSLStageRootWithin(parent, SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22"}, "10", "3", 25*time.Millisecond)
	var retryable retryableWSLStageError
	if !errors.As(err, &retryable) || !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil {
		t.Fatalf("candidate timeout did not preserve budgeted fallback: err=%v parent=%v", err, parent.Err())
	}
	canceled, cancelParent := context.WithCancel(t.Context())
	cancelParent()
	_, err = prepareWSLStageRootWithin(canceled, SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22"}, "10", "3", time.Second)
	if !errors.Is(err, context.Canceled) || errors.As(err, &retryable) {
		t.Fatalf("parent cancellation became a retryable route failure: %v", err)
	}
}

func TestProbeWSLSFTPSubsystemHandshakesClosesAndDoesNoFilesystemWork(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	oldStart := startWSLSFTPSubsystem
	waited := false
	startWSLSFTPSubsystem = func(_ context.Context, _ SSHTarget, connectTimeout, attempts, subsystem string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		if connectTimeout != "10" || attempts != "3" || subsystem != "sftp" {
			t.Fatalf("SFTP start timeout=%q attempts=%q subsystem=%q", connectTimeout, attempts, subsystem)
		}
		clientConn, serverConn := net.Pipe()
		server, err := sftp.NewServer(serverConn, sftp.WithServerWorkingDirectory(root))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- server.Serve() }()
		return clientConn, clientConn, func() error {
			waited = true
			return <-done
		}, nil
	}
	t.Cleanup(func() { startWSLSFTPSubsystem = oldStart })

	if err := probeWSLSFTPSubsystem(t.Context(), SSHTarget{}, "10", "3", io.Discard); err != nil {
		t.Fatal(err)
	}
	if !waited {
		t.Fatal("SFTP probe returned without waiting for the owned subsystem")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("SFTP probe performed filesystem work: entries=%v err=%v", entries, err)
	}
}

func TestProbeWSLSFTPSubsystemClassifiesRejectionTransportAndCancellation(t *testing.T) {
	oldStart := startWSLSFTPSubsystem
	t.Cleanup(func() { startWSLSFTPSubsystem = oldStart })
	exit255 := exec.Command("sh", "-c", "exit 255").Run()
	if runtime.GOOS == "windows" {
		exit255 = exec.Command("cmd.exe", "/c", "exit 255").Run()
	}
	tests := []struct {
		name       string
		ctx        context.Context
		diagnostic string
		packet     []byte
		waitErr    error
		check      func(error) bool
	}{
		{name: "rejected", ctx: t.Context(), diagnostic: "subsystem request failed on channel 0\n", waitErr: exit255, check: IsWSLSFTPUnavailable},
		{name: "transport", ctx: t.Context(), diagnostic: "Connection reset by peer\n", waitErr: exit255, check: func(err error) bool {
			return exitCode(err) == 255 && !IsWSLSFTPUnavailable(err)
		}},
		{name: "unanchored marker", ctx: t.Context(), diagnostic: "prefix: subsystem request failed on channel 0\n", waitErr: exit255, check: func(err error) bool {
			return exitCode(err) == 255 && !IsWSLSFTPUnavailable(err)
		}},
		{name: "malformed protocol", ctx: t.Context(), packet: []byte{0, 0, 0, 5, 99, 0, 0, 0, 3}, waitErr: exit255, check: func(err error) bool {
			return exitCode(err) == 255 && !IsWSLSFTPUnavailable(err)
		}},
		{name: "cancelled", ctx: func() context.Context { ctx, cancel := context.WithCancel(t.Context()); cancel(); return ctx }(), waitErr: context.Canceled, check: func(err error) bool {
			return errors.Is(err, context.Canceled) && !IsWSLSFTPUnavailable(err)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			startWSLSFTPSubsystem = func(_ context.Context, _ SSHTarget, _, _, _ string, stderr io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				_, _ = io.WriteString(stderr, test.diagnostic)
				clientConn, serverConn := net.Pipe()
				if len(test.packet) == 0 {
					_ = serverConn.Close()
				} else {
					go func() {
						_, _ = io.CopyN(io.Discard, serverConn, 9)
						_, _ = serverConn.Write(test.packet)
						_ = serverConn.Close()
					}()
				}
				return clientConn, clientConn, func() error { return test.waitErr }, nil
			}
			if err := probeWSLSFTPSubsystem(test.ctx, SSHTarget{}, "10", "3", io.Discard); err == nil || !test.check(err) {
				t.Fatalf("probe error=%v", err)
			}
		})
	}
}

func TestWSLSFTPDiagnosticsAreBoundedWithoutSuppressingStderr(t *testing.T) {
	var stderr bytes.Buffer
	tee, captured := captureWSLSFTPDiagnostics(SSHTarget{}, &stderr)
	diagnostic := strings.Repeat("x", wslSFTPDiagnosticLimit*2)
	if _, err := io.WriteString(tee, diagnostic); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != diagnostic || captured.buffer.Len() != wslSFTPDiagnosticLimit {
		t.Fatalf("stderr=%d captured=%d", stderr.Len(), captured.buffer.Len())
	}
}

func TestWSLSFTPDiagnosticsRedactSecretAuthenticationWithoutChangingClassification(t *testing.T) {
	secret := "private-authentication-user"
	target := SSHTarget{User: secret, Host: "private.example", AuthSecret: true}
	var stderr bytes.Buffer
	tee, captured := captureWSLSFTPDiagnostics(target, &stderr)
	diagnostic := secret + "@private.example: Permission denied\nsubsystem request failed on channel 0\n"
	for _, fragment := range []string{secret[:5], secret[5:17], secret[17:] + "@private.example: Permission denied\n", "subsystem request failed on channel 0\n"} {
		if _, err := io.WriteString(tee, fragment); err != nil {
			t.Fatal(err)
		}
	}
	if err := captured.flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "[redacted]") {
		t.Fatalf("user-visible SFTP diagnostic leaked secret authentication: %q", stderr.String())
	}
	if captured.buffer.String() != diagnostic || !wslSFTPRejected.MatchString(captured.buffer.String()) {
		t.Fatalf("raw bounded diagnostic no longer supports exact subsystem classification: %q", captured.buffer.String())
	}
}

func newTestWSLStageSpool(t *testing.T, payload []byte) (*wslStageSpool, []byte) {
	t.Helper()
	return newTestWSLStageSpoolWithLimit(t, payload, sshCommandLimit{})
}

func newTestWSLStageSpoolWithLimit(t *testing.T, payload []byte, limit sshCommandLimit) (*wslStageSpool, []byte) {
	t.Helper()
	remote := "printf stage"
	spool, err := newWSLStageSpool(remote, payload, nil, int64(len(payload)), limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := spool.close(); err != nil {
			t.Error(err)
		}
	})
	reader, err := spool.input.reset()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return spool, raw
}

func TestWSLStageBlindingBindsFullAndPartialDigests(t *testing.T) {
	spool, raw := newTestWSLStageSpool(t, []byte{0, 255, 128, 1})
	other, second := newTestWSLStageSpool(t, []byte{0, 255, 128, 1})
	if bytes.Equal(raw[wslStageBlindingOffset:wslStageHeaderSize], second[wslStageBlindingOffset:wslStageHeaderSize]) || spool.digest() == other.digest() {
		t.Fatal("identical workloads reused blinding or digest")
	}
	if !bytes.Equal(raw[:wslStageBlindingOffset], second[:wslStageBlindingOffset]) || !bytes.Equal(raw[wslStageHeaderSize:], second[wslStageHeaderSize:]) {
		t.Fatal("blinding changed program or workload bytes")
	}
	commandStart := wslStageHeaderSize + int(binary.LittleEndian.Uint32(raw[8:])) + int(binary.LittleEndian.Uint32(raw[12:]))
	payloadStart := commandStart + int(binary.LittleEndian.Uint64(raw[16:]))
	for _, size := range []int{-1, 0, 47, 48, 49, 79, 80, 81, commandStart, commandStart + 1, payloadStart, payloadStart + 1, len(raw), len(raw) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			digest, err := spool.prefixDigest(t.Context(), int64(size))
			if size < 0 || size > len(raw) {
				if err == nil {
					t.Fatal("invalid prefix length accepted")
				}
				return
			}
			if err != nil || digest != sha256.Sum256(raw[:size]) {
				t.Fatalf("prefix is not the exact private bytes: %v", err)
			}
			// Every byte of the field must affect any hash reaching user data.
			for i := wslStageBlindingOffset; i < min(size, wslStageHeaderSize); i++ {
				changed := bytes.Clone(raw[:size])
				changed[i] ^= 1
				if sha256.Sum256(changed) == digest {
					t.Fatal("prefix omitted blinding byte")
				}
			}
		})
	}
	reader, err := spool.input.reset()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(raw, replayed) || sha256.Sum256(replayed) != spool.digest() {
		t.Fatal("replay regenerated or lost the blinding field")
	}
}

func TestWSLStageEntropyFailurePreventsDeliveryAndSpoolCreation(t *testing.T) {
	for _, available := range []int{0, wslStageBlindingSize - 1} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(name, dir)
			}
			oldEntropy, oldStage := wslStageEntropy, stageWSLSpool
			t.Cleanup(func() { wslStageEntropy, stageWSLSpool = oldEntropy, oldStage })
			wslStageEntropy = io.MultiReader(bytes.NewReader(bytes.Repeat([]byte{0x91}, available)), iotest.ErrReader(errors.New("private entropy diagnostics")))
			stageWSLSpool = func(*wslStageSpool, context.Context, *SSHTarget, wslStageTiming, string, string, io.Writer) (string, error) {
				t.Fatal("entropy failure reached SSH staging/delivery")
				return "", nil
			}
			var stdout, stderr bytes.Buffer
			target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
			err := executePreparedSSH(t.Context(), &target, "printf exact", nil, 0, sshCommandLimit{}, "1", "1", &stdout, &stderr)
			if err == nil || err.Error() != "generate private WSL2 envelope blinding failed" || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatal("entropy failure succeeded or exposed diagnostics")
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Fatalf("entropy failure left local spool residue: %v", err)
			}
		})
	}
}

func TestWSLStageBlindingStaysOutOfPublicTransportMetadata(t *testing.T) {
	spool, raw := newTestWSLStageSpool(t, []byte("finite input"))
	blinder := raw[wslStageBlindingOffset:wslStageHeaderSize]
	nonce := strings.Repeat("a", 32)
	old := startWSLStageUploadSubsystem
	t.Cleanup(func() { startWSLStageUploadSubsystem = old })
	startWSLStageUploadSubsystem = func(_ context.Context, _ SSHTarget, _, _, _ string, stderr io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		_, _ = io.WriteString(stderr, "connection unavailable\n")
		return nil, nil, nil, io.EOF
	}
	var diagnostics bytes.Buffer
	err := spool.upload(t.Context(), SSHTarget{Port: "22"}, nonce, time.Second, "1", "1", &diagnostics)
	if err == nil {
		t.Fatal("diagnostic fixture succeeded")
	}
	public := []string{diagnostics.String(), err.Error(), wslStageRootPreparationCommand(nonce), wslStagePreparationMarker + " " + nonce + " cmd"}
	sensitivePrefix := int64(wslStageHeaderSize) + int64(binary.LittleEndian.Uint32(raw[8:])) + int64(binary.LittleEndian.Uint32(raw[12:])) + 1
	digest, err := spool.prefixDigest(t.Context(), sensitivePrefix)
	if err != nil {
		t.Fatal(err)
	}
	partial := wslStageFileCommand(nonce, nonce+".part", sensitivePrefix, digest, true, wslStageCMD)
	public = append(public, partial, decodePowerShellCommand(t, partial), decodePowerShellCommand(t, wslStageRootPreparationCommand(nonce)))
	for _, shell := range []wslStageShell{wslStageCMD, wslStagePowerShell} {
		for _, discard := range []bool{false, true} {
			command := wslStageFileCommand(nonce, nonce+".ready", spool.size, spool.digest(), discard, shell)
			if command == "" || len(command) >= wslStageLauncherCommandLimit {
				t.Fatal("invalid or oversized blinded launcher")
			}
			public = append(public, command)
			if shell == wslStageCMD {
				public = append(public, decodePowerShellCommand(t, command))
			}
		}
	}
	for _, representation := range []string{string(blinder), hex.EncodeToString(blinder), strings.ToUpper(hex.EncodeToString(blinder)), base64.StdEncoding.EncodeToString(blinder), base64.RawStdEncoding.EncodeToString(blinder)} {
		for _, value := range public {
			if strings.Contains(value, representation) {
				t.Fatal("private blinding escaped into public transport metadata")
			}
		}
	}
}

func TestUploadToSFTPRequiresPreparedPrivateRoot(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	client := newLoopbackSFTPClient(t, root)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(wslStageRoot))); err != nil {
		t.Fatal(err)
	}
	spool, _ := newTestWSLStageSpool(t, []byte("private payload"))
	_, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	ownership, err := uploadToSFTP(client, spool, strings.Repeat("a", 32), time.Second, cancel)
	if ownership != wslStageUnowned || err == nil || !strings.Contains(err.Error(), "private stage root unavailable") {
		t.Fatalf("upload accepted an unprepared root: ownership=%d err=%v", ownership, err)
	}
}

func TestUploadToSFTPRejectsDifferentSubsystemDirectoryBeforeSensitiveWrite(t *testing.T) {
	for _, test := range []struct {
		name  string
		proof string
		want  bool
	}{
		{name: "different subsystem directory"},
		{name: "forged binding", proof: strings.Repeat("f", 32)},
		{name: "verified private directory", proof: strings.Repeat("a", 32), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			installWSLStageDiscardFixture(t, root)
			client := newLoopbackSFTPClient(t, root)
			spool, _ := newTestWSLStageSpool(t, []byte("sensitive owner payload"))
			nonce := strings.Repeat("a", 32)
			spool.routeProof = nonce
			proof := filepath.Join(root, filepath.FromSlash(wslStageRoot), "."+nonce+".proof")
			if test.proof != "" {
				if err := os.WriteFile(proof, []byte(test.proof), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			ownership, err := uploadToSFTP(client, spool, nonce, time.Second, cancel)
			if (err == nil) != test.want {
				t.Fatalf("route binding ownership=%d error=%v want success=%t", ownership, err, test.want)
			}
			if !test.want {
				if ownership != wslStageUnowned || strings.Contains(err.Error(), nonce) {
					t.Fatalf("invalid route acquired ownership or leaked its challenge: ownership=%d err=%v", ownership, err)
				}
				for _, suffix := range []string{".part", ".ready"} {
					if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(wslStageRoot), nonce+suffix)); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("sensitive stage was written into an unverified directory: %v", statErr)
					}
				}
			} else if _, statErr := os.Stat(proof); statErr != nil {
				t.Fatalf("SFTP verifier removed route proof: %v", statErr)
			}
		})
	}
}

func TestCopyWSLStageAllowsSlowContinuousUploadProgress(t *testing.T) {
	// Only deliberate write delays, not host scheduling, should consume the idle budget.
	synctest.Test(t, func(t *testing.T) {
		data := bytes.Repeat([]byte("x"), 12*32<<10)
		dst := &slowWriter{delay: 10 * time.Millisecond}
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		start := time.Now()
		if err := copyWSLStage(dst, bytes.NewReader(data), int64(len(data)), 25*time.Millisecond, cancel); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < 4*25*time.Millisecond {
			t.Fatalf("upload completed too quickly to exercise progress watchdog: %s", elapsed)
		}
		if !bytes.Equal(dst.Bytes(), data) || context.Cause(ctx) != nil {
			t.Fatalf("upload bytes or watchdog changed: bytes=%d cause=%v", dst.Len(), context.Cause(ctx))
		}
	})
}

func TestCopyWSLStageStallIsRetryableAndBounded(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	blocked := writerFunc(func([]byte) (int, error) {
		<-ctx.Done()
		return 0, context.Cause(ctx)
	})
	start := time.Now()
	operationErr := copyWSLStage(blocked, strings.NewReader("x"), 1, 20*time.Millisecond, cancel)
	err := finishWSLStageAttempt(ctx, wslStageOperationError("upload WSL2 stage failed", operationErr), nil, nil)
	var retryable retryableWSLStageError
	if !errors.As(err, &retryable) || !strings.Contains(err.Error(), "upload WSL2 stage failed") {
		t.Fatalf("upload stall error=%v", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > time.Second {
		t.Fatalf("upload stall bound=%s", elapsed)
	}
}

func TestWSLStageUploadStallFreshSessionRemovesOnlyOwnedPart(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	stageRoot := filepath.Join(root, ".crabbox", "wsl-stage")
	if err := os.MkdirAll(stageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("a", 32)
	ready := filepath.Join(stageRoot, nonce+".ready")
	otherPart := filepath.Join(stageRoot, strings.Repeat("b", 32)+".part")
	if err := os.WriteFile(ready, []byte("existing ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPart, []byte("other partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldUpload, oldCleanup := startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem
	t.Cleanup(func() {
		startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem = oldUpload, oldCleanup
	})
	uploadStarts, cleanupStarts := 0, 0
	startWSLStageUploadSubsystem = func(ctx context.Context, target SSHTarget, _, attempts, subsystem string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		uploadStarts++
		if target.Port != "2222" || len(target.FallbackPorts) != 0 || attempts != "3" || subsystem != "sftp" {
			t.Fatalf("upload target=%+v attempts=%q subsystem=%q", target, attempts, subsystem)
		}
		part := filepath.Join(stageRoot, nonce+".part")
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn {
			return stallAfterStageWriteConn{Conn: conn, ctx: ctx, path: part}
		})
	}
	startWSLStageCleanupSubsystem = func(ctx context.Context, target SSHTarget, connectTimeout, attempts, subsystem string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		cleanupStarts++
		if target.Port != "2222" || len(target.FallbackPorts) != 0 || !target.NoControlMaster ||
			connectTimeout != "10" || attempts != "1" || subsystem != "sftp" {
			t.Fatalf("cleanup target=%+v timeout=%q attempts=%q subsystem=%q", target, connectTimeout, attempts, subsystem)
		}
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
	}
	spool, _ := newTestWSLStageSpool(t, bytes.Repeat([]byte("x"), 2<<20))
	var stderr bytes.Buffer
	err := spool.upload(t.Context(), SSHTarget{Port: "2222", NoControlMaster: true, FallbackPorts: []string{}}, nonce, 20*time.Millisecond, "10", "3", &stderr)
	var retryable retryableWSLStageError
	if !errors.As(err, &retryable) || !strings.Contains(err.Error(), "upload WSL2 stage failed") {
		t.Fatalf("original upload authority error=%v", err)
	}
	if uploadStarts != 1 || cleanupStarts != 1 || stderr.Len() != 0 {
		t.Fatalf("upload starts=%d cleanup starts=%d stderr=%q", uploadStarts, cleanupStarts, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stageRoot, nonce+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned partial survived fresh cleanup: %v", err)
	}
	for path, want := range map[string]string{ready: "existing ready", otherPart: "other partial"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("protected stage %s=%q err=%v", filepath.Base(path), got, err)
		}
	}
}

func TestWSLStageExclusiveCreateFailureOwnsNothingAndSkipsCleanup(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	stageRoot := filepath.Join(root, ".crabbox", "wsl-stage")
	if err := os.MkdirAll(stageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("c", 32)
	part := filepath.Join(stageRoot, nonce+".part")
	if err := os.WriteFile(part, []byte("existing partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldUpload, oldCleanup := startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem
	t.Cleanup(func() {
		startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem = oldUpload, oldCleanup
	})
	startWSLStageUploadSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
	}
	cleanupStarts := 0
	startWSLStageCleanupSubsystem = func(context.Context, SSHTarget, string, string, string, io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		cleanupStarts++
		return nil, nil, nil, errors.New("unexpected cleanup")
	}
	spool, _ := newTestWSLStageSpool(t, []byte("payload"))
	err := spool.upload(t.Context(), SSHTarget{Port: "22"}, nonce, time.Second, "10", "3", io.Discard)
	var retryable retryableWSLStageError
	if err == nil || errors.As(err, &retryable) || cleanupStarts != 0 {
		t.Fatalf("exclusive create error=%v cleanup starts=%d", err, cleanupStarts)
	}
	got, readErr := os.ReadFile(part)
	if readErr != nil || string(got) != "existing partial" {
		t.Fatalf("unowned partial changed: %q err=%v", got, readErr)
	}
}

func TestWSLStageCleanupFailureDoesNotChangeOriginalAuthority(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	stageRoot := filepath.Join(root, ".crabbox", "wsl-stage")
	if err := os.MkdirAll(stageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("e", 32)
	oldUpload, oldCleanup := startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem
	t.Cleanup(func() {
		startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem = oldUpload, oldCleanup
	})
	startWSLStageUploadSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		part := filepath.Join(stageRoot, nonce+".part")
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn {
			return stallAfterStageWriteConn{Conn: conn, ctx: ctx, path: part}
		})
	}
	cleanupFailure := errors.New("cleanup transport unavailable")
	startWSLStageCleanupSubsystem = func(context.Context, SSHTarget, string, string, string, io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		return nil, nil, nil, cleanupFailure
	}
	spool, _ := newTestWSLStageSpool(t, bytes.Repeat([]byte("x"), 2<<20))
	var stderr bytes.Buffer
	err := spool.upload(t.Context(), SSHTarget{Port: "22"}, nonce, 20*time.Millisecond, "10", "3", &stderr)
	var retryable retryableWSLStageError
	var nonRetryable nonRetryableWSLStageError
	if !errors.As(err, &retryable) || !errors.As(err, &nonRetryable) || !errors.Is(err, cleanupFailure) {
		t.Fatalf("cleanup failure did not preserve the cause and prohibit replay: %v", err)
	}
	if !strings.Contains(stderr.String(), "partial cleanup failed") || !strings.Contains(stderr.String(), cleanupFailure.Error()) {
		t.Fatalf("missing bounded cleanup diagnostic: %q", stderr.String())
	}
}

func TestCleanupWSLStageReadyRejectsInvalidNonceWithoutOpeningTransport(t *testing.T) {
	spool, _ := newTestWSLStageSpool(t, nil)
	oldCleanup := startWSLStageCleanupSubsystem
	t.Cleanup(func() { startWSLStageCleanupSubsystem = oldCleanup })
	startWSLStageCleanupSubsystem = func(context.Context, SSHTarget, string, string, string, io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		t.Fatal("invalid nonce opened a cleanup transport")
		return nil, nil, nil, nil
	}
	err := cleanupWSLStageReady(t.Context(), SSHTarget{Port: "22"}, "../../../secret", spool, time.Second, time.Second, "10")
	if err == nil || !strings.Contains(err.Error(), "invalid WSL2 stage cleanup identity") {
		t.Fatalf("invalid nonce error=%v", err)
	}
}

func TestWSLStageUploadWaitFailureCleansPublishedReady(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	nonce := strings.Repeat("f", 32)
	spool, _ := newTestWSLStageSpool(t, []byte("sensitive published payload"))
	oldUpload, oldCleanup := startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem
	t.Cleanup(func() { startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem = oldUpload, oldCleanup })
	waitFailure := errors.New("SFTP session completion failed")
	startWSLStageUploadSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		output, input, wait, err := startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
		return output, input, func() error { return errors.Join(wait(), waitFailure) }, err
	}
	cleanupCalls := 0
	startWSLStageCleanupSubsystem = func(ctx context.Context, target SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		cleanupCalls++
		if target.Port != "2299" || len(target.FallbackPorts) != 0 {
			t.Fatalf("published cleanup escaped its pinned upload route: %+v", target)
		}
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
	}
	err := spool.upload(t.Context(), SSHTarget{Port: "2299"}, nonce, time.Second, "10", "3", io.Discard)
	if !errors.Is(err, waitFailure) || cleanupCalls != 0 {
		t.Fatalf("published completion error=%v cleanup calls=%d", err, cleanupCalls)
	}
	for _, suffix := range []string{".part", ".ready"} {
		if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(wslStageRoot), nonce+suffix)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("published failure left %s residue: %v", suffix, statErr)
		}
	}
}

func TestWSLStageAmbiguousRenameCleansBothOwnedCandidates(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		name := "rename did not reach server"
		if renamed {
			name = "rename committed before response loss"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			installWSLStageDiscardFixture(t, root)
			nonce := strings.Repeat("9", 32)
			spool, _ := newTestWSLStageSpool(t, []byte("private ambiguous publication"))
			oldUpload, oldCleanup, oldPublish := startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem, publishWSLStage
			t.Cleanup(func() {
				startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem, publishWSLStage = oldUpload, oldCleanup, oldPublish
			})
			startWSLStageUploadSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
			}
			publishWSLStage = func(client *sftp.Client, part, ready string) error {
				if renamed {
					if err := client.Rename(part, ready); err != nil {
						return err
					}
				}
				return io.ErrUnexpectedEOF
			}
			cleanups := 0
			startWSLStageCleanupSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				cleanups++
				return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
			}
			err := spool.upload(t.Context(), SSHTarget{Port: "22"}, nonce, time.Second, "10", "3", io.Discard)
			var nonRetryable nonRetryableWSLStageError
			if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.As(err, &nonRetryable) || cleanups != 1 {
				t.Fatalf("ambiguous publish error=%v cleanup sessions=%d", err, cleanups)
			}
			for _, suffix := range []string{".part", ".ready"} {
				if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(wslStageRoot), nonce+suffix)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("ambiguous publication left %s residue: %v", suffix, statErr)
				}
			}
		})
	}
}

func TestWSLStageRunCleansPublishedReadyBeforeExecution(t *testing.T) {
	for _, test := range []struct {
		name           string
		reserve        bool
		launcherLength int
		cancel         bool
	}{
		{name: "execution reserve exhausted", reserve: true},
		{name: "empty launcher command", launcherLength: -1},
		{name: "launcher command at Windows limit", launcherLength: wslStageLauncherCommandLimit},
		{name: "launcher command exceeds Windows limit", launcherLength: wslStageLauncherCommandLimit + 1},
		{name: "context canceled after publication", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			installWSLStageDiscardFixture(t, root)
			spool, data := newTestWSLStageSpool(t, []byte("private-owner-payload"))
			nonce := strings.Repeat("e", 32)
			ready := filepath.Join(root, filepath.FromSlash(wslStageRoot), nonce+".ready")
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			if test.reserve {
				spool.timing.reserve = 100 * time.Millisecond
			}
			oldStage, oldCleanup, oldLauncher := stageWSLSpool, startWSLStageCleanupSubsystem, buildWSLStageLauncher
			t.Cleanup(func() {
				stageWSLSpool, startWSLStageCleanupSubsystem, buildWSLStageLauncher = oldStage, oldCleanup, oldLauncher
			})
			stageWSLSpool = func(spool *wslStageSpool, _ context.Context, target *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
				spool.shell = wslStageCMD
				if err := os.MkdirAll(filepath.Dir(ready), 0o700); err != nil {
					return "", err
				}
				if err := os.WriteFile(ready, data, 0o600); err != nil {
					return "", err
				}
				target.Port, target.FallbackPorts = "2299", []string{}
				if test.reserve {
					time.Sleep(70 * time.Millisecond)
				}
				if test.cancel {
					cancel()
				}
				return nonce, nil
			}
			if test.launcherLength != 0 {
				buildWSLStageLauncher = func(string, int64, [sha256.Size]byte, wslStageShell) string {
					return strings.Repeat("x", max(0, test.launcherLength))
				}
			}
			cleanupCalls := 0
			startWSLStageCleanupSubsystem = func(cleanupCtx context.Context, target SSHTarget, _, attempts, subsystem string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				cleanupCalls++
				if target.Port != "2299" || len(target.FallbackPorts) != 0 || attempts != "1" || subsystem != "sftp" {
					t.Fatalf("cleanup escaped published route: target=%+v attempts=%q subsystem=%q", target, attempts, subsystem)
				}
				return startLoopbackWSLSFTPSubsystem(cleanupCtx, root, func(conn net.Conn) net.Conn { return conn })
			}
			err := spool.run(ctx, &SSHTarget{Port: "2222"}, "10", "3", io.Discard, io.Discard)
			if err == nil || cleanupCalls != 0 {
				t.Fatalf("published stage cleanup error=%v cleanup calls=%d", err, cleanupCalls)
			}
			if _, statErr := os.Stat(ready); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("private ready stage survived local abort: %v", statErr)
			}
		})
	}
}

func TestWSLStageRunJoinsCleanupFailureWithoutClaimingRelease(t *testing.T) {
	spool, _ := newTestWSLStageSpool(t, []byte("private stage"))
	oldStage, oldCleanup, oldLauncher := stageWSLSpool, cleanupPublishedWSLStage, buildWSLStageLauncher
	t.Cleanup(func() {
		stageWSLSpool, cleanupPublishedWSLStage, buildWSLStageLauncher = oldStage, oldCleanup, oldLauncher
	})
	nonce := strings.Repeat("f", 32)
	stageWSLSpool = func(spool *wslStageSpool, _ context.Context, _ *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
		spool.shell = wslStageCMD
		return nonce, nil
	}
	buildWSLStageLauncher = func(string, int64, [sha256.Size]byte, wslStageShell) string {
		return strings.Repeat("x", wslStageLauncherCommandLimit)
	}
	cleanupFailure := errors.New("ready stage ownership remains unresolved")
	cleanupPublishedWSLStage = func(_ context.Context, _ SSHTarget, got string, _ *wslStageSpool, _, _ time.Duration, _ string) error {
		if got != nonce {
			t.Fatalf("cleanup escaped owned publication: %q", got)
		}
		return cleanupFailure
	}
	err := spool.run(t.Context(), &SSHTarget{Port: "22"}, "10", "3", io.Discard, io.Discard)
	if !errors.Is(err, cleanupFailure) || !strings.Contains(err.Error(), "launcher exceeds its command budget") ||
		!strings.Contains(err.Error(), "ready stage cleanup failed") {
		t.Fatalf("cleanup failure was lost or changed original failure authority: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestUploadToSFTPCollisionPreservesForeignReady(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	client := newLoopbackSFTPClient(t, root)
	spool, _ := newTestWSLStageSpool(t, []byte("collision"))
	nonce := strings.Repeat("c", 32)
	stageRoot := filepath.Join(root, ".crabbox", "wsl-stage")
	if err := os.MkdirAll(stageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageRoot, nonce+".ready"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	_, err := uploadToSFTP(client, spool, nonce, time.Second, cancel)
	if err == nil || !strings.Contains(err.Error(), "nonce collision") {
		t.Fatalf("collision error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".crabbox", "wsl-stage", nonce+".part")); err != nil {
		t.Fatalf("SFTP removed partial on collision: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".crabbox", "wsl-stage", nonce+".ready"))
	if err != nil || string(got) != "existing" {
		t.Fatalf("existing ready stage changed: %q err=%v", got, err)
	}
}

func TestWSLStagePinsOnlySuccessfulRetryableFallback(t *testing.T) {
	stubWSLStageRoutePreparation(t, func(context.Context, SSHTarget, string, string) error { return nil })
	spool, expected := newTestWSLStageSpool(t, nil)
	oldUpload := uploadWSLSpool
	t.Cleanup(func() { uploadWSLSpool = oldUpload })
	target := SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
	var ports, nonces []string
	timing := wslStageTiming{stage: time.Second, idle: time.Second}
	var exit255 *exec.Cmd
	if runtime.GOOS == "windows" {
		exit255 = exec.Command("cmd.exe", "/c", "exit 255")
	} else {
		exit255 = exec.Command("sh", "-c", "exit 255")
	}
	waitErr := exit255.Run()
	if waitErr == nil || exitCode(waitErr) != 255 {
		t.Fatalf("exit 255 fixture=%v", waitErr)
	}
	uploadWSLSpool = func(_ *wslStageSpool, ctx context.Context, candidate SSHTarget, nonce string, _ time.Duration, _, _ string, _ io.Writer) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > timing.stage {
			t.Fatalf("stage attempt deadline=%v ok=%t", deadline, ok)
		}
		ports, nonces = append(ports, candidate.Port), append(nonces, nonce)
		reader, err := spool.input.reset()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(data, expected) || sha256.Sum256(data) != spool.digest() {
			t.Fatal("candidate retry changed the private blinding/envelope")
		}
		if candidate.Port == "2222" {
			return finishWSLStageAttempt(ctx, sftp.ErrSSHFxConnectionLost, nil, waitErr)
		}
		return nil
	}
	if _, err := spool.stage(t.Context(), &target, timing, "10", "3", io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ports, ",") != "2222,22" || len(nonces) != 2 || nonces[0] == nonces[1] ||
		target.Port != "22" || len(target.FallbackPorts) != 0 || !target.NoControlMaster {
		t.Fatalf("ports=%v nonces=%v target=%+v", ports, nonces, target)
	}

	target = SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
	ports = nil
	uploadWSLSpool = func(_ *wslStageSpool, ctx context.Context, candidate SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
		ports = append(ports, candidate.Port)
		return finishWSLStageAttempt(ctx, &sftp.StatusError{Code: 3}, nil, nil)
	}
	if _, err := spool.stage(t.Context(), &target, timing, "10", "3", io.Discard); err == nil {
		t.Fatal("non-retryable stage failure succeeded")
	}
	if len(ports) != 1 || target.Port != "2222" || len(target.FallbackPorts) != 1 {
		t.Fatalf("non-retryable ports=%v target=%+v", ports, target)
	}

	target = SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
	ports = nil
	uploadWSLSpool = func(_ *wslStageSpool, _ context.Context, candidate SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
		ports = append(ports, candidate.Port)
		return nonRetryableWSLStageError{retryableWSLStageError{io.ErrUnexpectedEOF}}
	}
	if _, err := spool.stage(t.Context(), &target, timing, "10", "3", io.Discard); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("publication error lost its original transport cause: %v", err)
	}
	if len(ports) != 1 || target.Port != "2222" || len(target.FallbackPorts) != 1 {
		t.Fatalf("published stage escaped its original route: ports=%v target=%+v", ports, target)
	}
}

func TestWSLStagePreparesExactRouteBeforeEachUpload(t *testing.T) {
	for _, test := range []struct {
		name      string
		fallbacks []string
		want      string
	}{
		{"singleton", []string{}, "prepare:2222,upload:2222"},
		{"duplicates only", []string{"2222", "2222"}, "prepare:2222,upload:2222"},
		{"distinct fallbacks", []string{"2222", "22", "22"}, "probe:2222,prepare:2222,upload:2222,probe:22,prepare:22,upload:22"},
	} {
		t.Run(test.name, func(t *testing.T) {
			spool, _ := newTestWSLStageSpool(t, nil)
			oldUpload := uploadWSLSpool
			t.Cleanup(func() { uploadWSLSpool = oldUpload })
			var events []string
			var probeDeadline time.Time
			stubWSLStageRoutePreparation(t, func(ctx context.Context, target SSHTarget, connectTimeout, attempts string) error {
				if len(target.FallbackPorts) != 0 || !target.NoControlMaster || connectTimeout != "10" || attempts != "3" {
					t.Fatalf("ACL prep escaped the pinned candidate: target=%+v timeout=%s attempts=%s", target, connectTimeout, attempts)
				}
				if deadline, _ := ctx.Deadline(); !probeDeadline.IsZero() && !deadline.Equal(probeDeadline) {
					t.Fatal("nonce preparation received a new budget after the probe")
				}
				events = append(events, "prepare:"+target.Port)
				return nil
			})
			probeWSLStageRoute = func(ctx context.Context, target SSHTarget, _, _ string) error {
				if ctx.Value(wslStageRouteProofKey{}) != nil || len(target.FallbackPorts) != 0 || !target.NoControlMaster {
					t.Fatal("reachability probe acquired proof authority or escaped its candidate")
				}
				probeDeadline, _ = ctx.Deadline()
				events = append(events, "probe:"+target.Port)
				return nil
			}
			uploadWSLSpool = func(_ *wslStageSpool, _ context.Context, target SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
				events = append(events, "upload:"+target.Port)
				if test.name == "distinct fallbacks" && target.Port == "2222" {
					return retryableWSLStageError{errors.New("primary upload unavailable")}
				}
				return nil
			}
			target := SSHTarget{Port: "2222", FallbackPorts: test.fallbacks}
			if _, err := spool.stage(t.Context(), &target, wslStageTiming{stage: time.Second, idle: time.Second}, "10", "3", io.Discard); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(events, ","); got != test.want || len(target.FallbackPorts) != 0 {
				t.Fatalf("probe/ACL/upload route ordering=%q final target=%+v", got, target)
			}
		})
	}
}

func TestWSLStageUnreachablePrimaryFallsBackWithoutProofCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH recording fixture")
	}
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, refusedPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ssh := installWorkspaceOwnerRecordingSSH(t)
	t.Setenv("CRABBOX_OWNER_SSH_REAL_PORT", refusedPort)
	t.Setenv("CRABBOX_OWNER_SSH_REAL_EXECUTABLE", realSSH)
	root := t.TempDir()
	stageRoot := filepath.Join(root, filepath.FromSlash(wslStageRoot))
	if err := os.MkdirAll(stageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPrepare, oldStart, oldDiscard := prepareWSLStageRoute, startWSLStageUploadSubsystem, discardWSLStageFile
	t.Cleanup(func() {
		prepareWSLStageRoute, startWSLStageUploadSubsystem, discardWSLStageFile = oldPrepare, oldStart, oldDiscard
	})
	var prepared, uploaded, cleaned []string
	var proof string
	prepareWSLStageRoute = func(ctx context.Context, target SSHTarget, connect, attempts string) (wslStageShell, error) {
		prepared = append(prepared, target.Port)
		if target.Port == refusedPort {
			return oldPrepare(ctx, target, connect, attempts)
		}
		proof = ctx.Value(wslStageRouteProofKey{}).(string)
		return wslStageCMD, os.WriteFile(filepath.Join(stageRoot, "."+proof+".proof"), []byte(proof), 0o600)
	}
	installWSLStageDiscardFixture(t, root)
	fixtureDiscard := discardWSLStageFile
	discardWSLStageFile = func(ctx context.Context, target SSHTarget, nonce, name string, size int64, digest [sha256.Size]byte, connect string) error {
		cleaned = append(cleaned, target.Port)
		if target.Port == refusedPort {
			// A real failed connection cannot magically succeed at proof cleanup.
			return oldDiscard(ctx, target, nonce, name, size, digest, connect)
		}
		return fixtureDiscard(ctx, target, nonce, name, size, digest, connect)
	}
	startWSLStageUploadSubsystem = func(ctx context.Context, target SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		uploaded = append(uploaded, target.Port)
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
	}
	spool, want := newTestWSLStageSpool(t, []byte{0, 255, 1, 128})
	target := SSHTarget{Host: "127.0.0.1", User: "fixture", Port: refusedPort, FallbackPorts: []string{"22"}, DisableHostKeyChecking: true}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := spool.run(ctx, &target, "1", "1", io.Discard, io.Discard); err != nil {
		t.Fatalf("healthy fallback blocked: prepared=%v uploaded=%v cleaned=%v error=%v", prepared, uploaded, cleaned, err)
	}
	if strings.Join(prepared, ",") != "22" || strings.Join(uploaded, ",") != "22" || strings.Join(cleaned, ",") != "22" ||
		target.Port != "22" || len(target.FallbackPorts) != 0 || !target.NoControlMaster {
		t.Fatalf("route delivery: prepared=%v uploaded=%v cleaned=%v target=%+v", prepared, uploaded, cleaned, target)
	}
	for i, port := range []string{refusedPort, "22", "22"} {
		args := strings.Join(readSSHArgsRecorder(t, filepath.Join(ssh, fmt.Sprintf("%d.args", i+1))), "\n") + "\n"
		if !strings.Contains(args, "-p\n"+port+"\n") || !strings.Contains(args, "-n\n") || !strings.Contains(args, "ControlMaster=no") {
			t.Fatalf("call %d escaped pinned no-input route: %q", i+1, args)
		}
		command, input := readWorkspaceOwnerSSHCall(t, ssh, i+1)
		wantCommand := sshTransportProbeCommand(target)
		if i == 2 {
			wantCommand = wslStageLauncherCommand(proof, spool.size, spool.digest(), wslStageCMD)
		}
		if command != wantCommand || input != "" {
			t.Fatalf("call %d was not the exact zero-input probe/submission", i+1)
		}
	}
	if got, err := os.ReadFile(filepath.Join(ssh, "count")); err != nil || string(got) != "3" {
		t.Fatalf("unexpected SSH replay/cleanup: %s calls, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(stageRoot, proof+".ready")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("fallback changed the binary envelope: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stageRoot, "."+proof+".proof")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback proof not cleaned: %v", err)
	}
}

func TestWSLStagePreparationFailureRequiresConfirmedProofCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH recording fixture")
	}
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup_fails=%t", cleanupFails), func(t *testing.T) {
			installWorkspaceOwnerRecordingSSH(t)
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_FIRST_DELIVERY", "1")
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_CODE", "255")
			spool, _ := newTestWSLStageSpool(t, nil)
			oldUpload, oldPrepare := uploadWSLSpool, prepareWSLStageRoute
			t.Cleanup(func() { uploadWSLSpool = oldUpload })
			var events []string
			stubWSLStageRoutePreparation(t, func(ctx context.Context, target SSHTarget, connect, attempts string) error {
				events = append(events, "prepare:"+target.Port)
				if target.Port == "2222" {
					_, err := oldPrepare(ctx, target, connect, attempts)
					if !shouldRetrySSHPort(err) {
						t.Fatalf("fixture did not lose the preparation response with exit255: %v", err)
					}
					return err
				}
				return nil
			})
			probeWSLStageRoute = func(ctx context.Context, target SSHTarget, connect, attempts string) error {
				events = append(events, "probe:"+target.Port)
				return probeWSLStageTransport(ctx, target, connect, attempts)
			}
			discardWSLStageFile = func(_ context.Context, target SSHTarget, nonce, name string, size int64, digest [sha256.Size]byte, _ string) error {
				events = append(events, "cleanup:"+target.Port)
				if name != "."+nonce+".proof" || size != 32 || digest != sha256.Sum256([]byte(nonce)) {
					t.Fatal("cleanup lost exact proof identity")
				}
				if cleanupFails {
					return io.EOF
				}
				return nil
			}
			uploadWSLSpool = func(_ *wslStageSpool, _ context.Context, target SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
				events = append(events, "upload:"+target.Port)
				return nil
			}
			target := SSHTarget{Host: "example.test", Port: "2222", FallbackPorts: []string{"22"}}
			_, err := spool.stage(t.Context(), &target, wslStageTiming{stage: time.Second, idle: time.Second}, "10", "3", io.Discard)
			want := "probe:2222,prepare:2222,cleanup:2222"
			if cleanupFails {
				var terminal nonRetryableWSLStageError
				if !errors.As(err, &terminal) || !errors.Is(err, io.EOF) || target.Port != "2222" || len(target.FallbackPorts) != 1 {
					t.Fatalf("ambiguous proof delivery escaped its route: target=%+v err=%v", target, err)
				}
			} else {
				want += ",probe:22,prepare:22,upload:22,cleanup:22"
				if err != nil || target.Port != "22" {
					t.Fatalf("confirmed cleanup did not permit fallback: %v", err)
				}
			}
			if got := strings.Join(events, ","); got != want {
				t.Fatalf("proof delivery/cleanup ordering=%q want=%q", got, want)
			}
		})
	}
}

func TestWSLStageProbeHonorsPreparationBudgetAndCallerCancellation(t *testing.T) {
	for _, phase := range []string{"preparation deadline", "caller cancellation", "non-retryable probe"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			spool, _ := newTestWSLStageSpool(t, nil)
			probeFailure := errors.New("probe rejected")
			var events []string
			stubWSLStageRoutePreparation(t, func(_ context.Context, target SSHTarget, _, _ string) error {
				events = append(events, "prepare:"+target.Port)
				return nil
			})
			probeWSLStageRoute = func(ctx context.Context, target SSHTarget, _, _ string) error {
				events = append(events, "probe:"+target.Port)
				if target.Port == "22" {
					return nil
				}
				if phase == "caller cancellation" {
					cancel()
					return nil
				}
				if phase == "non-retryable probe" {
					return probeFailure
				}
				<-ctx.Done()
				return ctx.Err()
			}
			discardWSLStageFile = func(_ context.Context, target SSHTarget, _, _ string, _ int64, _ [sha256.Size]byte, _ string) error {
				events = append(events, "cleanup:"+target.Port)
				return nil
			}
			oldUpload := uploadWSLSpool
			t.Cleanup(func() { uploadWSLSpool = oldUpload })
			uploadWSLSpool = func(_ *wslStageSpool, _ context.Context, target SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
				events = append(events, "upload:"+target.Port)
				return nil
			}
			target := SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
			timing := wslStageTiming{prepare: 20 * time.Millisecond, stage: time.Second, idle: time.Second}
			_, err := spool.stage(ctx, &target, timing, "10", "3", io.Discard)
			want := "probe:2222"
			if phase == "caller cancellation" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("caller cancellation lost: %v", err)
				}
			} else if phase == "non-retryable probe" {
				if !errors.Is(err, probeFailure) {
					t.Fatalf("non-retryable probe failure lost: %v", err)
				}
			} else {
				want += ",probe:22,prepare:22,upload:22,cleanup:22"
				if err != nil {
					t.Fatalf("probe expiry prevented independent fallback: %v", err)
				}
			}
			if got := strings.Join(events, ","); got != want {
				t.Fatalf("probe expiry/cancellation ordering=%q want=%q", got, want)
			}
		})
	}
}

func TestWSLStageFallbackRetainsCompleteRouteAndExecutionReserve(t *testing.T) {
	stubWSLStageRoutePreparation(t, func(context.Context, SSHTarget, string, string) error { return nil })
	spool, _ := newTestWSLStageSpool(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	target := SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
	timing := wslStageTiming{stage: 250 * time.Millisecond, idle: time.Second, reserve: 200 * time.Millisecond}
	oldUpload := uploadWSLSpool
	t.Cleanup(func() { uploadWSLSpool = oldUpload })
	var fallbackBudget time.Duration
	uploadWSLSpool = func(_ *wslStageSpool, attemptCtx context.Context, candidate SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
		if candidate.Port == "2222" {
			time.Sleep(90 * time.Millisecond)
			return retryableWSLStageError{errors.New("primary WAN route unavailable")}
		}
		deadline, ok := attemptCtx.Deadline()
		if !ok {
			t.Fatal("fallback SFTP route has no deadline")
		}
		fallbackBudget = time.Until(deadline)
		return nil
	}
	if _, err := spool.stage(ctx, &target, timing, "10", "3", io.Discard); err != nil {
		t.Fatal(err)
	}
	if fallbackBudget < 100*time.Millisecond || target.Port != "22" {
		t.Fatalf("fallback route budget=%s target port=%s", fallbackBudget, target.Port)
	}
	if deadline, _ := ctx.Deadline(); time.Until(deadline) <= timing.reserve {
		t.Fatal("fallback route consumed the independent staged execution reserve")
	}
}

func TestWSLStageOperationClassificationControlsFallback(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation error
		retryable bool
	}{
		{name: "closed pipe", operation: io.ErrClosedPipe, retryable: true},
		{name: "wrapped closed pipe", operation: fmt.Errorf("subsystem write: %w", io.ErrClosedPipe), retryable: true},
		{name: "packet header", operation: fmt.Errorf("failed to send packet: %w", io.ErrClosedPipe), retryable: true},
		{name: "packet payload", operation: fmt.Errorf("failed to send packet payload: %w", io.ErrClosedPipe), retryable: true},
		{name: "eof", operation: io.EOF, retryable: true},
		{name: "connection lost", operation: sftp.ErrSSHFxConnectionLost, retryable: true},
		{name: "storage permission", operation: os.ErrPermission},
		{name: "permission status", operation: &sftp.StatusError{Code: 3}},
		{name: "storage failure status", operation: &sftp.StatusError{Code: 4}},
		{name: "metadata mismatch", operation: wslStageOperationError("verify WSL2 stage metadata failed", nil)},
		{name: "nonce collision", operation: wslStageOperationError("publish WSL2 stage failed: nonce collision", nil)},
		{name: "publication ambiguity", operation: nonRetryableWSLStageError{retryableWSLStageError{io.ErrClosedPipe}}},
		{name: "diagnostic text only", operation: errors.New("io: read/write on closed pipe")},
	} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/candidate_canceled=%t", test.name, canceled), func(t *testing.T) {
				stubWSLStageRoutePreparation(t, func(context.Context, SSHTarget, string, string) error { return nil })
				spool, _ := newTestWSLStageSpool(t, nil)
				oldUpload := uploadWSLSpool
				t.Cleanup(func() { uploadWSLSpool = oldUpload })
				routes := 0
				uploadWSLSpool = func(_ *wslStageSpool, ctx context.Context, _ SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
					routes++
					if routes > 1 {
						return nil
					}
					ctx, cancel := context.WithCancelCause(ctx)
					defer cancel(nil)
					if canceled {
						cancel(retryableWSLStageError{context.DeadlineExceeded})
					}
					operation := wslStageOperationError("upload WSL2 stage failed", test.operation)
					err := finishWSLStageAttempt(ctx, operation, io.ErrClosedPipe, sftp.ErrSSHFxConnectionLost)
					var terminal nonRetryableWSLStageError
					if errors.As(err, &terminal) == test.retryable || !errors.Is(err, test.operation) {
						t.Fatalf("operation authority changed: %v", err)
					}
					return err
				}
				target := SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
				_, err := spool.stage(t.Context(), &target, wslStageTiming{stage: time.Second, idle: time.Second}, "10", "3", io.Discard)
				if test.retryable {
					if err != nil || routes != 2 || target.Port != "22" {
						t.Fatalf("transport failure did not fall back: routes=%d target=%+v err=%v", routes, target, err)
					}
				} else if err == nil || routes != 1 || target.Port != "2222" || len(target.FallbackPorts) != 1 {
					t.Fatalf("terminal operation fell back: routes=%d target=%+v err=%v", routes, target, err)
				}
			})
		}
	}
}

func TestWSLStageClosedPipePacketFailureCleansBeforeFallback(t *testing.T) {
	for _, test := range []struct {
		name         string
		payload      bool
		cancel       bool
		cleanupFails bool
		publication  bool
	}{
		{name: "header transport teardown"},
		{name: "payload transport teardown", payload: true},
		{name: "header candidate cancellation", cancel: true},
		{name: "payload candidate cancellation", payload: true, cancel: true},
		{name: "failed cleanup forbids fallback", payload: true, cancel: true, cleanupFails: true},
		{name: "ambiguous publication forbids fallback", cancel: true, publication: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			installWSLStageDiscardFixture(t, root)
			stageRoot := filepath.Join(root, filepath.FromSlash(wslStageRoot))
			if err := os.MkdirAll(stageRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			spool, expected := newTestWSLStageSpool(t, []byte("publish only on the successful route"))
			var firstNonce string
			var events []string
			stubWSLStageRoutePreparation(t, func(ctx context.Context, target SSHTarget, _, _ string) error {
				nonce := ctx.Value(wslStageRouteProofKey{}).(string)
				if firstNonce == "" {
					firstNonce = nonce
				} else if _, err := os.Stat(filepath.Join(stageRoot, firstNonce+".part")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fallback overtook exact partial cleanup: %v", err)
				}
				events = append(events, "prepare:"+target.Port)
				return os.WriteFile(filepath.Join(stageRoot, "."+nonce+".proof"), []byte(nonce), 0o600)
			})
			installWSLStageDiscardFixture(t, root)
			oldUpload, oldStart, oldCleanup := uploadWSLSpool, startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem
			t.Cleanup(func() {
				uploadWSLSpool, startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem = oldUpload, oldStart, oldCleanup
			})
			oldPublish := publishWSLStage
			t.Cleanup(func() { publishWSLStage = oldPublish })
			var cancelAttempt context.CancelCauseFunc
			publications := 0
			publishWSLStage = func(client *sftp.Client, part, ready string) error {
				publications++
				if err := oldPublish(client, part, ready); err != nil {
					return err
				}
				if test.publication {
					cancelAttempt(retryableWSLStageError{context.DeadlineExceeded})
					return fmt.Errorf("failed to send packet: %w", io.ErrClosedPipe)
				}
				return nil
			}
			uploadWSLSpool = func(s *wslStageSpool, ctx context.Context, target SSHTarget, nonce string, idle time.Duration, connectTimeout, attempts string, stderr io.Writer) error {
				ctx, cancel := context.WithCancelCause(ctx)
				defer cancel(nil)
				cancelAttempt = cancel
				err := oldUpload(s, ctx, target, nonce, idle, connectTimeout, attempts, stderr)
				if target.Port == "2222" && (!errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.DeadlineExceeded) != test.cancel) {
					t.Fatalf("fixture lost the packet write/cancellation cause: %v", err)
				}
				return err
			}
			startWSLStageUploadSubsystem = func(_ context.Context, target SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				// Hold the receive side open until client.Close so sendPacket's wrapped
				// error wins deterministically over SFTP's connection-loss broadcast.
				output, input, wait, err := startLoopbackWSLSFTPSubsystem(t.Context(), root, func(conn net.Conn) net.Conn { return conn })
				if target.Port == "2222" && err == nil && !test.publication {
					transport := input
					payload, failed := false, false
					input = struct {
						io.Closer
						io.Writer
					}{transport, writerFunc(func(p []byte) (int, error) {
						writeHeader := len(p) > 4 && p[4] == 6 // SSH_FXP_WRITE
						if failed || payload || writeHeader && !test.payload {
							failed = true
							if test.cancel {
								cancelAttempt(retryableWSLStageError{context.DeadlineExceeded})
							}
							return 0, io.ErrClosedPipe
						}
						payload = writeHeader
						return transport.Write(p)
					})}
				}
				return output, input, func() error {
					err := wait()
					events = append(events, "wait:"+target.Port)
					return err
				}, err
			}
			startWSLStageCleanupSubsystem = func(ctx context.Context, target SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				events = append(events, "cleanup:"+target.Port)
				if test.cleanupFails {
					return nil, nil, nil, os.ErrPermission
				}
				return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
			}
			target := SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
			nonce, err := spool.stage(t.Context(), &target, wslStageTiming{stage: time.Second, idle: time.Second}, "10", "3", io.Discard)
			want := "prepare:2222,wait:2222,cleanup:2222"
			if test.cleanupFails {
				var terminal nonRetryableWSLStageError
				if !errors.As(err, &terminal) || !errors.Is(err, os.ErrPermission) || target.Port != "2222" {
					t.Fatalf("failed cleanup allowed replay: %v target=%+v", err, target)
				}
			} else if test.publication {
				want += ""
				var terminal nonRetryableWSLStageError
				if !errors.As(err, &terminal) || target.Port != "2222" {
					t.Fatalf("ambiguous publication allowed replay: %v target=%+v", err, target)
				}
				for _, suffix := range []string{".part", ".ready"} {
					if _, err := os.Stat(filepath.Join(stageRoot, firstNonce+suffix)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("ambiguous publication left %s residue: %v", suffix, err)
					}
				}
			} else {
				want += ",prepare:22,wait:22"
				data, readErr := os.ReadFile(filepath.Join(stageRoot, nonce+".ready"))
				if err != nil || readErr != nil || target.Port != "22" || !bytes.Equal(data, expected) {
					t.Fatalf("fallback did not publish the exact payload: err=%v read=%v target=%+v", err, readErr, target)
				}
			}
			if strings.Join(events, ",") != want {
				t.Fatalf("cleanup/fallback ordering=%v want=%s", events, want)
			}
			wantPublications := 1
			if test.cleanupFails {
				wantPublications = 0
			}
			if publications != wantPublications {
				t.Fatalf("publications=%d want=%d", publications, wantPublications)
			}
		})
	}
}

func TestWSLStageProgressingCandidateTimeoutReservesFallback(t *testing.T) {
	for _, test := range []struct {
		name          string
		parentTimeout time.Duration
		fallbacks     []string
		wantRoutes    int
	}{
		{name: "complete fallback", fallbacks: []string{"22"}, wantRoutes: 2},
		{name: "multiple fallbacks", fallbacks: []string{"2200", "22", "2200"}, wantRoutes: 3},
		{name: "caller expiry", parentTimeout: 200 * time.Millisecond, fallbacks: []string{"22"}, wantRoutes: 1},
	} {
		run := func(t *testing.T) {
			root := t.TempDir()
			installWSLStageDiscardFixture(t, root)
			stageRoot := filepath.Join(root, filepath.FromSlash(wslStageRoot))
			if err := os.MkdirAll(stageRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			spool, expected := newTestWSLStageSpool(t, bytes.Repeat([]byte("x"), 768<<10))
			timing := wslStageTiming{stage: 400 * time.Millisecond, prepare: 100 * time.Millisecond, cleanup: 200 * time.Millisecond, idle: 200 * time.Millisecond}
			var events []string
			var previousNonce string
			stubWSLStageRoutePreparation(t, func(ctx context.Context, target SSHTarget, _, _ string) error {
				if previousNonce != "" {
					if _, err := os.Stat(filepath.Join(stageRoot, previousNonce+".part")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("fallback overtook exact partial cleanup: %v", err)
					}
				}
				nonce, _ := ctx.Value(wslStageRouteProofKey{}).(string)
				previousNonce = nonce
				events = append(events, "prepare:"+target.Port)
				return os.WriteFile(filepath.Join(stageRoot, "."+nonce+".proof"), []byte(nonce), 0o600)
			})
			installWSLStageDiscardFixture(t, root)
			oldUpload, oldCleanup, oldPublish := startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem, publishWSLStage
			t.Cleanup(func() {
				startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem, publishWSLStage = oldUpload, oldCleanup, oldPublish
			})
			routes, publications := 0, 0
			startWSLStageUploadSubsystem = func(ctx context.Context, target SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				routes++
				deadline, _ := ctx.Deadline()
				if test.parentTimeout == 0 && time.Until(deadline) < timing.stage-50*time.Millisecond {
					t.Fatalf("route %s lost its full transfer allocation", target.Port)
				}
				events = append(events, "upload:"+target.Port)
				output, input, wait, err := startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn {
					if target.Port != "22" {
						return delayedWriteConn{Conn: conn, ctx: ctx, delay: 20 * time.Millisecond}
					}
					return conn
				})
				return output, input, func() error {
					err := wait()
					if target.Port != "22" && test.parentTimeout == 0 {
						info, statErr := os.Stat(filepath.Join(stageRoot, previousNonce+".part"))
						if statErr != nil || info.Size() <= 32<<10 || info.Size() >= spool.size || !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
							t.Errorf("primary did not progress until its allocation expired: info=%v err=%v cause=%v", info, statErr, context.Cause(ctx))
						}
					}
					events = append(events, "wait:"+target.Port)
					return err
				}, err
			}
			startWSLStageCleanupSubsystem = func(ctx context.Context, target SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				events = append(events, "cleanup:"+target.Port)
				output, input, wait, err := startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
				return output, input, func() error {
					err := wait()
					events = append(events, "cleaned:"+target.Port)
					return err
				}, err
			}
			publishWSLStage = func(client *sftp.Client, old, next string) error {
				publications++
				return oldPublish(client, old, next)
			}
			ctx := t.Context()
			if test.parentTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.parentTimeout)
				defer cancel()
			}
			target := SSHTarget{Port: "2222", FallbackPorts: test.fallbacks}
			nonce, err := spool.stage(ctx, &target, timing, "10", "3", io.Discard)
			if routes != test.wantRoutes {
				t.Fatalf("routes=%d events=%v err=%v", routes, events, err)
			}
			if test.parentTimeout > 0 {
				if !errors.Is(err, context.DeadlineExceeded) || publications != 0 {
					t.Fatalf("caller expiry allowed publication/retry: %v %v", err, events)
				}
				return
			}
			if err != nil || target.Port != "22" || publications != 1 {
				t.Fatalf("fallback failed or replayed: %v %v publications=%d", err, events, publications)
			}
			data, err := os.ReadFile(filepath.Join(stageRoot, nonce+".ready"))
			if err != nil || !bytes.Equal(data, expected) {
				t.Fatalf("fallback payload changed: %v", err)
			}
			for _, port := range append([]string{"2222"}, test.fallbacks[:len(test.fallbacks)-1]...) {
				if port == "22" {
					continue
				}
				want := "upload:" + port + ",wait:" + port + ",cleanup:" + port + ",cleaned:" + port + ",prepare:"
				if !strings.Contains(strings.Join(events, ","), want) {
					t.Fatalf("cleanup/process ordering changed: %v", events)
				}
			}
		}
		// The fixture uses net.Pipe: only its deliberate response delays should
		// consume transfer budgets, not host scheduling or filesystem latency.
		t.Run(test.name, func(t *testing.T) { synctest.Test(t, run) })
	}
}

func TestWSLStageCandidateTimeoutAfterPublicationNeverFallsBack(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	stageRoot := filepath.Join(root, filepath.FromSlash(wslStageRoot))
	if err := os.MkdirAll(stageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	spool, _ := newTestWSLStageSpool(t, []byte("execute exactly once"))
	stubWSLStageRoutePreparation(t, func(ctx context.Context, _ SSHTarget, _, _ string) error {
		nonce := ctx.Value(wslStageRouteProofKey{}).(string)
		return os.WriteFile(filepath.Join(stageRoot, "."+nonce+".proof"), []byte(nonce), 0o600)
	})
	installWSLStageDiscardFixture(t, root)
	oldUpload, oldCleanup, oldPublish := startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem, publishWSLStage
	t.Cleanup(func() {
		startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem, publishWSLStage = oldUpload, oldCleanup, oldPublish
	})
	var operationCtx context.Context
	routes, publications := 0, 0
	startWSLStageUploadSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		routes++
		operationCtx = ctx
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
	}
	startWSLStageCleanupSubsystem = func(ctx context.Context, target SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		if target.Port != "2222" || len(target.FallbackPorts) != 0 {
			t.Fatalf("cleanup escaped published route: %+v", target)
		}
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return conn })
	}
	publishWSLStage = func(client *sftp.Client, old, next string) error {
		publications++
		if err := oldPublish(client, old, next); err != nil {
			return err
		}
		<-operationCtx.Done()
		return io.ErrUnexpectedEOF
	}
	target := SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
	_, err := spool.stage(t.Context(), &target, wslStageTiming{stage: 200 * time.Millisecond, idle: time.Second}, "10", "3", io.Discard)
	var terminal nonRetryableWSLStageError
	if !errors.As(err, &terminal) || !errors.Is(err, context.DeadlineExceeded) || routes != 1 || publications != 1 {
		t.Fatalf("ambiguous publication replayed: routes=%d publications=%d err=%v", routes, publications, err)
	}
	entries, readErr := os.ReadDir(stageRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("owned publication was not cleaned: %v %v", entries, readErr)
	}
}

func TestFinishWSLStageAttemptPreservesOperationAuthority(t *testing.T) {
	exit255 := exec.Command("sh", "-c", "exit 255").Run()
	if runtime.GOOS == "windows" {
		exit255 = exec.Command("cmd.exe", "/c", "exit 255").Run()
	}
	tests := []struct {
		name      string
		operation error
		retryable bool
	}{
		{name: "status", operation: &sftp.StatusError{Code: 3}},
		{name: "integrity", operation: errors.New("verify WSL2 stage content failed")},
		{name: "collision", operation: errors.New("WSL2 stage nonce collision")},
		{name: "policy", operation: errors.New("prepare WSL2 stage root failed")},
		{name: "unknown", operation: errors.New("unexpected operation failure")},
		{name: "missing subsystem", operation: errors.Join(errWSLSFTPUnavailable, io.ErrUnexpectedEOF)},
		{name: "connection lost", operation: sftp.ErrSSHFxConnectionLost, retryable: true},
		{name: "unexpected eof", operation: io.ErrUnexpectedEOF, retryable: true},
		{name: "wait only", retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := finishWSLStageAttempt(t.Context(), test.operation, nil, exit255)
			var retryable retryableWSLStageError
			if errors.As(err, &retryable) != test.retryable {
				t.Fatalf("error=%v retryable=%t", err, errors.As(err, &retryable))
			}
			if !test.retryable && exitCode(err) == 255 {
				t.Fatalf("teardown exit 255 overrode operation error: %v", err)
			}
			if test.operation != nil && !errors.Is(err, test.operation) {
				t.Fatalf("operation error lost: %v", err)
			}
		})
	}
}

func TestFinishWSLStageAttemptCallerCancellationWins(t *testing.T) {
	exit255 := exec.Command("sh", "-c", "exit 255").Run()
	if runtime.GOOS == "windows" {
		exit255 = exec.Command("cmd.exe", "/c", "exit 255").Run()
	}
	tests := []struct {
		name  string
		ctx   context.Context
		cause error
	}{
		{name: "cancel", ctx: func() context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}(), cause: context.Canceled},
		{name: "deadline", ctx: func() context.Context {
			ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
			return ctx
		}(), cause: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := wslStageOperationError("verify WSL2 stage content failed", sftp.ErrSSHFxConnectionLost)
			err := finishWSLStageAttempt(test.ctx, operation, sftp.ErrSSHFxConnectionLost, exit255)
			var retryable retryableWSLStageError
			if !errors.Is(err, test.cause) || errors.As(err, &retryable) || !strings.Contains(err.Error(), "verify WSL2 stage content failed") {
				t.Fatalf("caller authority error=%v", err)
			}
		})
	}
}

func TestWSLStageParentDeadlineWinsDerivedBudget(t *testing.T) {
	stubWSLStageRoutePreparation(t, func(context.Context, SSHTarget, string, string) error { return nil })
	spool, _ := newTestWSLStageSpool(t, bytes.Repeat([]byte("b"), 2<<20))
	oldUpload := uploadWSLSpool
	t.Cleanup(func() { uploadWSLSpool = oldUpload })
	uploadWSLSpool = func(_ *wslStageSpool, ctx context.Context, _ SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := spool.stage(ctx, &SSHTarget{Port: "22"}, wslStageTiming{stage: 2 * time.Minute, idle: 15 * time.Second}, "10", "3", io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent deadline error=%v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("parent deadline lost to derived budget: %s", elapsed)
	}
}

func TestWSLStagePreservesOwnerExecutionAndCleanupReserve(t *testing.T) {
	spool, _ := newTestWSLStageSpool(t, []byte("private-owner-payload"))
	target := SSHTarget{Port: "22"}
	previousCleanup := cleanupPublishedWSLStage
	cleanupPublishedWSLStage = func(context.Context, SSHTarget, string, *wslStageSpool, time.Duration, time.Duration, string) error {
		return nil
	}
	t.Cleanup(func() { cleanupPublishedWSLStage = previousCleanup })
	for _, test := range []struct {
		name        string
		deadline    time.Duration
		reserve     time.Duration
		consume     time.Duration
		wantStaging bool
		canceled    bool
	}{
		{name: "reject before staging", deadline: 40 * time.Millisecond, reserve: 80 * time.Millisecond},
		{name: "reject after staging consumed reserve", deadline: 120 * time.Millisecond, reserve: 80 * time.Millisecond, consume: 55 * time.Millisecond, wantStaging: true},
		{name: "canceled before staging without reserve", deadline: time.Second, canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), test.deadline)
			defer cancel()
			if test.canceled {
				cancel()
			}
			spool.timing.reserve = test.reserve
			oldStage := stageWSLSpool
			staged := false
			stageWSLSpool = func(spool *wslStageSpool, _ context.Context, _ *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
				spool.shell = wslStageCMD
				staged = true
				time.Sleep(test.consume)
				return strings.Repeat("a", 32), nil
			}
			t.Cleanup(func() { stageWSLSpool = oldStage })
			err := spool.run(ctx, &target, "10", "3", io.Discard, io.Discard)
			wrongError := err == nil
			if test.canceled {
				wrongError = !errors.Is(err, context.Canceled)
			} else if err != nil {
				wrongError = !strings.Contains(err.Error(), "execution and cleanup deadline")
			}
			if wrongError || staged != test.wantStaging {
				t.Fatalf("staged=%t want=%t error=%v", staged, test.wantStaging, err)
			}
		})
	}
}

func TestWSLStageUploadDeadlineCannotConsumeExecutionReserve(t *testing.T) {
	stubWSLStageRoutePreparation(t, func(context.Context, SSHTarget, string, string) error { return nil })
	spool, _ := newTestWSLStageSpool(t, []byte("private-owner-payload"))
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	oldUpload := uploadWSLSpool
	var uploadBudget time.Duration
	uploadWSLSpool = func(_ *wslStageSpool, uploadCtx context.Context, _ SSHTarget, _ string, _ time.Duration, _, _ string, _ io.Writer) error {
		deadline, ok := uploadCtx.Deadline()
		if !ok {
			t.Fatal("owner SFTP upload has no bounded phase deadline")
		}
		uploadBudget = time.Until(deadline)
		<-uploadCtx.Done()
		return uploadCtx.Err()
	}
	t.Cleanup(func() { uploadWSLSpool = oldUpload })
	reserve := 125 * time.Millisecond
	started := time.Now()
	_, err := spool.stage(ctx, &SSHTarget{Port: "22", FallbackPorts: []string{}}, wslStageTiming{
		stage: time.Second, idle: time.Second, reserve: reserve,
	}, "10", "3", io.Discard)
	remaining := time.Until(started.Add(250 * time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) || uploadBudget <= 0 || uploadBudget >= 200*time.Millisecond ||
		ctx.Err() != nil || remaining < 75*time.Millisecond {
		t.Fatalf("upload budget=%s outer remaining=%s outer error=%v error=%v", uploadBudget, remaining, ctx.Err(), err)
	}
}

// This fixture exercises Go's ownership/byte decisions only. Native Windows
// fixtures execute the real same-handle verifier; this is not kernel proof.
func installWSLStageDiscardFixture(t *testing.T, root string) {
	t.Helper()
	old := discardWSLStageFile
	discardWSLStageFile = func(ctx context.Context, _ SSHTarget, nonce, name string, size int64, digest [sha256.Size]byte, _ string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		path := filepath.Join(root, filepath.FromSlash(wslStageRoot), name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != size {
			return errors.New("refuse replaced stage")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if sha256.Sum256(data) != digest {
			return errors.New("refuse replaced stage")
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { discardWSLStageFile = old })
}

func TestWSLStageCleanupContextsAreFiniteAndHonorCancellation(t *testing.T) {
	for _, phase := range []string{"deadline", "already canceled", "late cancellation"} {
		t.Run(phase, func(t *testing.T) {
			parent, cancelParent := context.WithCancel(t.Context())
			defer cancelParent()
			if phase == "already canceled" {
				cancelParent()
			}
			ctx, cancel := wslStageCleanupContext(parent, 100*time.Millisecond, 20*time.Millisecond)
			defer cancel()
			if phase == "late cancellation" {
				cancelParent()
			}
			start := time.Now()
			<-ctx.Done()
			maximum := 150 * time.Millisecond
			if phase != "deadline" {
				maximum = 80 * time.Millisecond
			}
			if time.Since(start) > maximum {
				t.Fatalf("cleanup exceeded %s", maximum)
			}
		})
	}
}

func TestWSLStagePartialCleanupBindsExactLocalPrefix(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			root := t.TempDir()
			installWSLStageDiscardFixture(t, root)
			old := startWSLStageCleanupSubsystem
			startWSLStageCleanupSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				return startLoopbackWSLSFTPSubsystem(ctx, root, func(c net.Conn) net.Conn { return c })
			}
			t.Cleanup(func() { startWSLStageCleanupSubsystem = old })
			spool, data := newTestWSLStageSpool(t, []byte("binary payload"))
			nonce := strings.Repeat("d", 32)
			dir := filepath.Join(root, filepath.FromSlash(wslStageRoot))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			prefix := bytes.Clone(data[:100])
			if replacement {
				prefix[40] ^= 1
			}
			path := filepath.Join(dir, nonce+".part")
			if err := os.WriteFile(path, prefix, 0600); err != nil {
				t.Fatal(err)
			}
			err := cleanupWSLStagePart(t.Context(), SSHTarget{}, nonce, spool, time.Second, time.Second, "1")
			if (err != nil) != replacement {
				t.Fatalf("replacement=%t cleanup=%v", replacement, err)
			}
			if replacement {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, prefix) {
					t.Fatal("foreign partial changed")
				}
			}
		})
	}
}

func TestWSLStageUnacknowledgedCreateNeverAcquiresCleanupAuthority(t *testing.T) {
	root := t.TempDir()
	nonce := strings.Repeat("a", 32)
	part := filepath.Join(root, filepath.FromSlash(wslStageRoot), nonce+".part")
	oldUpload, oldCleanup := startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem
	t.Cleanup(func() { startWSLStageUploadSubsystem, startWSLStageCleanupSubsystem = oldUpload, oldCleanup })
	startWSLStageUploadSubsystem = func(ctx context.Context, _ SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		return startLoopbackWSLSFTPSubsystem(ctx, root, func(conn net.Conn) net.Conn { return &dropCreatedStageResponse{Conn: conn, path: part} })
	}
	startWSLStageCleanupSubsystem = func(context.Context, SSHTarget, string, string, string, io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
		t.Fatal("unacknowledged creation acquired cleanup authority")
		return nil, nil, nil, nil
	}
	spool, _ := newTestWSLStageSpool(t, []byte("must not be delivered"))
	err := spool.upload(t.Context(), SSHTarget{Port: "22"}, nonce, time.Second, "1", "1", io.Discard)
	var terminal nonRetryableWSLStageError
	if !errors.As(err, &terminal) || !strings.Contains(err.Error(), "creation was not acknowledged") {
		t.Fatalf("unknown creation allowed replay: %v", err)
	}
	if info, err := os.Stat(part); err != nil || info.Size() != 0 {
		t.Fatalf("unacknowledged object was changed: %v", err)
	}
}

type dropCreatedStageResponse struct {
	net.Conn
	path string
}

func (c *dropCreatedStageResponse) Write(p []byte) (int, error) {
	if _, err := os.Stat(c.path); err == nil {
		_ = c.Conn.Close()
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(p)
}

func TestWSLStagePartialObservationCancellationClosesPipes(t *testing.T) {
	for _, phase := range []string{"deadline", "already canceled", "late cancellation"} {
		t.Run(phase, func(t *testing.T) {
			old := startWSLStageCleanupSubsystem
			t.Cleanup(func() { startWSLStageCleanupSubsystem = old })
			peerEOF := make(chan struct{})
			startWSLStageCleanupSubsystem = func(ctx context.Context, target SSHTarget, _, _, _ string, _ io.Writer) (io.Reader, io.WriteCloser, func() error, error) {
				if !target.NoControlMaster || len(target.FallbackPorts) != 0 {
					t.Fatal("partial observation escaped its pinned route")
				}
				client, server := net.Pipe()
				t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
				go func() { _, _ = io.Copy(io.Discard, server); _ = server.Close(); close(peerEOF) }()
				return client, client, func() error { <-peerEOF; return ctx.Err() }, nil
			}
			spool, _ := newTestWSLStageSpool(t, nil)
			parent, cancelParent := context.WithCancel(t.Context())
			defer cancelParent()
			if phase == "already canceled" {
				cancelParent()
			}
			if phase == "late cancellation" {
				timer := time.AfterFunc(10*time.Millisecond, cancelParent)
				defer timer.Stop()
			}
			done := make(chan error, 1)
			go func() {
				done <- cleanupWSLStagePart(parent, SSHTarget{Port: "22"}, strings.Repeat("c", 32), spool, 100*time.Millisecond, 20*time.Millisecond, "1")
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cleanup cause=%v", err)
				}
				select {
				case <-peerEOF:
				default:
					t.Fatal("cleanup returned without closing its transport")
				}
			case <-time.After(time.Second):
				t.Fatal("partial observation did not close bounded SFTP pipes")
			}
		})
	}
}

const wslHandoffFixture = `Add-Type -TypeDefinition '
public class HandoffFixture {
    public long ElapsedMilliseconds;
    public int Delay, ExitDelay, ExitCode, CleanupDelay, CleanupCode;
    public bool Late, CleanupLate, HasExited;
    public System.Collections.Generic.List<int> ExitWaits = new System.Collections.Generic.List<int>();
    public bool WaitForExit(int ceiling) {
        ExitWaits.Add(ceiling);
        ElapsedMilliseconds += Late ? ExitDelay : System.Math.Min(ExitDelay, ceiling);
        HasExited = Late || ExitDelay <= ceiling;
        ExitDelay = 0;
        return HasExited;
    }
    public void WaitForExit() { WaitForExit(int.MaxValue); }
    public void Restart() {
        ElapsedMilliseconds = 0; ExitDelay = CleanupDelay;
        ExitCode = CleanupCode; Late = CleanupLate;
    }
    public void Dispose() { }
    public void Kill() { HasExited = true; }
    public System.IO.FileStream BaseStream;
    public System.IO.MemoryStream Wire = new System.IO.MemoryStream();
    public System.Collections.Generic.List<int> Ceilings = new System.Collections.Generic.List<int>();
    private byte[] pending;
    public HandoffFixture FlushAsync() { pending = null; return this; }
    public HandoffFixture WriteAsync(byte[] bytes, int offset, int count) {
        pending = new byte[count]; System.Array.Copy(bytes, offset, pending, 0, count); return this;
    }
    public bool Wait(int ceiling) {
        Ceilings.Add(ceiling); ElapsedMilliseconds += System.Math.Min(Delay, ceiling);
        int delay = Delay; Delay = 0;
        if (delay > ceiling) return false;
        if (pending != null) Wire.Write(pending, 0, pending.Length);
        return true;
    }
    public HandoffFixture GetAwaiter() { return this; }
    public void GetResult() { }
}'`

func TestWSLStageInitialHandoffBudgets(t *testing.T) {
	t.Run("embedded programs use LF", func(t *testing.T) {
		for name, program := range map[string]string{
			"wsl-stage.ps1":     wslStageVerifier,
			"wsl-owner.ps1":     wslWindowsOwner,
			"wsl-supervisor.sh": wslLinuxHelper,
		} {
			if strings.ContainsRune(program, '\r') || !strings.HasSuffix(program, "\n") {
				t.Errorf("embedded %s must use LF line endings; check .gitattributes", name)
			}
		}
	})
	executable := "pwsh"
	if runtime.GOOS == "windows" {
		executable = "powershell.exe"
	}
	powerShell, err := exec.LookPath(executable)
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	_, raw := newTestWSLStageSpool(t, []byte{0, 255, 13, 10})
	owner, helper, command, payload := decodeWSLStage(t, raw)
	functions, _, found := strings.Cut(owner, "\ntry {\n    $process = Start-Linux 'run'")
	if !found {
		t.Fatal("owner function fixture boundary missing")
	}
	path := filepath.Join(t.TempDir(), "envelope")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                                     string
		execution, elapsed, flush, helper, input int
		ceilings                                 string
		end                                      int
		reason                                   string
		complete                                 bool
	}{
		{"delayed helper", 12000, 0, 0, 3000, 0, "[12000,12000,2000]", 3000, "", true},
		{"delayed flush and helper", 12000, 0, 3000, 3000, 0, "[12000,9000,2000]", 6000, "", true},
		{"never ready", 12000, 0, 0, 20000, 0, "[12000,12000]", 12000, "WSL2 command timed out", false},
		{"no execution clock reset", 12000, 11000, 0, 1500, 0, "[1000,1000]", 12000, "WSL2 command timed out", false},
		{"later input stall", 12000, 0, 0, 3000, 3000, "[12000,12000,2000]", 5000, "WSL2 pipe transfer made no progress", false},
		{"ordinary transfer idle unchanged", 0, 0, 0, 3000, 3000, "[15000,15000,15000]", 6000, "", true},
		{"unlimited execution bounded startup", 0, 0, 0, 20000, 0, "[15000,15000]", 15000, "WSL2 pipe transfer made no progress", false},
		{"shared startup cap", 0, 14000, 600, 600, 0, "[1000,400]", 15000, "WSL2 pipe transfer made no progress", false},
		{"cleanup total clips startup", 10000, 0, 3000, 8000, 0, "[10000,7000]", 10000, "WSL2 command timed out", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Exercise the generated functions with a deterministic clock/task;
			// Windows fixtures separately exercise the real anonymous pipe reader.
			idle := sshTransportTiming(sshCommandLimit{control: test.execution > 0}).idle.Milliseconds()
			script := wslHandoffFixture + `
$file=[IO.File]::OpenRead(` + psQuote(path) + `)
try {
  $descriptor=[IO.BinaryReader]::new($file).ReadBytes(` + fmt.Sprint(wslStageHeaderSize) + `)
  $file.Position+=` + fmt.Sprint(len(owner)) + `
  & {` + functions + fmt.Sprintf(`
$execution=%d; $idle=%d
$clock=[HandoffFixture]::new(); $clock.ElapsedMilliseconds=%d
$clock.BaseStream=[IO.File]::Open(`, test.execution, idle, test.elapsed) + psQuote(filepath.Join(t.TempDir(), "pipe-view")) + `,'CreateNew','Write','ReadWrite')
$reason=''
try {
  $clock.Delay=` + fmt.Sprint(test.flush) + `
  $view=Open-LinuxInput ([pscustomobject]@{StandardInput=$clock})
  $view.Dispose()
  $clock.Delay=` + fmt.Sprint(test.helper) + `
  Write-Pipe $clock $helper $helper.Length -startup
  $frame=[IO.BinaryReader]::new($file).ReadBytes([int]($commandSize+$payloadSize))
  $clock.Delay=` + fmt.Sprint(test.input) + `
  Write-Pipe $clock $frame $frame.Length
} catch { $reason=$_.Exception.Message } finally { $clock.BaseStream.Dispose() }
@{ceilings=@($clock.Ceilings.ToArray());elapsed=$clock.ElapsedMilliseconds;reason=$reason;wire=[Convert]::ToBase64String($clock.Wire.ToArray())}|ConvertTo-Json -Compress
  } $file $descriptor 'handoff-fixture'
} finally { $file.Dispose() }`
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", "& ([ScriptBlock]::Create([Console]::In.ReadToEnd()))")
			cmd.Stdin = strings.NewReader(script)
			diagnostic := &boundedWSLDiagnostics{remaining: wslSFTPDiagnosticLimit}
			cmd.Stderr = diagnostic
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("handoff fixture: %v cause=%v stderr=%q", err, context.Cause(ctx), RedactDiagnosticSecrets(diagnostic.buffer.String()))
			}
			var got struct {
				Ceilings []int
				Elapsed  int
				Reason   string
				Wire     []byte
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			ceilings, _ := json.Marshal(got.Ceilings)
			if string(ceilings) != test.ceilings || got.Elapsed != test.end || got.Reason != test.reason {
				t.Fatalf("ceilings=%s elapsed=%d reason=%q; want %s %d %q", ceilings, got.Elapsed, got.Reason, test.ceilings, test.end, test.reason)
			}
			if test.complete && !bytes.Equal(got.Wire, append([]byte(helper+command), payload...)) {
				t.Fatal("initial handoff changed helper/frame bytes")
			}
		})
	}
	t.Run("completion", func(t *testing.T) { testWSLStageOwnerCompletionBudgets(t, powerShell) })
}

func testWSLStageOwnerCompletionBudgets(t *testing.T, powerShell string) {
	t.Helper()
	for _, test := range []struct {
		name                        string
		finite                      bool
		delay, code                 int
		late                        bool
		cleanupDelay, cleanupCode   int
		cleanupLate, cleanupFailure bool
		wantCode                    int
		wantFailure                 bool
	}{
		{name: "control handoff plus normal grace", delay: 5000},
		{name: "ordinary finite deadline unchanged", finite: true, delay: 5000, wantFailure: true},
		{name: "late zero rejected", delay: 38000, late: true, wantFailure: true},
		{name: "late nonzero preserved", delay: 38000, late: true, code: 23, wantCode: 23},
		{name: "timeout survives timely cleanup", delay: 38000, cleanupDelay: 5000, wantFailure: true},
		{name: "late cleanup zero rejected", delay: 38000, cleanupDelay: 10001, cleanupLate: true, cleanupFailure: true, wantFailure: true},
		{name: "cleanup refusal stays failure", delay: 38000, cleanupCode: 23, cleanupFailure: true, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, raw := newTestWSLStageSpoolWithLimit(t, []byte{0, 255, 13, 10}, sshCommandLimit{execution: sshControlExecutionLimit, control: !test.finite})
			owner, helper, command, payload := decodeWSLStage(t, raw)
			path := filepath.Join(t.TempDir(), "envelope")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			// Keep the generated owner, including Remaining, WaitForExit and
			// cleanup. Only the clock and external launcher/pipe are modeled.
			script := owner
			for call, replacement := range map[string]string{
				"[Diagnostics.Stopwatch]::StartNew()":       "$fixture",
				"$process = Start-Linux 'run'":              "$process = $fixture",
				"$writer = Open-LinuxInput $process":        "$writer = $fixture",
				"$cleanup = Start-Linux 'cleanup'":          "$cleanup = $fixture",
				"$cleanupWriter = Open-LinuxInput $cleanup": "$cleanupWriter = $fixture",
				"exit $code": "$script:result = $code",
			} {
				if !strings.Contains(script, call) {
					t.Fatal("owner fixture boundary missing")
				}
				script = strings.Replace(script, call, replacement, 1)
			}
			script = wslHandoffFixture + fmt.Sprintf(`
$fixture=[HandoffFixture]::new();$fixture.Delay=11000
$fixture.ExitDelay=%d;$fixture.ExitCode=%d;$fixture.Late=$%t
$fixture.CleanupDelay=%d;$fixture.CleanupCode=%d;$fixture.CleanupLate=$%t
`, test.delay, test.code, test.late, test.cleanupDelay, test.cleanupCode, test.cleanupLate) + `
$file=[IO.File]::OpenRead(` + psQuote(path) + `)
$reason='';$script:result=-1
try {
  $descriptor=[IO.BinaryReader]::new($file).ReadBytes(` + fmt.Sprint(wslStageHeaderSize) + `)
  $file.Position+=` + fmt.Sprint(len(owner)) + `
  & ([ScriptBlock]::Create(` + psQuote(script) + `)) $file $descriptor 'completion-fixture'
} catch { $reason=$_.Exception.Message } finally { $file.Dispose() }
@{code=$script:result;reason=$reason;waits=@($fixture.ExitWaits.ToArray());wire=[Convert]::ToBase64String($fixture.Wire.ToArray())}|ConvertTo-Json -Compress`
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", "& ([ScriptBlock]::Create([Console]::In.ReadToEnd()))")
			cmd.Stdin = strings.NewReader(script)
			diagnostic := &boundedWSLDiagnostics{remaining: wslSFTPDiagnosticLimit}
			cmd.Stderr = diagnostic
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("completion fixture: %v cause=%v stderr=%q", err, context.Cause(ctx), RedactDiagnosticSecrets(diagnostic.buffer.String()))
			}
			var got struct {
				Code   int
				Reason string
				Waits  []int
				Wire   []byte
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			wantReason := ""
			if test.wantFailure {
				wantReason = fmt.Sprintf("WSL2 command timed out phase=execute expected=%d read=%d written=%d", len(command)+len(payload), len(command)+len(payload), len(command)+len(payload))
			}
			if got.Reason != wantReason || !test.wantFailure && got.Code != test.wantCode {
				t.Fatalf("code=%d reason=%q want code=%d reason=%q", got.Code, got.Reason, test.wantCode, wantReason)
			}
			wantDiagnostic := ""
			if test.cleanupFailure {
				wantDiagnostic = "WSL2 command cleanup failed"
			}
			if strings.TrimSpace(diagnostic.buffer.String()) != wantDiagnostic {
				t.Fatalf("cleanup diagnostic=%q want=%q", diagnostic.buffer.String(), wantDiagnostic)
			}
			wantWait := 27000
			if test.finite {
				wantWait = 1000
			}
			if len(got.Waits) == 0 || got.Waits[0] != wantWait {
				t.Fatalf("execution waits=%v want first=%d", got.Waits, wantWait)
			}
			wantWire := append([]byte(helper+command), payload...)
			if test.wantFailure {
				wantWire = append(wantWire, []byte(helper)...)
			}
			if !bytes.Equal(got.Wire, wantWire) {
				t.Fatal("owner changed the finite frame or replayed command/input during cleanup")
			}
		})
	}
}

func TestWSLStageOwnerReportsExactFailurePhase(t *testing.T) {
	executable := "pwsh"
	if runtime.GOOS == "windows" {
		executable = "powershell.exe"
	}
	powerShell, err := exec.LookPath(executable)
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	_, raw := newTestWSLStageSpool(t, []byte{0, 255})
	owner, _, command, payload := decodeWSLStage(t, raw)
	path := filepath.Join(t.TempDir(), "envelope")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	const privateFailure = "private-operation-detail"
	for _, test := range []struct{ name, phase, call, reason string }{
		{"launch", "launcher-start", "$process = Start-Linux 'run'", privateFailure},
		{"flush", "pipe-open", "$writer = Open-LinuxInput $process", "WSL2 pipe transfer made no progress"},
		{"open fault", "pipe-open", "$writer = Open-LinuxInput $process", privateFailure},
		{"helper", "helper-write", "Write-Pipe $writer $helper $helper.Length -startup", "WSL2 pipe transfer made no progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Execute the generated owner's validation/catch/finally paths, but
			// replace external operations so this fixture never launches WSL.
			script := owner
			for call, replacement := range map[string]string{
				"$process = Start-Linux 'run'":                       "$process = $null",
				"$writer = Open-LinuxInput $process":                 "$writer = [IO.MemoryStream]::new()",
				"Write-Pipe $writer $helper $helper.Length -startup": "",
				"$cleanup = Start-Linux 'cleanup'":                   "throw " + psQuote(privateFailure),
			} {
				if call == test.call {
					replacement = "throw " + psQuote(test.reason)
				}
				if !strings.Contains(script, call) {
					t.Fatal("owner fixture injection point missing")
				}
				script = strings.Replace(script, call, replacement, 1)
			}
			script = `$file=[IO.File]::OpenRead(` + psQuote(path) + `)
try {
  $descriptor=[IO.BinaryReader]::new($file).ReadBytes(` + fmt.Sprint(wslStageHeaderSize) + `)
  $file.Position+=` + fmt.Sprint(len(owner)) + `
  & ([ScriptBlock]::Create(` + psQuote(script) + `)) $file $descriptor 'diagnostic-fixture'
} catch { [Console]::Out.Write($_.Exception.Message) } finally { $file.Dispose() }`
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", "& ([ScriptBlock]::Create([Console]::In.ReadToEnd()))")
			cmd.Stdin = strings.NewReader(script)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("owner fixture failed: %v", err)
			}
			reason := test.reason
			if reason == privateFailure {
				reason = "WSL2 transport failed"
			}
			want := fmt.Sprintf("%s phase=%s expected=%d read=0 written=0", reason, test.phase, len(command)+len(payload))
			if stdout.String() != want || strings.TrimSpace(stderr.String()) != "WSL2 command cleanup failed" {
				t.Fatal("owner diagnostic lost the exact phase, workload counters, or error redaction")
			}
		})
	}
}

func TestWSLStageShellProofRequiresExactNonceAndKnownShell(t *testing.T) {
	for _, result := range []string{"cmd", "powershell", "bash", "", "cmd extra", "wrong-nonce"} {
		t.Run(result, func(t *testing.T) {
			installWorkspaceOwnerRecordingSSH(t)
			nonce := strings.Repeat("a", 32)
			output := wslStagePreparationMarker + " " + nonce + " " + result
			if result == "wrong-nonce" {
				output = wslStagePreparationMarker + " " + strings.Repeat("b", 32) + " cmd"
			}
			t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", output)
			shell, err := prepareWSLStageRoot(context.WithValue(t.Context(), wslStageRouteProofKey{}, nonce), SSHTarget{Host: "example.test"}, "10", "3")
			valid := result == "cmd" || result == "powershell"
			if valid != (err == nil) || valid && string(shell) != result || !valid && shell != "" {
				t.Fatalf("shell=%q err=%v", shell, err)
			}
		})
	}
	if command := wslStageLauncherCommand(strings.Repeat("a", 32), 1, [sha256.Size]byte{}, ""); command != "" {
		t.Fatal("unproven shell launched")
	}
}

func TestWSLStagePowerShellDefaultShellPreservesNativeStreamsAndExit(t *testing.T) {
	powerShell, err := exec.LookPath("pwsh")
	if err != nil {
		powerShell, err = exec.LookPath("powershell.exe")
	}
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	// A real child inherits the OS streams, as wsl.exe does in the Windows owner.
	child := `[Console]::OpenStandardOutput().Write([byte[]](65,0,255,13,10),0,5);[Console]::OpenStandardError().Write([byte[]](66,0,254,10),0,4);exit 23`
	childArgs := strings.TrimPrefix(powershellCommand(child), "powershell.exe ")
	script := `$p=[Diagnostics.ProcessStartInfo]::new(` + psQuote(powerShell) + `);$p.UseShellExecute=$false;$p.Arguments=` + psQuote(childArgs) + `;$c=[Diagnostics.Process]::Start($p);$c.WaitForExit();exit $c.ExitCode`
	command := wslStagePowerShellCommand(script, wslStagePowerShell)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), powerShell, "-NoProfile", "-NonInteractive", "-Command", command)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if exitCode(err) != 23 || !bytes.Equal(stdout.Bytes(), []byte{65, 0, 255, 13, 10}) || !bytes.Equal(stderr.Bytes(), []byte{66, 0, 254, 10}) {
		t.Fatalf("literal default-shell command: exit=%d stdout=%x stderr=%x err=%v", exitCode(err), stdout.Bytes(), stderr.Bytes(), err)
	}
	for _, shell := range []wslStageShell{wslStageCMD, wslStagePowerShell} {
		command := wslStageLauncherCommand(strings.Repeat("a", 32), wslStageMaxSize, [sha256.Size]byte{}, shell)
		if len(command) >= wslStageLauncherCommandLimit || command == "" {
			t.Fatalf("shell=%s command size=%d", shell, len(command))
		}
		cmd := exec.CommandContext(t.Context(), powerShell, "-NoProfile", "-NonInteractive", "-Command", `$errors=$null;[Management.Automation.Language.Parser]::ParseInput([Console]::In.ReadToEnd(),[ref]$null,[ref]$errors)|Out-Null;if($errors){exit 1}`)
		cmd.Stdin = strings.NewReader(command)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("literal %s parser: %s %v", shell, out, err)
		}
	}
}

func TestWSLStageUploadPublicationContract(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "empty"},
		{name: "small_binary", payload: []byte{0, 1, 2, 255}},
		{name: "8_MiB_binary", payload: bytes.Repeat([]byte{0, 255, 13, 10}, 2<<20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			nonce, err := randomHex(16)
			if err != nil {
				t.Fatal(err)
			}
			var requests *wslSFTPRequestConn
			client := newLoopbackSFTPClientWithServerConn(t, root, func(conn net.Conn) net.Conn {
				requests = &wslSFTPRequestConn{Conn: conn}
				return requests
			})
			stageRoot := filepath.Join(root, filepath.FromSlash(wslStageRoot))
			proof := filepath.Join(stageRoot, "."+nonce+".proof")
			if err := os.WriteFile(proof, []byte(nonce), 0o600); err != nil {
				t.Fatal(err)
			}
			foreignPart := filepath.Join(stageRoot, strings.Repeat("e", 32)+".part")
			if err := os.WriteFile(foreignPart, []byte("unknown"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-72 * time.Hour)
			if err := os.Chtimes(foreignPart, old, old); err != nil {
				t.Fatal(err)
			}
			spool, want := newTestWSLStageSpool(t, test.payload)
			spool.routeProof = nonce
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			owned, err := uploadToSFTP(client, spool, nonce, time.Second, cancel)
			if err != nil || owned != wslStagePublished {
				t.Fatalf("upload ownership=%d err=%v", owned, err)
			}
			if requests.readsBeforeWrite.Load() == 0 || requests.readsAfterWrite.Load() != 0 {
				t.Fatalf("root reads=%d content reads=%d", requests.readsBeforeWrite.Load(), requests.readsAfterWrite.Load())
			}
			got, err := os.ReadFile(filepath.Join(stageRoot, nonce+".ready"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) || sha256.Sum256(got) != spool.digest() {
				t.Fatal("published WSL2 stage bytes or digest changed")
			}
			if cause := context.Cause(ctx); cause != nil {
				t.Fatalf("unexpected transfer cancellation: %v", cause)
			}
			if _, err := os.Stat(filepath.Join(stageRoot, nonce+".part")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial stage survived: %v", err)
			}
			if runtime.GOOS != "windows" {
				rootInfo, err := os.Stat(stageRoot)
				if err != nil {
					t.Fatal(err)
				}
				if rootInfo.Mode().Perm() != 0o700 {
					t.Fatalf("stage root is not private: mode=%o", rootInfo.Mode().Perm())
				}
			}
			if data, err := os.ReadFile(foreignPart); err != nil || string(data) != "unknown" {
				t.Fatalf("unknown old partial removed or changed: %v", err)
			}
		})
	}
}

func TestWSLStageUploadMetadataFailurePreventsPublication(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint(directory), func(t *testing.T) {
			root, nonce := t.TempDir(), strings.Repeat("d", 32)
			part := filepath.Join(root, filepath.FromSlash(wslStageRoot), nonce+".part")
			client := newLoopbackSFTPClientWithServerConn(t, root, func(conn net.Conn) net.Conn {
				return &wslSFTPRequestConn{Conn: conn, beforeClose: func() {
					if directory {
						if err := os.Remove(part); err != nil {
							t.Error(err)
							return
						}
						if err := os.Mkdir(part, 0700); err != nil {
							t.Error(err)
						}
					} else if err := os.Truncate(part, 1); err != nil {
						t.Error(err)
					}
				}}
			})
			spool, _ := newTestWSLStageSpool(t, []byte("metadata"))
			_, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			owned, err := uploadToSFTP(client, spool, nonce, time.Second, cancel)
			var retryable retryableWSLStageError
			if err == nil || errors.As(err, &retryable) || owned != wslStagePartial || !strings.Contains(err.Error(), "metadata failed") {
				t.Fatalf("metadata=%d %v", owned, err)
			}
			if _, err := os.Stat(part); err != nil {
				t.Fatal("metadata failure deleted partial")
			}
			if _, err := os.Stat(strings.TrimSuffix(part, ".part") + ".ready"); !os.IsNotExist(err) {
				t.Fatal("metadata failure published")
			}
		})
	}
}

func TestWSLStagePublishedCorruptionIsNotReplayedOrDeleted(t *testing.T) {
	root := t.TempDir()
	installWSLStageDiscardFixture(t, root)
	stubWSLStageRoutePreparation(t, func(context.Context, SSHTarget, string, string) error { return nil })
	ssh := installWorkspaceOwnerRecordingSSH(t)
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "1")
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CODE", "74") // native verifier refusal, exercised on Windows separately
	oldUpload := uploadWSLSpool
	t.Cleanup(func() { uploadWSLSpool = oldUpload })
	client := newLoopbackSFTPClient(t, root)
	attempts := 0
	var ready string
	var corrupted []byte
	uploadWSLSpool = func(spool *wslStageSpool, ctx context.Context, _ SSHTarget, nonce string, idle time.Duration, _, _ string, _ io.Writer) error {
		attempts++
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(wslStageRoot), "."+nonce+".proof"), []byte(nonce), 0600); err != nil {
			return err
		}
		_, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		owned, err := uploadToSFTP(client, spool, nonce, idle, cancel)
		if err != nil || owned != wslStagePublished {
			return fmt.Errorf("upload=%d: %w", owned, err)
		}
		ready = filepath.Join(root, filepath.FromSlash(wslStageRoot), nonce+".ready")
		corrupted, err = os.ReadFile(ready)
		if err != nil {
			return err
		}
		corrupted[len(corrupted)-1] ^= 1
		return os.WriteFile(ready, corrupted, 0600)
	}
	spool, _ := newTestWSLStageSpool(t, []byte("binary payload"))
	target := SSHTarget{Host: "example.test", Port: "2222", FallbackPorts: []string{"22"}}
	err := spool.run(t.Context(), &target, "10", "3", io.Discard, io.Discard)
	if err == nil || attempts != 1 || !strings.Contains(err.Error(), "ready stage cleanup failed") {
		t.Fatalf("published corruption attempts=%d err=%v", attempts, err)
	}
	if _, err := os.Stat(filepath.Join(ssh, "2.args")); !os.IsNotExist(err) {
		t.Fatal("published corruption replayed execution")
	}
	got, err := os.ReadFile(ready)
	if err != nil || !bytes.Equal(got, corrupted) {
		t.Fatalf("corrupt replacement deleted/changed: %v", err)
	}
}

func TestSSHTransportRejectsLateZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local POSIX output-drain fixture")
	}
	for _, staged := range []bool{false, true} {
		for _, code := range []int{0, 23} {
			t.Run(fmt.Sprintf("staged=%t/exit=%d", staged, code), func(t *testing.T) {
				dir := t.TempDir()
				// The descendant writes only after the SSH leader has been reaped.
				// Cancellation during output drain therefore cannot kill the leader
				// and mask the late-success guard with a process failure.
				script := `#!/bin/sh
parent=$$
(
  attempts=0
  while kill -0 "$parent" 2>/dev/null; do
    attempts=$((attempts + 1))
    [ "$attempts" -lt 200 ] || exit 99
    sleep 0.01
  done
  printf drained
) &
exit ` + fmt.Sprint(code) + "\n"
				if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := errors.New("caller expired during output drain")
				var output bytes.Buffer
				stdout := writerFunc(func(p []byte) (int, error) { cancel(cause); return output.Write(p) })
				target := SSHTarget{Host: "example.test", Port: "22", FallbackPorts: []string{}, NoControlMaster: true}
				var err error
				cleanups := 0
				cleanupFailure := errors.New("owned cleanup unconfirmed")
				if staged {
					spool, _ := newTestWSLStageSpoolWithLimit(t, nil, sshCommandLimit{execution: sshControlExecutionLimit})
					captureWSLStage(t, strings.Repeat("a", 32), func(*wslStageSpool, *SSHTarget, wslStageTiming, []byte) {})
					previous := cleanupPublishedWSLStage
					t.Cleanup(func() { cleanupPublishedWSLStage = previous })
					cleanupPublishedWSLStage = func(context.Context, SSHTarget, string, *wslStageSpool, time.Duration, time.Duration, string) error {
						cleanups++
						return cleanupFailure
					}
					err = spool.run(ctx, &target, "10", "3", stdout, io.Discard)
				} else {
					err = executePreparedSSH(ctx, &target, "true", nil, 0, sshCommandLimit{execution: sshControlExecutionLimit}, "10", "3", stdout, io.Discard)
				}
				if output.String() != "drained" || !errors.Is(context.Cause(ctx), cause) {
					t.Fatalf("drain boundary not reached: output=%q cause=%v", output.String(), context.Cause(ctx))
				}
				if code == 0 && !errors.Is(err, cause) || code != 0 && exitCode(err) != code {
					t.Fatalf("late exit=%d error=%v", code, err)
				}
				if staged && (cleanups != 1 || !errors.Is(err, cleanupFailure)) {
					t.Fatalf("cleanup calls=%d error=%v", cleanups, err)
				}
			})
		}
	}
}

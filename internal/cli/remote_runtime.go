package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

// An explicit operator artifact set supports unpublished and offline builds.
// Without one, existing shell-backed installations retain their current route.
const runtimeArtifactsEnv = "CRABBOX_RUNTIME_ARTIFACTS"

type nativeRuntimeContextKey struct{}

type remoteNativeRuntime struct {
	target SSHTarget
	nonce  string
	path   string
	shell  wslStageShell
}

func (r *remoteNativeRuntime) command(args ...string) string {
	words := []string{r.path, remoteruntime.Command}
	words = append(words, args...)
	for i := range words {
		words[i] = shellQuote(words[i])
	}
	return remotePOSIXControlCommand("exec " + strings.Join(words, " "))
}

func (r *remoteNativeRuntime) runCommand(nonce string, commandBytes int, budget time.Duration) string {
	return r.command("run", remoteruntime.Protocol, nonce, strconv.Itoa(commandBytes), "0",
		strconv.FormatInt(wslStageIdleTimeout.Milliseconds(), 10), strconv.FormatInt(wsl2SignalGrace.Milliseconds(), 10),
		strconv.FormatInt(budget.Milliseconds(), 10), "preflight", "finite")
}

func (r *remoteNativeRuntime) close(ctx context.Context) error {
	cleanupCtx, cancel := wslStageCleanupContext(ctx, 15*time.Second, 5*time.Second)
	defer cancel()
	err := r.runPOSIX(cleanupCtx, nativeRuntimeRemoveScript(r.nonce), io.Discard)
	if err != nil {
		return fmt.Errorf("remote runtime staging cleanup unconfirmed for %s: %w", r.path, err)
	}
	return nil
}

func (r *remoteNativeRuntime) runPOSIX(ctx context.Context, script string, stdout io.Writer) error {
	command := remotePOSIXControlCommand(script)
	if isWindowsWSL2Target(r.target) {
		budget, err := nativeRuntimeTransportBudget(ctx)
		if err != nil {
			return err
		}
		command, err = nativeWSLPOSIXCommand(script, budget, r.shell)
		if err != nil {
			return err
		}
	}
	transport := sshTransportPreparation{command: command}
	_, err := transport.runOnce(ctx, r.target, "2", "1", stdout, io.Discard, false)
	if err == nil {
		err = context.Cause(ctx)
	}
	return err
}

func (r *remoteNativeRuntime) runMetadata(ctx context.Context, args []string, stdout io.Writer) error {
	command := r.command(args...)
	if isWindowsWSL2Target(r.target) {
		budget, err := nativeRuntimeTransportBudget(ctx)
		if err != nil {
			return err
		}
		command, err = nativeWSLMetadataCommand(r.path, append([]string{remoteruntime.Command}, args...), min(time.Minute, budget), r.shell)
		if err != nil {
			return err
		}
	}
	transport := sshTransportPreparation{command: command}
	_, err := transport.runOnce(ctx, r.target, "2", "1", stdout, io.Discard, false)
	if err == nil {
		err = context.Cause(ctx)
	}
	return err
}

func (r *remoteNativeRuntime) confirmCleanup(ctx context.Context, nonce string) error {
	budget, err := nativeRuntimeTransportBudget(ctx)
	if err != nil {
		return err
	}
	millis := min(budget, remoteruntime.MaxCleanupWait).Milliseconds()
	return r.runMetadata(ctx, []string{"control", remoteruntime.Protocol, nonce, "cleanup", strconv.FormatInt(millis, 10)}, io.Discard)
}

func nativeRuntimeTransportBudget(ctx context.Context) (time.Duration, error) {
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	budget := sshTransportPreparationTimeout
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
	}
	if budget < time.Millisecond {
		return 0, context.DeadlineExceeded
	}
	return budget, nil
}

func prepareNativeRuntime(ctx context.Context, target SSHTarget, artifacts *runtimeartifact.LocalSet) (installed *remoteNativeRuntime, err error) {
	var remote *remoteNativeRuntime
	defer func() {
		if err != nil && remote != nil {
			err = errors.Join(err, remote.close(ctx))
			installed = nil
		}
	}()
	if target.TargetOS != targetLinux && !isWindowsWSL2Target(target) {
		return nil, errors.New("native runtime installation requires a Linux or WSL2 SSH target")
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, sshTransportPreparationTimeout)
	defer cancelProbe()
	runtime := &remoteNativeRuntime{target: target}
	if isWindowsWSL2Target(target) {
		runtime.shell, err = discoverNativeWSLShell(probeCtx, target)
		if err != nil {
			return nil, err
		}
	}
	architecture := newSynchronizedBuffer(65)
	if err := runtime.runPOSIX(probeCtx, "uname -m\nif command -v gzip >/dev/null 2>&1; then printf 'gzip\\n'; else printf 'raw\\n'; fi", &architecture); err != nil {
		return nil, fmt.Errorf("discover native runtime architecture: %w", err)
	}
	arch := ""
	capabilities := strings.Fields(architecture.String())
	if len(capabilities) != 2 || capabilities[1] != "gzip" && capabilities[1] != "raw" {
		return nil, errors.New("invalid native runtime capability report")
	}
	switch capabilities[0] {
	case "x86_64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	default:
		return nil, errors.New("unsupported native runtime architecture")
	}
	artifact, err := artifacts.Open(ctx, runtimeartifact.Target{OS: "linux", Arch: arch})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, artifact.Close()) }()
	identity := artifact.Identity()
	var payload io.ReadSeeker = artifact
	transportSize, compressed := identity.Size, false
	if capabilities[1] == "gzip" {
		compressionCtx, cancelCompression := context.WithTimeout(ctx, wslStageBudget(wslStageTimeout, wslStageIdleTimeout, identity.Size))
		packed, packErr := runtimeartifact.CompressGzip(compressionCtx, artifact)
		cancelCompression()
		if packErr != nil {
			return nil, fmt.Errorf("compress native runtime: %w", packErr)
		}
		defer func() { err = errors.Join(err, packed.Close()) }()
		if packed.Size() < identity.Size {
			payload, transportSize, compressed = packed, packed.Size(), true
		}
	}
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	runtime.nonce, runtime.path = nonce, "/tmp/crabbox-runtime-"+nonce+"/crabbox"
	// Artifact installation has its own size-dependent allowance. It does not
	// consume the worker's execution clock or its separate cleanup reserve.
	transferBudget := wslStageBudget(wslStageTimeout, wslStageIdleTimeout, transportSize)
	// The checksum-tool-free path reads the full artifact back as well.
	readbackBudget := wslStageBudget(wslStageTimeout, wslStageIdleTimeout, identity.Size)
	installCtx, cancelInstall := context.WithTimeout(ctx, addWSLStageDurations(transferBudget, readbackBudget))
	defer cancelInstall()
	transport, input, err := runtime.prepareUpload(payload, transportSize, identity.Size, compressed, transferBudget)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, input.close()) }()
	remote = runtime // A failed dispatch may still have created remote staging.
	if _, err := transport.runOnce(installCtx, target, "2", "1", io.Discard, io.Discard, false); err != nil {
		return nil, fmt.Errorf("upload native runtime: %w", err)
	}
	if err := verifyNativeRuntimeBytes(installCtx, runtime, identity); err != nil {
		return nil, err
	}
	hello := newSynchronizedBuffer(4097)
	if err := runtime.runMetadata(installCtx, []string{"identity"}, &hello); err != nil {
		return nil, fmt.Errorf("native runtime handshake: %w", err)
	}
	if err := validateNativeRuntimeIdentity(hello.Bytes(), arch); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *remoteNativeRuntime) prepareUpload(payload io.ReadSeeker, transportSize, executableSize int64, compressed bool, budget time.Duration) (sshTransportPreparation, *replayableSSHInput, error) {
	command := nativeRuntimeUploadCommand(r.nonce, transportSize, executableSize, compressed)
	var prefix []byte
	if isWindowsWSL2Target(r.target) {
		installer := nativeRuntimeUploadScript(r.nonce, transportSize, executableSize, compressed)
		var err error
		command, err = nativeWSLUploadCommand(installer, transportSize, budget, r.shell)
		if err != nil {
			return sshTransportPreparation{}, nil, err
		}
		prefix = []byte(installer)
	}
	input, err := newReplayableSSHInputStream(prefix, payload, transportSize)
	if err != nil {
		return sshTransportPreparation{}, nil, err
	}
	return sshTransportPreparation{command: command, replay: input}, input, nil
}

func nativeRuntimeUploadCommand(nonce string, transportSize, executableSize int64, compressed bool) string {
	return remotePOSIXControlCommand(nativeRuntimeUploadScript(nonce, transportSize, executableSize, compressed))
}

func nativeRuntimeUploadScript(nonce string, transportSize, executableSize int64, compressed bool) string {
	directory := shellQuote("/tmp/crabbox-runtime-" + nonce)
	unpack := "mv -- \"$directory/upload.part\" \"$directory/crabbox.part\" || exit 74\n"
	if compressed {
		unpack = "gzip -dc -- \"$directory/upload.part\" >\"$directory/crabbox.part\" || exit 74\n" +
			"rm -f -- \"$directory/upload.part\" || exit 74\n"
	}
	return "set -eu\numask 077\ndirectory=" + directory + "\n" +
		"mkdir -m 700 -- \"$directory\" || exit 74\n" +
		"printf %s " + shellQuote(nonce) + " >\"$directory/.nonce\" || exit 74\n" +
		"head -c " + strconv.FormatInt(transportSize, 10) + " >\"$directory/upload.part\" || exit 74\n" +
		"[ \"$(wc -c <\"$directory/upload.part\")\" -eq " + strconv.FormatInt(transportSize, 10) + " ] || exit 74\n" + unpack +
		"[ \"$(wc -c <\"$directory/crabbox.part\")\" -eq " + strconv.FormatInt(executableSize, 10) + " ] || exit 74\n" +
		"chmod 500 \"$directory/crabbox.part\" || exit 74\n" +
		"mv -- \"$directory/crabbox.part\" \"$directory/crabbox\"\n"
}

func nativeRuntimeRemoveScript(nonce string) string {
	directory := shellQuote("/tmp/crabbox-runtime-" + nonce)
	return "set -eu\ndirectory=" + directory + "\n" +
		"if [ ! -e \"$directory\" ] && [ ! -L \"$directory\" ]; then exit 0; fi\n" +
		"[ -d \"$directory\" ] && [ ! -L \"$directory\" ] && [ -O \"$directory\" ] || exit 74\n" +
		"[ \"$(cat \"$directory/.nonce\")\" = " + shellQuote(nonce) + " ] || exit 74\n" +
		"rm -rf -- \"$directory\" || exit 74\n" +
		"[ ! -e \"$directory\" ] && [ ! -L \"$directory\" ]\n"
}

func verifyNativeRuntimeBytes(ctx context.Context, runtime *remoteNativeRuntime, identity runtimeartifact.Identity) error {
	path := shellQuote(runtime.path)
	command := "if command -v sha256sum >/dev/null 2>&1; then sha256sum -- " + path +
		"; elif command -v shasum >/dev/null 2>&1; then shasum -a 256 -- " + path +
		"; else printf 'CBX-NO-SHA256\\n'; fi"
	output := newSynchronizedBuffer(4097)
	if err := runtime.runPOSIX(ctx, command, &output); err != nil {
		return fmt.Errorf("verify native runtime bytes: %w", err)
	}
	if output.String() == "CBX-NO-SHA256\n" {
		// Minimal hosts need no new checksum utility: authenticated readback
		// verifies the exact staged bytes before any executable is started.
		writer := &runtimeDigestWriter{hash: sha256.New(), remaining: identity.Size}
		if err := runtime.runPOSIX(ctx, "exec cat -- "+path, writer); err != nil {
			return fmt.Errorf("read back native runtime: %w", err)
		}
		if writer.remaining != 0 || hex.EncodeToString(writer.hash.Sum(nil)) != identity.SHA256 {
			return errors.New("native runtime checksum mismatch")
		}
		return nil
	}
	fields := strings.Fields(output.String())
	if len(fields) != 2 || fields[0] != identity.SHA256 || fields[1] != runtime.path {
		return errors.New("native runtime checksum mismatch")
	}
	return nil
}

type runtimeDigestWriter struct {
	hash      hash.Hash
	remaining int64
}

func (w *runtimeDigestWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("native runtime readback exceeds artifact size")
	}
	n, err := w.hash.Write(data)
	w.remaining -= int64(n)
	return n, err
}

func validateNativeRuntimeIdentity(data []byte, arch string) error {
	var identity remoteruntime.Identity
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if len(data) > 4096 || decoder.Decode(&identity) != nil || identity.Protocol != remoteruntime.Protocol || identity.OS != "linux" || identity.Arch != arch || !identity.Runnable {
		return errors.New("native runtime identity mismatch")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("native runtime identity contains trailing output")
	}
	return nil
}

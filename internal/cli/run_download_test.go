package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRunDownloadSpec(t *testing.T) {
	got, err := parseRunDownloadSpec(`out/sixel.bin=/tmp/sixel.bin`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote != "out/sixel.bin" || got.Local != "/tmp/sixel.bin" {
		t.Fatalf("spec=%#v", got)
	}
	if _, err := parseRunDownloadSpec("out.bin"); err == nil {
		t.Fatal("expected missing local path to fail")
	}
}

func TestPreflightRunLocalOutputsRejectsBadInputs(t *testing.T) {
	if err := preflightRunLocalOutputs("", "", []string{"out.bin"}); err == nil {
		t.Fatal("expected malformed download to fail")
	}
	if err := preflightRunLocalOutputs(t.TempDir()+"/missing/stdout.bin", "", nil); err == nil {
		t.Fatal("expected missing capture directory to fail")
	}
	if err := preflightRunLocalOutputs("", "", []string{"remote.out=" + t.TempDir()}); err == nil {
		t.Fatal("expected download to existing directory to fail")
	}
	fileParent := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(fileParent, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := preflightRunLocalOutputs("", "", []string{"remote.out=" + fileParent + "/out.bin"}); err == nil {
		t.Fatal("expected download parent file to fail")
	}
}

func TestPreflightRunLocalOutputsValidatesCaptureFile(t *testing.T) {
	path := t.TempDir() + "/stdout.bin"
	if err := preflightRunLocalOutputs(path, "", []string{"remote.out=local.out"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("preflight should not create the final capture file")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPreflightRunLocalOutputsAllowsDownloadMissingDirs(t *testing.T) {
	root := t.TempDir()
	path := root + "/missing/nested/out.bin"
	if err := preflightRunLocalOutputs("", "", []string{"remote.out=" + path}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root + "/missing"); err == nil {
		t.Fatal("preflight should not create download directories")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPreflightRunLocalOutputsDoesNotTruncateExistingDownloadFile(t *testing.T) {
	path := t.TempDir() + "/out.bin"
	if err := os.WriteFile(path, []byte("keep"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := preflightRunLocalOutputs("", "", []string{"remote.out=" + path}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("download file was modified: %q", got)
	}
}

func TestPreflightRunLocalOutputsDoesNotTruncateExistingCaptureFile(t *testing.T) {
	path := t.TempDir() + "/stdout.bin"
	if err := os.WriteFile(path, []byte("keep"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := preflightRunLocalOutputs(path, "", nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("capture file was modified: %q", got)
	}
}

func TestPreflightRunLocalOutputsRejectsEquivalentCapturePaths(t *testing.T) {
	dir := t.TempDir()
	if err := preflightRunLocalOutputs(filepath.Join(dir, "run.log"), "", nil); err != nil {
		t.Fatal(err)
	}
	if err := preflightRunLocalOutputs(filepath.Join(dir, ".", "run.log"), filepath.Join(dir, "run.log"), nil); err == nil {
		t.Fatal("expected equivalent capture paths to fail")
	}
}

func TestPreflightRunLocalOutputsRejectsCaptureDownloadPathCollisions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")

	for _, tc := range []struct {
		name          string
		captureStdout string
		captureStderr string
	}{
		{name: "stdout", captureStdout: filepath.Join(dir, ".", "run.log")},
		{name: "stderr", captureStderr: filepath.Join(dir, ".", "run.log")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightRunLocalOutputs(tc.captureStdout, tc.captureStderr, []string{"remote.out=" + path})
			if err == nil {
				t.Fatal("expected capture/download collision to fail")
			}
			if !strings.Contains(err.Error(), "paths must be different") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestPreflightRunLocalOutputsRejectsDuplicateDownloadTargets(t *testing.T) {
	dir := t.TempDir()
	err := preflightRunLocalOutputs("", "", []string{
		"first.out=" + filepath.Join(dir, "artifacts", "run.log"),
		"second.out=" + filepath.Join(dir, "artifacts", ".", "run.log"),
	})
	if err == nil {
		t.Fatal("expected duplicate download target to fail")
	}
	if !strings.Contains(err.Error(), "paths must be different") {
		t.Fatalf("err=%v", err)
	}
}

func TestRemoteDownloadBase64CommandPOSIX(t *testing.T) {
	got := remoteDownloadBase64Command(SSHTarget{}, "/work/repo", "out/sixel.bin")
	for _, want := range []string{
		"cd '/work/repo'",
		"test -f 'out/sixel.bin'",
		"wc -c < 'out/sixel.bin'",
		remoteDownloadHeaderPrefix,
		"base64 < 'out/sixel.bin'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("command missing %q in %q", want, got)
		}
	}
}

func TestRemoteDownloadBase64CommandWindows(t *testing.T) {
	got := remoteDownloadBase64Command(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, `C:\crabbox\repo`, `out\sixel.bin`)
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`$path = 'out\sixel.bin'`,
		`[System.IO.File]::OpenRead`,
		remoteDownloadHeaderPrefix,
		`[System.Security.Cryptography.CryptoStream]`,
		`$file.CopyTo($encoded)`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("command missing %q in %q", want, decoded)
		}
	}
	if strings.Contains(decoded, "ReadAllBytes") {
		t.Fatalf("Windows download still buffers the whole file: %q", decoded)
	}
}

func TestRunDownloadDefaultLimits(t *testing.T) {
	if defaultRunDownloadLimits.MaxBytes != 1<<30 {
		t.Fatalf("ordinary download max=%d want=%d", defaultRunDownloadLimits.MaxBytes, int64(1<<30))
	}
	if defaultRunDownloadLimits.DiskReserveBytes != 1<<30 {
		t.Fatalf("ordinary download reserve=%d want=%d", defaultRunDownloadLimits.DiskReserveBytes, int64(1<<30))
	}
	if failureCaptureDownloadLimits.MaxBytes != 64<<20 {
		t.Fatalf("failure capture max=%d want=%d", failureCaptureDownloadLimits.MaxBytes, int64(64<<20))
	}
	if failureCaptureDownloadLimits.DiskReserveBytes != defaultRunDownloadLimits.DiskReserveBytes {
		t.Fatalf("failure capture reserve=%d want=%d", failureCaptureDownloadLimits.DiskReserveBytes, defaultRunDownloadLimits.DiskReserveBytes)
	}
}

func TestStageRunDownloadBoundsAndPreservesDestination(t *testing.T) {
	tests := []struct {
		name       string
		advertised int64
		payload    []byte
		limit      int64
		free       int64
		reserve    int64
		want       string
	}{
		{name: "zero bytes", advertised: 0, limit: 4, free: 100, reserve: 10},
		{name: "exact limit", advertised: 4, payload: []byte("data"), limit: 4, free: 100, reserve: 10},
		{name: "limit plus one", advertised: 5, payload: []byte("12345"), limit: 4, free: 100, reserve: 10, want: "advertised size 5 exceeds limit 4"},
		{name: "false remote size", advertised: 3, payload: []byte("data"), limit: 4, free: 100, reserve: 10, want: "decoded response exceeds advertised size 3"},
		{name: "short remote size", advertised: 4, payload: []byte("abc"), limit: 4, free: 100, reserve: 10, want: "decoded size 3 does not match advertised size 4"},
		{name: "low disk", advertised: 4, payload: []byte("data"), limit: 4, free: 13, reserve: 10, want: "insufficient disk"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "output.bin")
			if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			stage, err := stageRunDownload(t.Context(), encodedRunDownload(test.advertised, test.payload), path, runDownloadLimits{
				MaxBytes:         test.limit,
				DiskReserveBytes: test.reserve,
			}, func(string) (int64, error) {
				return test.free, nil
			})
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("err=%v want=%q", err, test.want)
				}
				assertRunDownloadDestinationAndTemps(t, path, "keep")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if stage.bytes != test.advertised {
				t.Fatalf("bytes=%d want=%d", stage.bytes, test.advertised)
			}
			if err := stage.publish(); err != nil {
				t.Fatal(err)
			}
			assertRunDownloadDestinationAndTemps(t, path, string(test.payload))
		})
	}
}

func TestStageRunDownloadCancellationPreservesDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.bin")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	payload := bytes.Repeat([]byte("x"), 4096)
	headerLength := len(fmt.Sprintf("%s%8d\n", remoteDownloadHeaderPrefix, len(payload)))
	source := &cancelingRunDownloadReader{
		reader:      encodedRunDownload(int64(len(payload)), payload),
		cancel:      cancel,
		cancelAfter: headerLength + 4,
	}
	stage, err := stageRunDownload(ctx, source, path, runDownloadLimits{
		MaxBytes:         int64(len(payload)),
		DiskReserveBytes: 10,
	}, func(string) (int64, error) {
		return 1 << 20, nil
	})
	if stage != nil || !strings.Contains(fmt.Sprint(err), context.Canceled.Error()) {
		t.Fatalf("stage=%#v err=%v", stage, err)
	}
	assertRunDownloadDestinationAndTemps(t, path, "keep")
}

func encodedRunDownload(advertised int64, payload []byte) io.Reader {
	var data bytes.Buffer
	fmt.Fprintf(&data, "%s%8d\n", remoteDownloadHeaderPrefix, advertised)
	encoder := base64.NewEncoder(base64.StdEncoding, &data)
	_, _ = encoder.Write(payload)
	_ = encoder.Close()
	return &data
}

type cancelingRunDownloadReader struct {
	reader      io.Reader
	cancel      context.CancelFunc
	read        int
	cancelAfter int
}

func (reader *cancelingRunDownloadReader) Read(data []byte) (int, error) {
	if len(data) > 1 {
		data = data[:1]
	}
	n, err := reader.reader.Read(data)
	reader.read += n
	if reader.read >= reader.cancelAfter {
		reader.cancel()
	}
	return n, err
}

func assertRunDownloadDestinationAndTemps(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("destination=%q err=%v want=%q", data, err, want)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".crabbox-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary downloads=%v err=%v", temps, err)
	}
}

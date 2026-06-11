//go:build windows

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var errCommandStreamTestWriter = errors.New("stream writer failed")

type commandStreamFailingWriter struct{}

func (commandStreamFailingWriter) Write([]byte) (int, error) {
	return 0, errCommandStreamTestWriter
}

func TestRunCommandWithPlatformStreams(t *testing.T) {
	if os.Getenv("CRABBOX_FILE_STREAM_HELPER") == "1" {
		fmt.Fprint(os.Stdout, "stdout-before-exit\n")
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(os.Stderr, "stderr-before-exit\n")
		os.Exit(23)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCommandWithPlatformStreams$")
	cmd.Env = append(os.Environ(), "CRABBOX_FILE_STREAM_HELPER=1")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runCommandWithPlatformStreams(cmd, &stdout, &stderr)
	if code := exitCode(err); code != 23 {
		t.Fatalf("exit code=%d error=%v want 23", code, err)
	}
	if got := stdout.String(); !strings.Contains(got, "stdout-before-exit") {
		t.Fatalf("stdout=%q", got)
	}
	if got := stderr.String(); !strings.Contains(got, "stderr-before-exit") {
		t.Fatalf("stderr=%q", got)
	}
}

func TestRunCommandWithPlatformStreamsPreservesWriterError(t *testing.T) {
	if os.Getenv("CRABBOX_FAILING_FILE_STREAM_HELPER") == "1" {
		fmt.Fprint(os.Stdout, "trigger writer")
		time.Sleep(time.Second)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCommandWithPlatformStreamsPreservesWriterError$")
	cmd.Env = append(os.Environ(), "CRABBOX_FAILING_FILE_STREAM_HELPER=1")
	err := runCommandWithPlatformStreams(cmd, commandStreamFailingWriter{}, io.Discard)
	if !errors.Is(err, errCommandStreamTestWriter) {
		t.Fatalf("error=%v want writer failure", err)
	}
	if isSSHCommandExitError(err) {
		t.Fatalf("writer failure misclassified as SSH exit: %v", err)
	}
}

func TestRunCommandWithPlatformStreamsPreservesCombinedOrder(t *testing.T) {
	if os.Getenv("CRABBOX_COMBINED_FILE_STREAM_HELPER") == "1" {
		fmt.Fprint(os.Stdout, "a")
		fmt.Fprint(os.Stderr, "b")
		fmt.Fprint(os.Stdout, "c")
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCommandWithPlatformStreamsPreservesCombinedOrder$")
	cmd.Env = append(os.Environ(), "CRABBOX_COMBINED_FILE_STREAM_HELPER=1")
	var combined bytes.Buffer
	if err := runCommandWithPlatformStreams(cmd, &combined, &combined); err != nil {
		t.Fatal(err)
	}
	if got := combined.String(); got != "abc" {
		t.Fatalf("combined=%q want abc", got)
	}
}

func TestRunCommandWithPlatformStreamsAllowsNilWriters(t *testing.T) {
	if os.Getenv("CRABBOX_NIL_FILE_STREAM_HELPER") == "1" {
		fmt.Fprint(os.Stdout, "discard stdout")
		fmt.Fprint(os.Stderr, "discard stderr")
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCommandWithPlatformStreamsAllowsNilWriters$")
	cmd.Env = append(os.Environ(), "CRABBOX_NIL_FILE_STREAM_HELPER=1")
	if err := runCommandWithPlatformStreams(cmd, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCommandStreamRotationNeededBoundsSpool(t *testing.T) {
	spool, err := newCommandStreamSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	path := strings.TrimSuffix(spool.output.Name(), "-output")
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("spool path remains visible: %s", path)
	}
	if err := spool.output.Truncate(commandStreamRotateSize); err != nil {
		t.Fatal(err)
	}
	rotate, err := commandStreamRotationNeeded(spool)
	if err != nil {
		t.Fatal(err)
	}
	if !rotate {
		t.Fatal("commandStreamRotationNeeded()=false want true")
	}
}

func TestCommandStreamSpoolRotateResetsHandles(t *testing.T) {
	spool, err := newCommandStreamSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	if _, err := spool.output.WriteString("output"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("output"))
	if _, err := io.ReadFull(spool.reader, buf); err != nil {
		t.Fatal(err)
	}
	spool.offset = int64(len(buf))
	if err := spool.rotate(); err != nil {
		t.Fatal(err)
	}
	info, err := spool.output.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || spool.offset != 0 {
		t.Fatalf("size=%d offset=%d want 0", info.Size(), spool.offset)
	}
}

func TestRunCommandWithPlatformStreamsRotatesLargeOutput(t *testing.T) {
	if os.Getenv("CRABBOX_LARGE_FILE_STREAM_HELPER") == "1" {
		chunk := bytes.Repeat([]byte("x"), 1024*1024)
		for range 20 {
			if _, err := os.Stdout.Write(chunk); err != nil {
				t.Fatal(err)
			}
			time.Sleep(25 * time.Millisecond)
		}
		return
	}
	previousRotateSize := commandStreamRotateSize
	commandStreamRotateSize = 1024 * 1024
	defer func() {
		commandStreamRotateSize = previousRotateSize
	}()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCommandWithPlatformStreamsRotatesLargeOutput$")
	cmd.Env = append(os.Environ(), "CRABBOX_LARGE_FILE_STREAM_HELPER=1")
	if err := runCommandWithPlatformStreams(cmd, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

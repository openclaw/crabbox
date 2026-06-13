package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForNamespaceProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.json")
	done := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		if err := os.WriteFile(path, []byte(`{"endpoint":"127.0.0.1:4321"}`), 0o600); err != nil {
			done <- err
		}
	}()
	output, _, err := waitForNamespaceProxy(context.Background(), path, done, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if output.Endpoint != "127.0.0.1:4321" {
		t.Fatalf("endpoint=%q", output.Endpoint)
	}
}

func TestWaitForNamespaceProxyRejectsMissingEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := waitForNamespaceProxy(context.Background(), path, make(chan error), time.Second); err == nil {
		t.Fatal("expected missing endpoint error")
	}
}

func TestWaitForNamespaceProxyRetriesPartialJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.json")
	if err := os.WriteFile(path, []byte(`{"endpoint":`), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		if err := os.WriteFile(path, []byte(`{"endpoint":"127.0.0.1:4321"}`), 0o600); err != nil {
			done <- err
		}
	}()
	output, _, err := waitForNamespaceProxy(context.Background(), path, done, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if output.Endpoint != "127.0.0.1:4321" {
		t.Fatalf("endpoint=%q", output.Endpoint)
	}
}

func TestWaitForNamespaceProxyReportsConsumedProcessExit(t *testing.T) {
	done := make(chan error, 1)
	done <- context.Canceled
	if _, processExited, err := waitForNamespaceProxy(context.Background(), filepath.Join(t.TempDir(), "missing.json"), done, time.Second); err == nil || !processExited {
		t.Fatalf("processExited=%t err=%v", processExited, err)
	}
}

func TestCopyNamespaceProxyStreamsDrainsOutputAfterInputEOF(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer conn.Close()
		if _, copyErr := io.Copy(io.Discard, conn); copyErr != nil {
			serverDone <- copyErr
			return
		}
		time.Sleep(20 * time.Millisecond)
		_, writeErr := conn.Write([]byte("remote-tail"))
		serverDone <- writeErr
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	processDone := make(chan error, 1)
	go func() { processDone <- <-serverDone }()
	processErr, processExited, copyErr := copyNamespaceProxyStreams(context.Background(), conn, strings.NewReader("request"), &output, processDone)
	if processErr != nil || copyErr != nil {
		t.Fatalf("processErr=%v copyErr=%v", processErr, copyErr)
	}
	if !processExited {
		t.Fatal("process exit was not consumed")
	}
	if output.String() != "remote-tail" {
		t.Fatalf("output=%q", output.String())
	}
}

func TestCopyNamespaceProxyStreamsReportsConsumedExitOnCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, inputWriter := io.Pipe()
	defer inputWriter.Close()
	processDone := make(chan error)
	type result struct {
		processErr    error
		processExited bool
		copyErr       error
	}
	resultDone := make(chan result, 1)
	go func() {
		processErr, processExited, copyErr := copyNamespaceProxyStreams(ctx, client, input, io.Discard, processDone)
		resultDone <- result{processErr: processErr, processExited: processExited, copyErr: copyErr}
	}()
	sent := make(chan struct{})
	go func() {
		processDone <- nil
		close(sent)
	}()
	<-sent
	cancel()
	got := <-resultDone
	if got.processErr != nil || !got.processExited || !errors.Is(got.copyErr, context.Canceled) {
		t.Fatalf("processErr=%v processExited=%t copyErr=%v", got.processErr, got.processExited, got.copyErr)
	}
}

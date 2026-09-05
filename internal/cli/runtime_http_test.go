package cli

import (
	"io"
	"testing"
	"time"
)

func TestRuntimeForAppSetsTimedHTTPClient(t *testing.T) {
	rt := runtimeForApp(App{})
	if rt.HTTP == nil {
		t.Fatal("runtimeForApp HTTP is nil")
	}
	if got, want := rt.HTTP.Timeout, 60*time.Second; got != want {
		t.Fatalf("runtimeForApp HTTP Timeout = %v, want %v", got, want)
	}
}

func TestRuntimeForProviderOperationSetsTimedHTTPClient(t *testing.T) {
	rt := RuntimeForProviderOperation(io.Discard)
	if rt.HTTP == nil {
		t.Fatal("RuntimeForProviderOperation HTTP is nil")
	}
	if got, want := rt.HTTP.Timeout, 60*time.Second; got != want {
		t.Fatalf("RuntimeForProviderOperation HTTP Timeout = %v, want %v", got, want)
	}
}

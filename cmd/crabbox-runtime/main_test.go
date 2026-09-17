package main

import (
	"encoding/json"
	"os"
	"runtime"
	"testing"

	"github.com/openclaw/crabbox/internal/remoteruntime"
)

func TestIdentity(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "identity")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	original := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = original }()
	if code := run([]string{remoteruntime.Command, "identity"}); code != 0 {
		t.Fatalf("identity exit code = %d", code)
	}
	if _, err := output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var got remoteruntime.Identity
	if err := json.NewDecoder(output).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := remoteruntime.Identity{Protocol: remoteruntime.Protocol, OS: runtime.GOOS, Arch: runtime.GOARCH, Runnable: runtime.GOOS == "linux"}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

func TestRequiresCommandMarker(t *testing.T) {
	for _, args := range [][]string{nil, {"identity"}} {
		if code := run(args); code != 74 {
			t.Fatalf("run(%q) = %d, want 74", args, code)
		}
	}
}

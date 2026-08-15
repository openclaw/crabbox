//go:build !windows

package cli

import (
	"bytes"
	"io"
	"testing"
)

func TestReplayableSSHInputRemainsInMemoryAndResets(t *testing.T) {
	want := []byte{'a', 0, 'b', '\n'}
	input, err := newReplayableSSHInput(want)
	if err != nil {
		t.Fatal(err)
	}
	defer input.close()

	first, err := input.reset()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.(*bytes.Reader); !ok {
		t.Fatalf("reader=%T, want *bytes.Reader", first)
	}
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(first, prefix); err != nil {
		t.Fatal(err)
	}

	second, err := input.reset()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("replayed input=%v, want %v", got, want)
	}
}

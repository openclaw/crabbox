package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWindowsSFTPPathContract(t *testing.T) {
	for _, name := range []string{`relative`, `C:\`, `\\server\share\file`, `C:\a\..\b`, `C:\a\NUL.txt`, `C:\a\file:stream`, `C:\a\file[1]`, `C:\a\trailing.`, "C:\\a\\bad\nname"} {
		if _, err := normalizeWindowsSFTPPath(name); err == nil {
			t.Errorf("accepted unsupported path %q", name)
		}
	}
	got, err := normalizeWindowsSFTPPath(`C:/a/o'hara file.zip`)
	if err != nil || got != `C:\a\o'hara file.zip` || windowsSFTPWirePath(got) != `/C:/a/o'hara file.zip` {
		t.Fatalf("path=%q err=%v", got, err)
	}
	if _, err := parseWindowsSFTPRequest("local", `SANDBOX:C:\file`, true); err == nil {
		t.Fatal("unsupported -L was silently ignored")
	}
}

func TestWindowsSFTPMetadataOutputIsBoundedThroughCopy(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	out := windowsSFTPOutput{overflow: cancel}
	_, err := io.Copy(&out, strings.NewReader(strings.Repeat("x", 128*1024)))
	if err == nil || context.Cause(ctx) == nil || out.buffer.Len() > 64*1024 {
		t.Fatal("metadata output bypassed the bounded writer")
	}
	if errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatal("metadata overflow lost its diagnostic cause")
	}
}

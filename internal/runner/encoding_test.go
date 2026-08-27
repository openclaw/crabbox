package runner

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

func TestMetadataRejectsLossyFilenameBeforeTransport(t *testing.T) {
	bad := "name-\xff"
	for _, request := range []Request{
		{Operation: Upload, Destination: bad},
		{Operation: Upload, Source: runnerfs.ArchiveSource{Base: bad}},
		{Operation: Download, SourcePath: bad},
		{Operation: Collect, Workdir: bad},
		{Operation: Collect, Paths: []string{bad}},
	} {
		called := false
		client := Client{Identity: testIdentity(), Transport: func(context.Context, io.Reader, io.Writer) error { called = true; return nil }}
		if _, err := client.invoke(t.Context(), request, 0, nil, nil); err == nil || called {
			t.Errorf("request=%+v err=%v transport=%v", request, err, called)
		}
	}
	var out bytes.Buffer
	if err := writeFrame(&out, runnerwire.File, FileInfo{Path: bad}, 0, nil); err == nil || out.Len() != 0 {
		t.Fatalf("lossy response: err=%v bytes=%d", err, out.Len())
	}
}

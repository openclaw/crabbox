package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/runner"
)

func TestRunnerJUnitCollectionSeparatesWarningsFromOperationFailure(t *testing.T) {
	root := t.TempDir()
	good := `<testsuite tests="1" failures="1"><testcase name="fails"><failure>details
__CRABBOX_RESULT_FILE__:not-another-file.xml
still failure text
</failure></testcase></testsuite>`
	if err := os.WriteFile(filepath.Join(root, "good.xml"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.xml"), []byte("<testsuite>broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := runner.CurrentIdentity()
	client := &runner.Client{Identity: identity, Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		return runner.Serve(ctx, input, output, identity)
	}}
	collection, err := CollectJUnitResultsWithRunner(t.Context(), client, root, ResultsConfig{JUnit: []string{"good.xml", "bad.xml"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if collection.Summary == nil || collection.Summary.Tests != 1 || collection.Summary.Failures != 1 || len(collection.Summary.Files) != 1 {
		t.Fatalf("summary=%+v", collection.Summary)
	}
	if len(collection.Warnings) != 1 || !strings.Contains(collection.Warnings[0].Error(), "skip junit bad.xml") {
		t.Fatalf("warnings=%v", collection.Warnings)
	}
	failedTransport := errors.New("helper terminal exit unknown")
	client.Transport = func(context.Context, io.Reader, io.Writer) error { return failedTransport }
	collection, err = CollectJUnitResultsWithRunner(t.Context(), client, root, ResultsConfig{JUnit: []string{"good.xml"}}, "")
	if !errors.Is(err, failedTransport) || collection.Summary != nil || len(collection.Warnings) != 0 {
		t.Fatalf("collection=%v err=%v", collection, err)
	}
}

package blacksmith

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func artifactValidationTar(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, header := range headers {
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			if _, err := tw.Write(make([]byte, header.Size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func artifactValidationGzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func TestBlacksmithArtifactArchiveCollectorFormats(t *testing.T) {
	longName := "reports/" + strings.Repeat("long-", 40) + "result"
	for _, format := range []tar.Format{tar.FormatGNU, tar.FormatPAX} {
		t.Run(format.String(), func(t *testing.T) {
			raw := artifactValidationTar(t,
				tar.Header{Name: longName, Format: format, Typeflag: tar.TypeReg, Size: 3},
				tar.Header{Name: "reports/hardlink", Format: format, Typeflag: tar.TypeLink, Linkname: longName},
				tar.Header{Name: "reports/link", Format: format, Typeflag: tar.TypeSymlink, Linkname: "/outside/" + strings.Repeat("target-", 30)},
				tar.Header{Name: "reports/nested/line\nbreak", Format: format, Typeflag: tar.TypeReg},
			)
			// GNU tar normally pads the archive to a 20-block record.
			raw = append(raw, make([]byte, (10240-len(raw)%10240)%10240)...)
			archive := artifactValidationGzip(t, raw)
			if err := validateBlacksmithArtifactArchive(t.Context(), archive, []string{"./reports/**"}, []string{"reports/**/line?break", "reports/hardlink", "reports/link"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBlacksmithArtifactArchiveCompressedLimit(t *testing.T) {
	// The advertised limit applies to the tarball, not expanded member bytes.
	raw := artifactValidationTar(t, tar.Header{Name: "report", Size: core.DelegatedRunArtifactDefaultMaxBytes + 1})
	archive := artifactValidationGzip(t, raw)
	if int64(len(archive)) >= core.DelegatedRunArtifactDefaultMaxBytes {
		t.Fatal("fixture does not exercise compression")
	}
	if err := validateBlacksmithArtifactArchive(t.Context(), archive, nil, []string{"report"}); err != nil {
		t.Fatal(err)
	}
}

func TestBlacksmithArtifactArchiveFraming(t *testing.T) {
	raw := artifactValidationTar(t, tar.Header{Name: "report", Size: 1})
	archive := artifactValidationGzip(t, raw)
	for _, endBlocks := range []int{0, 1, 2} {
		canonical := artifactValidationGzip(t, raw[:len(raw)-(2-endBlocks)*512])
		if err := validateBlacksmithArtifactArchive(t.Context(), canonical, nil, []string{"report"}); err != nil {
			t.Fatalf("canonical tar EOF with %d end blocks rejected: %v", endBlocks, err)
		}
	}
	badChecksum := bytes.Clone(archive)
	badChecksum[len(badChecksum)-8] ^= 1
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"truncated-tar-header", artifactValidationGzip(t, raw[:300])},
		{"truncated-tar-member", artifactValidationGzip(t, raw[:512])},
		{"nonzero-tar-padding", artifactValidationGzip(t, append(bytes.Clone(raw), 1))},
		{"gzip-checksum", badChecksum},
		{"truncated-gzip", archive[:len(archive)-1]},
		{"second-gzip-member", append(bytes.Clone(archive), archive...)},
		{"trailing-compressed-byte", append(bytes.Clone(archive), 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateBlacksmithArtifactArchive(t.Context(), tt.data, nil, []string{"report"}); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	empty := artifactValidationGzip(t, artifactValidationTar(t))
	if err := validateBlacksmithArtifactArchive(t.Context(), empty, []string{"optional"}, nil); err != nil {
		t.Fatalf("valid empty optional archive rejected: %v", err)
	}
	if err := validateBlacksmithArtifactArchive(t.Context(), empty, nil, []string{"required"}); err == nil {
		t.Fatal("empty archive satisfied required artifact")
	}
}

type artifactValidationCancelContext struct {
	context.Context
	checks, cancelAt int
	cancel           func()
}

func (c *artifactValidationCancelContext) Err() error {
	c.checks++
	if c.checks == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

func TestBlacksmithArtifactArchiveCancellationCause(t *testing.T) {
	archive := artifactValidationGzip(t, artifactValidationTar(t, tar.Header{Name: "report", Size: 1024}))
	probe := &artifactValidationCancelContext{Context: t.Context()}
	if err := validateBlacksmithArtifactArchive(probe, archive, nil, []string{"report"}); err != nil {
		t.Fatal(err)
	}
	if probe.checks < 2 {
		t.Fatal("fixture did not reach mid-validation cancellation checkpoints")
	}
	// Discover the checkpoints instead of depending on a fixed number of reads.
	// Each run starts live, then loses authority at a different validation boundary.
	for checkpoint := 1; checkpoint <= probe.checks; checkpoint++ {
		t.Run(fmt.Sprintf("checkpoint-%d", checkpoint), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("synthetic collection deadline expired")
			controlled := &artifactValidationCancelContext{Context: ctx, cancelAt: checkpoint, cancel: func() { cancel(cause) }}
			err := validateBlacksmithArtifactArchive(controlled, archive, nil, []string{"report"})
			if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
				t.Fatalf("validation lost cancellation classification or cause: %v", err)
			}
		})
	}
	t.Run("already-canceled-malformed-input", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := errors.New("synthetic caller cancellation")
		cancel(cause)
		err := validateBlacksmithArtifactArchive(ctx, []byte("invalid gzip"), nil, nil)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatalf("canceled input lost its original cause: %v", err)
		}
	})
}

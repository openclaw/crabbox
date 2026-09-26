//go:build !darwin && !linux

package cli

import "os"

// Do not reuse digests on filesystems without an observed change timestamp.
func syncDigestChangeTime(os.FileInfo) ([2]int64, bool) { return [2]int64{}, false }

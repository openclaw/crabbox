//go:build !darwin

package runnerfs

import "os"

func archiveParentCaseInsensitive(*os.Root) bool { return false }

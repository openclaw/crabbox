package runnerfs

import (
	"os"
	"syscall"
)

func archiveParentCaseInsensitive(parent *os.Root) bool {
	file, err := parent.Open(".")
	if err != nil {
		return false
	}
	defer file.Close()
	// Darwin's sys/unistd.h defines _PC_CASE_SENSITIVE as 11. Query the
	// bound parent even when an interrupted publication has no target entry.
	const pcCaseSensitive = 11
	value, err := syscall.Fpathconf(int(file.Fd()), pcCaseSensitive)
	return err == nil && value == 0
}

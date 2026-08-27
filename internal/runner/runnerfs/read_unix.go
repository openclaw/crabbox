//go:build !windows

package runnerfs

import "syscall"

// Opening a FIFO must not block before its descriptor can be rejected.
const nonblockingOpen = syscall.O_NONBLOCK

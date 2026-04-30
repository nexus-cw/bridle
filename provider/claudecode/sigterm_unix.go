//go:build !windows

package claudecode

import (
	"os"
	"syscall"
)

func sigterm() os.Signal { return syscall.SIGTERM }

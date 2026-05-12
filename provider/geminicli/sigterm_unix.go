//go:build !windows

package geminicli

import (
	"os"
	"syscall"
)

func sigterm() os.Signal { return syscall.SIGTERM }

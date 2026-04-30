//go:build windows

package claudecode

import "os"

// Windows doesn't support SIGTERM for subprocess signaling in the same way.
// Kill() is the graceful option available; we use it directly.
func sigterm() os.Signal { return os.Interrupt }

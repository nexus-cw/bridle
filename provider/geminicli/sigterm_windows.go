//go:build windows

package geminicli

import "os"

func sigterm() os.Signal { return os.Interrupt }

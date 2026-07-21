//go:build windows

package local

import "os"

// fileIno is unavailable on Windows (no stable inode via os.FileInfo.Sys).
func fileIno(os.FileInfo) (uint64, bool) { return 0, false }

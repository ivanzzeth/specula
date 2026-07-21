//go:build unix

package local

import (
	"os"
	"syscall"
)

// fileIno returns the inode number for hardlink dedup in UsageBytes.
func fileIno(info os.FileInfo) (uint64, bool) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return sys.Ino, true
}

//go:build unix

package agoartifact

import (
	"os"
	"syscall"
)

// hardLinkCount reports how many directory entries point at this inode. More
// than one means the bytes are reachable and mutable from outside the managed
// root, which disqualifies the file as trustworthy evidence.
func hardLinkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 1
	}
	return uint64(stat.Nlink)
}

//go:build unix

package agoserve

import (
	"os"
	"syscall"
)

// fileIdentity reads the filesystem's own identity for a path: the device and
// inode numbers.
//
// A path is not an identity. The ownership marker has to survive being asked
// "is this still the same directory?" after a rename, a copy, a restore from a
// backup, or a user deleting Ago's contents and putting their own there under
// the same names. Only the filesystem can answer that, and only through
// numbers that a copy does not preserve.
func fileIdentity(info os.FileInfo) (device uint64, inode uint64, ok bool) {
	stat, valid := info.Sys().(*syscall.Stat_t)
	if !valid {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}

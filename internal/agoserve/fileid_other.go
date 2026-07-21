//go:build !unix

package agoserve

import "os"

// fileIdentity has no portable answer outside unix. Reporting that plainly is
// the point: callers treat an unavailable identity as "cannot prove this is
// the same directory", which fails closed.
func fileIdentity(_ os.FileInfo) (device uint64, inode uint64, ok bool) {
	return 0, 0, false
}

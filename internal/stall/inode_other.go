//go:build !unix

package stall

import "os"

// inodeOf has no implementation on this platform; the caller falls back to
// trusting file size alone (the pre-inode behavior).
func inodeOf(fi os.FileInfo) (uint64, bool) {
	return 0, false
}

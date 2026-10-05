//go:build unix

package stall

import (
	"os"
	"syscall"
)

// inodeOf returns the file's inode, used to tell an APPEND (same inode) from a
// REPLACEMENT (new inode) of identical size. On every Unix platform
// os.FileInfo.Sys() is *syscall.Stat_t and Ino is a FIELD of that struct — an
// interface assertion for an Ino() method matches nothing and was the reason an
// earlier attempt was dead code (found by live probe: iface=false, stat_t=true).
// Returns ok=false where the platform does not expose it, and the caller then
// trusts size alone — degrading to the previous behavior rather than failing.
func inodeOf(fi os.FileInfo) (uint64, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino, true
	}
	return 0, false
}

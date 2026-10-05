package taskwatch

import (
	"syscall"
)

// mkfifo creates a named pipe (Linux/macOS test helper).
func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0o644)
}

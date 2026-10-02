//go:build linux

package media

import "syscall"

// freeBytes reports the free space available to an unprivileged process on
// the filesystem holding path.
func freeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

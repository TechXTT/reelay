//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package fsprobe

import "golang.org/x/sys/unix"

func FreeSpace(path string) (uint64, error) {
	var stats unix.Statfs_t
	var err = unix.Statfs(path, &stats)

	return uint64(stats.Bavail) * uint64(stats.Bsize), err
}

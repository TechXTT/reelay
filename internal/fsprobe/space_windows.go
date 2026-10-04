package fsprobe

import "golang.org/x/sys/windows"

func FreeSpace(path string) (uint64, error) {
	var available, total, free uint64
	var pointer, err = windows.UTF16PtrFromString(path)

	if err != nil {
		return 0, err
	}
	err = windows.GetDiskFreeSpaceEx(pointer, &available, &total, &free)
	return available, err
}

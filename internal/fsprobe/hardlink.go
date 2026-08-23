// Package fsprobe answers filesystem capability questions at startup rather
// than at the moment they would ruin an import.
//
// The question that matters: can we hardlink from the download folder into the
// library folder? If we can, qBittorrent keeps seeding the same bytes we just
// filed away and the import costs nothing. If we cannot, every import is a
// full copy — double the disk, and on a Synology DS214se over the network, a
// very long wait. Discovering that three hours into a download is unacceptable,
// so we find out in the first second of the process's life.
package fsprobe

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Support is the outcome of a hardlink probe.
type Support int

const (
	// Unknown means the probe could not run (a path was missing or unwritable).
	Unknown Support = iota
	// Supported means link() succeeded and both paths refer to one inode.
	Supported
	// Unsupported means the filesystem refused, or "linked" into a copy.
	Unsupported
)

func (s Support) String() string {
	switch s {
	case Supported:
		return "supported"
	case Unsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}

// Result is what the health endpoint and settings page display.
type Result struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Support Support `json:"-"`
	// Status is Support rendered for JSON.
	Status string `json:"status"`
	// Detail explains an Unsupported or Unknown result in operator language.
	Detail string `json:"detail,omitempty"`
	// CrossDevice distinguishes "different filesystems" (fixable by moving one
	// of the folders) from "this filesystem cannot do it" (fixable only by
	// changing protocol, e.g. SMB -> NFS).
	CrossDevice bool `json:"cross_device"`
}

// Hardlink probes whether a file created under fromDir can be hardlinked into
// toDir. It cleans up after itself unconditionally.
//
// Both directories must already exist; we never create the operator's folders
// as a side effect of a capability check.
// The return is named so the deferred Status fill-in lands on the value the
// caller actually receives.
func Hardlink(fromDir, toDir string) (res Result) {
	res = Result{From: fromDir, To: toDir, Support: Unknown}
	defer func() { res.Status = res.Support.String() }()

	for label, dir := range map[string]string{"download path": fromDir, "library path": toDir} {
		st, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			res.Detail = fmt.Sprintf("%s %s does not exist yet; probe skipped", label, dir)
			return res
		}
		if err != nil {
			res.Detail = fmt.Sprintf("%s %s is not readable: %v", label, dir, err)
			return res
		}
		if !st.IsDir() {
			res.Detail = fmt.Sprintf("%s %s is not a directory", label, dir)
			return res
		}
	}

	src, err := os.CreateTemp(fromDir, ".reelay-hardlink-probe-*")
	if err != nil {
		res.Detail = fmt.Sprintf("cannot write a probe file into %s: %v", fromDir, err)
		return res
	}
	srcPath := src.Name()
	defer os.Remove(srcPath)

	// Non-empty so a "link" that silently copied still has bytes to compare.
	if _, err := src.WriteString("reelay hardlink probe\n"); err != nil {
		_ = src.Close()
		res.Detail = fmt.Sprintf("cannot write a probe file into %s: %v", fromDir, err)
		return res
	}
	if err := src.Close(); err != nil {
		res.Detail = fmt.Sprintf("cannot close the probe file in %s: %v", fromDir, err)
		return res
	}

	dstPath := filepath.Join(toDir, filepath.Base(srcPath)+".link")
	// A leftover from a killed previous run would make Link fail with EEXIST.
	_ = os.Remove(dstPath)

	if err := os.Link(srcPath, dstPath); err != nil {
		res.Support = Unsupported
		res.CrossDevice = isCrossDevice(err)
		res.Detail = explain(err)
		return res
	}
	defer os.Remove(dstPath)

	// Link() returning nil is not proof. Some SMB and FUSE layers satisfy the
	// call by copying, which looks like success and quietly doubles disk use.
	// os.SameFile compares device+inode on Unix and volume+file index on
	// Windows, so it catches that.
	srcInfo, err1 := os.Stat(srcPath)
	dstInfo, err2 := os.Stat(dstPath)
	if err1 != nil || err2 != nil {
		res.Support = Unknown
		res.Detail = fmt.Sprintf("probe files vanished mid-check (%v / %v)", err1, err2)
		return res
	}
	if !os.SameFile(srcInfo, dstInfo) {
		res.Support = Unsupported
		res.Detail = "link() reported success but the two paths are separate files; " +
			"this filesystem emulates hardlinks by copying"
		return res
	}

	res.Support = Supported
	return res
}

// isCrossDevice reports whether err means "different filesystems" as opposed
// to "this filesystem will not do it".
//
// EXDEV is the portable answer. Windows raises ERROR_NOT_SAME_DEVICE (0x11),
// which Go surfaces as syscall.Errno(17) — numerically equal to Linux EXDEV,
// so errors.Is covers both. ERROR_INVALID_FUNCTION (0x01) is what an SMB share
// whose server has hardlinks disabled tends to return.
func isCrossDevice(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	// Fall back to text: driver and protocol layers are inconsistent about
	// which errno they surface, and a wrong hint here is cosmetic.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "different disk drive") ||
		strings.Contains(msg, "not the same device") ||
		strings.Contains(msg, "cross-device")
}

func explain(err error) string {
	var le *os.LinkError
	detail := err.Error()
	if errors.As(err, &le) {
		detail = le.Err.Error()
	}
	switch {
	case isCrossDevice(err):
		return fmt.Sprintf("cross-device link (%s): the download folder and the library are on "+
			"different filesystems. Put both on the same share, or imports will copy.", detail)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("permission denied (%s): the account running Reelay cannot create "+
			"links in the library folder.", detail)
	case errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM):
		return fmt.Sprintf("the filesystem does not implement hardlinks (%s). SMB/CIFS mounts "+
			"commonly refuse; an NFS export of the same volume usually works.", detail)
	default:
		return fmt.Sprintf("link failed: %s", detail)
	}
}

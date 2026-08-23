package importer

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/TechXTT/reelay/internal/config"
)

func collectVideos(contentPath string, cfg config.Library) ([]string, error) {
	if strings.TrimSpace(contentPath) == "" {
		return nil, errors.New("download client returned an empty content path")
	}
	info, err := os.Stat(contentPath)
	if err != nil {
		return nil, fmt.Errorf("inspect content path %s: %w", contentPath, err)
	}
	allowed := stringSet(cfg.VideoExtensions)
	minSize := int64(cfg.MinVideoSizeMB) * 1024 * 1024
	var out []string
	consider := func(path string, info os.FileInfo) {
		if info.Mode().IsRegular() && info.Size() >= minSize &&
			allowed[strings.ToLower(filepath.Ext(path))] && !ignoredPath(path, cfg.IgnorePatterns) {
			out = append(out, path)
		}
	}
	if !info.IsDir() {
		consider(contentPath, info)
		return out, nil
	}
	err = filepath.Walk(contentPath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		consider(path, info)
		return nil
	})
	return out, err
}

func (s *Service) land(source, dest, root string) (method, replaced string, skipped bool, err error) {
	if err = ensureInside(root, dest); err != nil {
		return "", "", false, err
	}
	if err = os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", "", false, fmt.Errorf("create destination folder: %w", err)
	}
	if current, statErr := os.Stat(dest); statErr == nil {
		sourceInfo, sourceErr := os.Stat(source)
		if sourceErr != nil {
			return "", "", false, sourceErr
		}
		if current.Size() == sourceInfo.Size() {
			return "copy", "", true, nil
		}
		replaced, err = s.recycle(dest, root)
		if err != nil {
			return "", "", false, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", "", false, statErr
	}

	if s.cfg.Library.Hardlink {
		if err = s.link(source, dest); err == nil {
			return "hardlink", replaced, false, nil
		} else if !errors.Is(err, syscall.EXDEV) && !isUnsupportedLink(err) {
			return "", replaced, false, fmt.Errorf("hardlink %s: %w", dest, err)
		}
	}
	if s.cfg.Library.AllowMove {
		if err = os.Rename(source, dest); err == nil {
			return "move", replaced, false, nil
		}
	}
	if err = copyVerified(source, dest); err != nil {
		return "", replaced, false, err
	}
	return "copy", replaced, false, nil
}

func (s *Service) recycle(dest, root string) (string, error) {
	if s.cfg.Library.RecycleDir == "" {
		if err := os.Remove(dest); err != nil {
			return "", fmt.Errorf("remove replaced file: %w", err)
		}
		return dest, nil
	}
	dir := filepath.Join(root, s.cfg.Library.RecycleDir)
	if err := ensureInside(root, dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	target := filepath.Join(dir, filepath.Base(dest))
	for i := 1; ; i++ {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			break
		}
		ext := filepath.Ext(dest)
		target = filepath.Join(dir, fmt.Sprintf("%s.%d%s",
			strings.TrimSuffix(filepath.Base(dest), ext), i, ext))
	}
	if err := os.Rename(dest, target); err != nil {
		return "", fmt.Errorf("recycle replaced file: %w", err)
	}
	return target, nil
}

func copyVerified(source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".reelay-part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return errors.Join(copyErr, closeErr)
	}
	want := h.Sum(nil)
	got, err := fileHash(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("verify copied file %s: %w", dest, err)
	}
	if !equalBytes(want, got) {
		_ = os.Remove(tmp)
		return fmt.Errorf("verify copied file %s: checksum mismatch", dest)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func fileHash(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	return h.Sum(nil), err
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

func isUnsupportedLink(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not supported") || strings.Contains(text, "invalid function")
}

func ensureInside(root, path string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to write outside library root %s: %s", root, path)
	}
	return nil
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[strings.ToLower(value)] = true
	}
	return out
}

func ignoredPath(path string, patterns []string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	for _, pattern := range patterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

func (s *Service) carrySubtitles(source, dest, root string) error {
	dir := filepath.Dir(source)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sourceBase := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	destBase := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	allowed := stringSet(s.cfg.Library.SubtitleExtensions)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		base := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if !allowed[ext] || (base != sourceBase && !strings.HasPrefix(base, sourceBase+".")) {
			continue
		}
		suffix := strings.TrimPrefix(base, sourceBase)
		target := filepath.Join(filepath.Dir(dest), destBase+suffix+ext)
		if err := ensureInside(root, target); err != nil {
			return err
		}
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := copyVerified(filepath.Join(dir, entry.Name()), target); err != nil {
			return fmt.Errorf("copy subtitle %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (s *Service) postWebhook(grab model.Grab, path string) {
	if s.cfg.Library.PostImportWebhook == "" {
		return
	}
	payload, _ := json.Marshal(map[string]any{"event": "imported", "grab_id": grab.ID,
		"subject_type": grab.SubjectType, "subject_id": grab.SubjectID, "path": path})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		s.cfg.Library.PostImportWebhook, bytes.NewReader(payload))
	if err != nil {
		s.log.Warn("build post-import webhook", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Warn("post-import webhook failed", "error", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.log.Warn("post-import webhook rejected", "status", resp.StatusCode)
	}
}

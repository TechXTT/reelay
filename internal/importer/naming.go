package importer

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/TechXTT/reelay/internal/parser"
)

var illegalSegment = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
var emptyBrackets = regexp.MustCompile(`\[\s*\]`)

func parseSource(source string, fallback parser.Parsed) parser.Parsed {
	var parsed = parser.Parse(filepath.Base(source))

	if parsed.Title == "" || parsed.Resolution == "" {
		parsed = mergeParsed(parsed, fallback)
	}
	return parsed
}

func destination(source, root, folderTemplate, fileTemplate string, maxPathLength int, values map[string]string) (string, error) {
	var ext = strings.ToLower(filepath.Ext(source))
	var folder = renderPath(folderTemplate, values)
	var name = sanitizeSegment(render(fileTemplate, values)) + ext
	var dest = capPath(filepath.Join(root, folder, name), maxPathLength)

	if err := ensureInside(root, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func templateValues(p parser.Parsed, ext string) map[string]string {
	return map[string]string{
		"Title": p.TitleRaw, "Year": number(p.Year, 4), "Season": number(p.Season, 2),
		"Episode": number(p.FirstEpisode(), 2), "Absolute": number(p.AbsoluteEp, 3),
		"Resolution": p.Resolution, "Source": p.Source, "VideoCodec": p.VideoCodec,
		"AudioCodec": p.AudioCodec, "HDR": strings.Join(p.HDR, "+"),
		"Group": p.ReleaseGroup, "Ext": strings.TrimPrefix(ext, "."),
	}
}

func render(template string, values map[string]string) string {
	for key, value := range values {
		template = strings.ReplaceAll(template, "{"+key+"}", value)
	}
	template = strings.ReplaceAll(template, "()", "")
	template = emptyBrackets.ReplaceAllString(template, "")
	template = strings.Join(strings.Fields(template), " ")
	return strings.TrimRight(strings.TrimSpace(template), " -_.")
}

func renderPath(template string, values map[string]string) string {
	parts := strings.FieldsFunc(filepath.ToSlash(render(template, values)), func(r rune) bool { return r == '/' })
	for i := range parts {
		parts[i] = sanitizeSegment(parts[i])
	}
	return filepath.Join(parts...)
}

func sanitizeSegment(value string) string {
	value = illegalSegment.ReplaceAllString(value, "-")
	value = strings.Trim(value, " .")
	if value == "" {
		return "Unknown"
	}
	return value
}

func number(value, width int) string {
	if value <= 0 {
		return ""
	}
	return fmt.Sprintf("%0*d", width, value)
}

func capPath(path string, max int) string {
	if max <= 0 || len(path) <= max {
		return path
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	ext := filepath.Ext(base)
	allowed := max - len(dir) - 1 - len(ext)
	if allowed < 1 {
		return path
	}
	name := strings.TrimSuffix(base, ext)
	for len(name) > allowed {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return filepath.Join(dir, strings.TrimRight(name, " .")+ext)
}

func mergeParsed(primary, fallback parser.Parsed) parser.Parsed {
	if primary.Title == "" {
		primary.Title, primary.TitleRaw = fallback.Title, fallback.TitleRaw
	}
	if primary.Year == 0 {
		primary.Year = fallback.Year
	}
	if primary.Resolution == "" {
		primary.Resolution = fallback.Resolution
	}
	if primary.Source == "" {
		primary.Source = fallback.Source
	}
	if primary.VideoCodec == "" {
		primary.VideoCodec = fallback.VideoCodec
	}
	if primary.ReleaseGroup == "" {
		primary.ReleaseGroup = fallback.ReleaseGroup
	}
	if primary.Season == 0 {
		primary.Season, primary.Episodes = fallback.Season, fallback.Episodes
	}
	return primary
}

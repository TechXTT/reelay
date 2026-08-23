package parser

import (
	"regexp"
	"sort"
	"strings"
)

// Quality token vocabularies.
//
// Each map is alias -> canonical value. The vocabularies are disjoint, so once
// the title boundary is known these can be applied in any order — which is why
// this step comes last and needs no careful sequencing.
//
// Every key is lowercase and separator-free; matching happens on tokens that
// have already been lowercased and had dots/underscores collapsed to spaces.

var resolutionAlias = map[string]string{
	"2160p": "2160p", "2160i": "2160p", "4k": "2160p", "uhd": "2160p",
	"1080p": "1080p", "1080i": "1080p", "fullhd": "1080p",
	"720p": "720p", "720i": "720p", "hd": "720p",
	"480p": "480p", "480i": "480p", "576p": "480p", "576i": "480p", "sd": "480p",
}

var sourceAlias = map[string]string{
	"remux": "remux", "bdremux": "remux",
	"bluray": "bluray", "blu-ray": "bluray", "bdrip": "bluray", "brrip": "bluray",
	"bd": "bluray", "bd25": "bluray", "bd50": "bluray", "bdmv": "bluray",
	"webdl": "webdl", "web-dl": "webdl", "webdlrip": "webdl", "web": "webdl",
	"webrip": "webrip", "web-rip": "webrip",
	"hdtv": "hdtv", "pdtv": "hdtv", "sdtv": "hdtv", "dsr": "hdtv", "tvrip": "hdtv",
	"dvdrip": "dvd", "dvd": "dvd", "dvd5": "dvd", "dvd9": "dvd", "dvdr": "dvd",
	"ntsc": "dvd", "pal": "dvd",
	"cam": "cam", "camrip": "cam", "hdcam": "cam", "ts": "cam", "hdts": "cam",
	"telesync": "cam", "telecine": "cam", "tc": "cam", "screener": "cam",
	"dvdscr": "cam", "scr": "cam", "r5": "cam", "workprint": "cam",
}

var videoAlias = map[string]string{
	"x265": "x265", "h265": "x265", "h 265": "x265", "hevc": "x265", "x 265": "x265",
	"x264": "x264", "h264": "x264", "h 264": "x264", "avc": "x264", "x 264": "x264",
	"av1":  "av1",
	"xvid": "xvid", "divx": "xvid",
	"mpeg2": "mpeg2", "mpeg-2": "mpeg2", "vc1": "vc1",
}

var audioAlias = map[string]string{
	"atmos": "atmos", "truehd": "truehd",
	"dtshd": "dts-hd", "dts-hd": "dts-hd", "dtshdma": "dts-hd", "dts-hd-ma": "dts-hd",
	"dtsx": "dts-x", "dts-x": "dts-x",
	"dts": "dts",
	"ddp": "ddp", "eac3": "ddp", "ddplus": "ddp", "dd+": "ddp", "e-ac3": "ddp",
	"dd": "dd", "ac3": "dd", "dolby": "dd",
	"aac": "aac", "flac": "flac", "opus": "opus", "mp3": "mp3", "pcm": "pcm",
	"lpcm": "pcm", "mp2": "mp3",
}

var hdrAlias = map[string]string{
	"hdr10plus": "hdr10plus", "hdr10+": "hdr10plus", "hdrplus": "hdr10plus",
	"hdr10": "hdr10", "hdr": "hdr10",
	"dv": "dv", "dovi": "dv", "dolbyvision": "dv", "dolby vision": "dv",
	"hlg": "hlg",
}

// languageAlias maps release-name language markers to ISO-639-1.
//
// Deliberately conservative: a two-letter token like "it" appears in ordinary
// English titles, so only unambiguous three-or-more-letter markers and a few
// well-known exceptions are listed.
var languageAlias = map[string]string{
	"english": "en", "eng": "en",
	"italian": "it", "ita": "it",
	"french": "fr", "fra": "fr", "vf": "fr", "vff": "fr", "truefrench": "fr", "vostfr": "fr",
	"german": "de", "ger": "de", "deu": "de",
	"spanish": "es", "spa": "es", "esp": "es", "castellano": "es", "latino": "es",
	"russian": "ru", "rus": "ru",
	"japanese": "ja", "jpn": "ja", "jap": "ja",
	"korean": "ko", "kor": "ko",
	"chinese": "zh", "chi": "zh", "cht": "zh", "chs": "zh", "mandarin": "zh",
	"portuguese": "pt", "por": "pt", "brazilian": "pt", "pt-br": "pt",
	"dutch": "nl", "nld": "nl", "ned": "nl",
	"polish": "pl", "pol": "pl", "lektor": "pl",
	"swedish": "sv", "swe": "sv",
	"danish": "da", "dan": "da",
	"norwegian": "no", "nor": "no",
	"finnish": "fi", "fin": "fi",
	"hindi": "hi", "hin": "hi",
	"tamil": "ta", "tam": "ta",
	"telugu": "te", "tel": "te",
	"turkish": "tr", "tur": "tr",
	"arabic": "ar", "ara": "ar",
	"hebrew": "he", "heb": "he",
	"czech": "cs", "cze": "cs",
	"hungarian": "hu", "hun": "hu",
	"greek": "el", "gre": "el",
	"thai": "th", "tha": "th",
	"ukrainian": "uk", "ukr": "uk",
	"bulgarian": "bg", "bul": "bg",
	"romanian": "ro", "rum": "ro", "ron": "ro",
}

// tokenSplitRe splits the remainder into candidate tokens. Dashes are split on
// too, because "WEB-DL" is handled by a two-token lookahead below rather than
// by keeping the hyphen.
//
// '+' is NOT a separator: it is part of "HDR10+" and "DD+", and splitting on it
// silently downgraded every HDR10+ release to plain HDR10.
var tokenSplitRe = regexp.MustCompile(`[\s\-()\[\]{},]+`)

// extractTokens pulls quality metadata out of the post-title remainder.
//
// Two-token lookahead exists because apibay flattens dots to spaces: "H.264"
// becomes "H 264" and "WEB-DL" survives as "WEB DL", so a single-token scan
// would find neither.
func extractTokens(rest string, p *Parsed) {
	tokens := tokenSplitRe.Split(strings.ToLower(rest), -1)
	hdrSeen := map[string]bool{}

	// lookupPairFirst tries the two-token form before the single token.
	//
	// Order matters and cost real bugs: "DTS-HD" splits into "dts" and "hd",
	// and checking the single token first matches plain "dts" and then never
	// looks at the pair, silently downgrading every DTS-HD release. Same story
	// for "WEB DL" versus bare "WEB".
	lookupPairFirst := func(table map[string]string, t, pair string) string {
		if pair != "" {
			if v := table[strings.ReplaceAll(pair, " ", "")]; v != "" {
				return v
			}
			if v := table[pair]; v != "" {
				return v
			}
		}
		return table[t]
	}

	for i, t := range tokens {
		if t == "" {
			continue
		}
		pair := ""
		if i+1 < len(tokens) && tokens[i+1] != "" {
			pair = t + " " + tokens[i+1]
		}

		// Resolution: first match wins, so "1080p" is not overwritten by a
		// later mention of the source's resolution.
		if p.Resolution == "" {
			if v := lookupPairFirst(resolutionAlias, t, pair); v != "" {
				p.Resolution = v
			}
		}

		// Source: keep the strongest signal seen anywhere, because "BluRay
		// REMUX" mentions both and remux is the real answer.
		if v := lookupPairFirst(sourceAlias, t, pair); v != "" {
			p.Source = strongerSource(p.Source, v)
		}

		if p.VideoCodec == "" {
			if v := lookupPairFirst(videoAlias, t, pair); v != "" {
				p.VideoCodec = v
			}
		}

		if v := lookupPairFirst(audioAlias, t, pair); v != "" {
			p.AudioCodec = strongerAudio(p.AudioCodec, v)
		}
		// "DDP5 1" / "DD5 1": the channel count is glued to the codec.
		if v := audioAlias[trimChannelSuffix(t)]; v != "" {
			p.AudioCodec = strongerAudio(p.AudioCodec, v)
		}

		if v := lookupPairFirst(hdrAlias, t, pair); v != "" && !hdrSeen[v] {
			hdrSeen[v] = true
			p.HDR = append(p.HDR, v)
		}
	}

	// hdr10plus and hdr10 both matching means the plus form is the truth.
	if hdrSeen["hdr10plus"] && hdrSeen["hdr10"] {
		p.HDR = removeString(p.HDR, "hdr10")
	}
	sort.Strings(p.HDR)
}

var channelSuffixRe = regexp.MustCompile(`^([a-z+]+?)\d(?:\s?\d)?$`)

// trimChannelSuffix turns "ddp5" into "ddp" and "dd5" into "dd".
func trimChannelSuffix(t string) string {
	if m := channelSuffixRe.FindStringSubmatch(t); m != nil {
		return m[1]
	}
	return ""
}

// sourceRank orders sources by how close they are to the master. Used only to
// resolve a name that mentions several.
var sourceRank = map[string]int{
	"remux": 7, "bluray": 6, "webdl": 5, "webrip": 4,
	"hdtv": 3, "dvd": 2, "cam": 1,
}

func strongerSource(current, candidate string) string {
	if current == "" {
		return candidate
	}
	// A cam marker is never overridden: "1080p HDCAM" is a cam, and letting
	// the "1080p" or a stray "web" token upgrade it would defeat the whole
	// banned-terms mechanism.
	if current == "cam" || candidate == "cam" {
		return "cam"
	}
	if sourceRank[candidate] > sourceRank[current] {
		return candidate
	}
	return current
}

var audioRank = map[string]int{
	"atmos": 9, "truehd": 8, "dts-x": 7, "dts-hd": 6, "dts": 5,
	"flac": 5, "pcm": 5, "ddp": 4, "dd": 3, "aac": 2, "opus": 2, "mp3": 1,
}

func strongerAudio(current, candidate string) string {
	if current == "" {
		return candidate
	}
	if audioRank[candidate] > audioRank[current] {
		return candidate
	}
	return current
}

var (
	properRe = regexp.MustCompile(`(?i)\bproper\b`)
	repackRe = regexp.MustCompile(`(?i)\brepack\d?\b`)
)

func extractFlags(s string, p *Parsed) {
	p.Proper = properRe.MatchString(s)
	p.Repack = repackRe.MatchString(s)
}

// extractLanguages scans the whole name, not just the remainder: language
// markers are commonly prefixed ("FRENCH.Show.S01E01.1080p").
func extractLanguages(s string, p *Parsed) {
	tokens := tokenSplitRe.Split(strings.ToLower(s), -1)
	seen := map[string]bool{}
	var out []string
	for i, t := range tokens {
		if t == "" {
			continue
		}
		code := languageAlias[t]
		if code == "" && i+1 < len(tokens) {
			code = languageAlias[t+" "+tokens[i+1]]
		}
		if code == "" {
			continue
		}
		// "SUB ITA" and "SUBS ENG" mark subtitles, not audio tracks. Treating
		// them as languages would make an Italian-subtitled English release
		// look bilingual and win a language-preference bonus it has not earned.
		if i > 0 && isSubtitleMarker(tokens[i-1]) {
			continue
		}
		if !seen[code] {
			seen[code] = true
			out = append(out, code)
		}
	}
	// "DUAL AUDIO" without explicit languages tells us there are two, but not
	// which — record nothing rather than guess.
	sort.Strings(out)
	p.Language = out
}

func isSubtitleMarker(t string) bool {
	switch t {
	case "sub", "subs", "subbed", "subtitle", "subtitles", "vostfr", "hardsub":
		return true
	}
	return false
}

func removeString(list []string, v string) []string {
	out := list[:0]
	for _, s := range list {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

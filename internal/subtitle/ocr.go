package subtitle

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// BundledLanguages are the tesseract language packs baked into the container
// image. It MUST track the Dockerfile's TESSERACT_LANGS build arg default: the
// image installs a fixed set and there is no on-demand install at runtime, so a
// configured language missing from this list can never be OCRed. Rebuilding the
// image with a different TESSERACT_LANGS means updating this list too.
var BundledLanguages = []string{"eng", "osd", "fra", "deu", "spa", "ita", "jpn"}

// Default binaries used when the Converter leaves a path empty.
const (
	defaultFFmpeg = "ffmpeg"
	defaultPGSRip = "pgsrip"
)

// maxErrorBytes caps how much subprocess stderr is kept in a
// ConversionResult.Error, so one chatty ffmpeg run cannot bloat the manifest.
const maxErrorBytes = 1000

// ConversionResult is the outcome of OCRing one subtitle stream. Callers turn
// it into a docs/MANIFEST.md subtitles[] update, and the muxing stage reads
// SRTPath plus the two disposition flags to build the processed MKV.
type ConversionResult struct {
	// StreamIndex is the source stream this result describes, matching
	// SubtitleStream.StreamIndex.
	StreamIndex int
	// Language is the ISO 639-2 code recorded for the source stream.
	Language string
	// SRTPath is the converted subtitle file, inside the Converter's temp dir.
	// It is empty when Converted is false.
	SRTPath string
	// Converted reports whether OCR produced an SRT.
	Converted bool
	// Forced is true when the muxed SRT track should carry the forced flag,
	// which is the case for a confirmed forced candidate.
	Forced bool
	// Default is true when the muxed SRT track should carry the default flag.
	// The docs set forced and default together on confirmed forced tracks.
	Default bool
	// Error is the reason OCR failed, including captured subprocess stderr. It
	// is nil when Converted is true.
	Error *string
}

// Converter turns PGS subtitle streams into SRT files. It runs in-process
// beside the rest of the pipeline: ffmpeg extracts each track to a .sup and
// pgsrip (a tesseract wrapper) OCRs it. The zero value is usable and shells out
// to ffmpeg and pgsrip on PATH, writing scratch files to os.TempDir.
type Converter struct {
	// Runner executes ffmpeg and pgsrip. Nil means ExecRunner.
	Runner CommandRunner
	// FFmpegPath overrides the ffmpeg binary. Empty means "ffmpeg".
	FFmpegPath string
	// PGSRipPath overrides the pgsrip binary. Empty means "pgsrip".
	PGSRipPath string
	// TempDir is where .sup and .srt scratch files are written; it is created
	// if missing. Empty means os.TempDir. It should be config's output.temp so
	// nothing temporary is ever written to the library output dir.
	TempDir string
	// Languages are the configured tesseract codes (config's
	// subtitle.ocr_languages), already checked with ValidateLanguages. Empty
	// falls back to English.
	Languages []string
	// Log receives per-stream progress and failures. Nil means slog.Default().
	Log *slog.Logger
}

// ValidateLanguages reports whether every configured tesseract language code
// has a pack in the bundled set. It is pure — no filesystem, no subprocess — so
// the daemon can call it during config validation at startup and fail fast
// rather than discovering a missing pack mid-rip. Passing a nil bundled list
// checks against BundledLanguages.
func ValidateLanguages(configured, bundled []string) error {
	if bundled == nil {
		bundled = BundledLanguages
	}
	if len(configured) == 0 {
		return errors.New("subtitle: no OCR languages configured; set subtitle.ocr_languages")
	}

	var missing []string
	for _, lang := range configured {
		lang = strings.ToLower(strings.TrimSpace(lang))
		if lang == "" {
			return errors.New("subtitle: empty entry in subtitle.ocr_languages")
		}
		if !slices.Contains(bundled, lang) && !slices.Contains(missing, lang) {
			missing = append(missing, lang)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"subtitle: no tesseract language pack for %s; the image bundles only %s "+
				"(rebuild it with TESSERACT_LANGS to add more)",
			strings.Join(missing, ", "), strings.Join(bundled, ", "))
	}
	return nil
}

// CheckTools reports whether the binaries OCR needs are present, so the daemon
// can fail at startup with the missing tool named instead of failing per rip.
func (c *Converter) CheckTools() error {
	for _, bin := range []string{c.ffmpeg(), c.pgsrip()} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("subtitle: %s not found on PATH: %w", bin, err)
		}
	}
	return nil
}

// Convert OCRs every stream with NeedsOCR set and returns one result per such
// stream, in file order. Streams that do not need OCR are skipped entirely: no
// subprocess is run for them and they get no result.
//
// It annotates streams in place — Converted and ConversionError — so the slice
// can be handed straight to WriteSidecar or used to build a manifest update,
// and it returns the same information as results carrying the SRT path and the
// disposition the muxing stage should apply.
//
// Failures are isolated per stream: a track whose extraction or OCR fails is
// recorded with Converted false and a non-nil error, and the remaining tracks
// are still converted. Convert itself only errors when nothing could be
// attempted (unusable temp dir) or when ctx is cancelled.
//
// mkvPath is only ever read. Its full path (not just its basename) is folded
// into every scratch filename (see scratchBase), so concurrent Convert calls
// — e.g. two optical drives ripping in parallel, whose MakeMKV output is
// identically named — can never collide in the shared TempDir. Each .sup is
// removed as soon as its OCR finishes, as is the .srt of a failed conversion;
// the SRTs of successful conversions are left in TempDir for the muxing
// stage, which owns their cleanup.
func (c *Converter) Convert(ctx context.Context, mkvPath string, streams []SubtitleStream) ([]ConversionResult, error) {
	var results []ConversionResult

	needed := false
	for i := range streams {
		if streams[i].NeedsOCR {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil
	}

	dir := c.tempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("subtitle: creating temp dir %s: %w", dir, err)
	}

	for i := range streams {
		if !streams[i].NeedsOCR {
			continue
		}
		if err := ctx.Err(); err != nil {
			return results, fmt.Errorf("subtitle: OCR of %s cancelled: %w", mkvPath, err)
		}

		srtPath, err := c.convertStream(ctx, mkvPath, dir, streams[i])
		result := ConversionResult{
			StreamIndex: streams[i].StreamIndex,
			Language:    streams[i].Language,
		}
		if err != nil {
			msg := truncate(err.Error(), maxErrorBytes)
			streams[i].Converted = false
			streams[i].ConversionError = &msg
			result.Error = &msg
			c.logger().Error("subtitle: OCR failed",
				"file", mkvPath, "stream_index", streams[i].StreamIndex, "error", msg)
		} else {
			streams[i].Converted = true
			streams[i].ConversionError = nil
			result.Converted = true
			result.SRTPath = srtPath
			// The docs set forced and default together on a confirmed forced
			// track, so players pick it up without the user hunting for it.
			result.Forced = streams[i].ForcedCandidate || streams[i].ForcedFlagInSource
			result.Default = result.Forced
			c.logger().Info("subtitle: OCR complete",
				"file", mkvPath, "stream_index", streams[i].StreamIndex, "srt", srtPath)
		}
		results = append(results, result)
	}
	return results, nil
}

// convertStream extracts one stream to a raw .sup, then OCRs it once per
// candidate language (see languagesFor), stopping at the first that produces
// a usable SRT. The raw extraction is shared across attempts since the PGS
// bytes themselves don't depend on language; only the per-attempt copy's
// filename does (see scratchName for why).
func (c *Converter) convertStream(ctx context.Context, mkvPath, dir string, s SubtitleStream) (string, error) {
	rawSup := filepath.Join(dir, scratchBase(mkvPath, s.StreamIndex)+".sup")
	defer func() { _ = os.Remove(rawSup) }()

	if _, err := c.runner().Run(ctx, c.ffmpeg(),
		"-nostdin",
		"-v", "error",
		"-y",
		"-i", mkvPath,
		"-map", "0:"+strconv.Itoa(s.StreamIndex),
		"-c", "copy",
		"-f", "sup",
		rawSup,
	); err != nil {
		return "", fmt.Errorf("extracting stream %d: %w", s.StreamIndex, err)
	}

	raw, err := os.ReadFile(rawSup)
	if err != nil {
		return "", fmt.Errorf("reading extracted stream %d: %w", s.StreamIndex, err)
	}

	var lastErr error
	for _, lang := range c.languagesFor(s) {
		srtPath, err := c.ripLanguage(ctx, dir, mkvPath, s.StreamIndex, lang, raw)
		if err == nil {
			return srtPath, nil
		}
		lastErr = err
	}
	return "", lastErr
}

// ripLanguage stages one language-tagged copy of an already-extracted .sup
// and runs pgsrip against it. pgsrip derives BOTH the OCR language handed to
// tesseract and the language it filters --language against from the .sup's
// filename, not from the --language flag itself (pgsrip 0.1.12,
// media_path.py MediaPath.__init__ / media.py Media.matches / ripper.py
// PgsToSrtRipper.process): the flag only has to intersect with whatever the
// filename says, and only selects cleanit rules. So a single pgsrip run can
// only ever attempt one language, and that language must be baked into the
// scratch filename via scratchName, or pgsrip silently discards the file
// (0 PGS subtitles collected, exit 0) instead of erroring.
func (c *Converter) ripLanguage(ctx context.Context, dir, mkvPath string, streamIndex int, lang string, raw []byte) (string, error) {
	base := filepath.Join(dir, scratchName(mkvPath, streamIndex, lang))
	supPath, srtPath := base+".sup", base+".srt"
	defer func() { _ = os.Remove(supPath) }()

	if err := os.WriteFile(supPath, raw, 0o644); err != nil {
		return "", fmt.Errorf("staging stream %d for %s: %w", streamIndex, lang, err)
	}

	if _, err := c.runner().Run(ctx, c.pgsrip(), "--force", "--language", lang, supPath); err != nil {
		_ = os.Remove(srtPath)
		return "", fmt.Errorf("OCRing stream %d (%s): %w", streamIndex, lang, err)
	}

	// pgsrip can exit cleanly having written nothing, or having written an
	// empty SRT when every OCR item was rejected (low confidence, a
	// graphic-only track, a wrong language pack), so confirm the SRT is
	// really there and non-empty before reporting the stream converted.
	st, err := os.Stat(srtPath)
	if err != nil || st.Size() == 0 {
		_ = os.Remove(srtPath)
		return "", fmt.Errorf("OCRing stream %d (%s): pgsrip produced no usable %s",
			streamIndex, lang, filepath.Base(srtPath))
	}
	return srtPath, nil
}

// languagesFor picks the tesseract codes for one stream: its own language when
// the configured list covers it, so OCR is not confused by unrelated packs, and
// otherwise the whole configured list, tried one pgsrip run at a time — an
// untagged or foreign track gets every language we were told to try, in
// order, until one produces a usable SRT.
//
// Both sides are normalized (trimmed, lowercased) before comparing so that a
// configured code like " ENG " — which ValidateLanguages accepts — still
// matches the stream's own (already-lowercased, see analyze.go) language
// instead of silently falling through, and so pgsrip is never handed a
// --language value it will reject outright.
func (c *Converter) languagesFor(s SubtitleStream) []string {
	configured := normalizeLanguages(c.Languages)
	if len(configured) == 0 {
		configured = []string{LanguageEnglish}
	}
	lang := strings.ToLower(strings.TrimSpace(s.Language))
	if lang != "" && lang != LanguageUndetermined && slices.Contains(configured, lang) {
		return []string{lang}
	}
	return configured
}

// normalizeLanguages trims and lowercases each entry, dropping empties.
func normalizeLanguages(languages []string) []string {
	var out []string
	for _, l := range languages {
		l = strings.ToLower(strings.TrimSpace(l))
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// scratchBase builds the shared temp-file base name for one stream: the
// source file's basename, a short hash of its full path, and the stream
// index — with no language segment. It is only ever used for the raw,
// language-agnostic extraction — never handed to pgsrip directly (see
// scratchName).
//
// The hash covers the full mkvPath, not just its basename, so two rips whose
// MakeMKV output happens to share a filename (e.g. both named title_t00.mkv,
// which MakeMKV always produces) still land on distinct scratch names in the
// shared TempDir — otherwise a second disc's extraction can overwrite the
// first's .sup mid-OCR, and a later run can mistake a stale leftover .srt
// for a fresh success.
func scratchBase(mkvPath string, streamIndex int) string {
	base := filepath.Base(mkvPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	h := fnv.New32a()
	_, _ = h.Write([]byte(mkvPath))
	return fmt.Sprintf("%s.%08x.%d", base, h.Sum32(), streamIndex)
}

// scratchName builds a temp-file base name in the form pgsrip expects to
// find a language in: {scratchBase}.{lang}, which for the .sup extension
// pgsrip parses as base_path="{scratchBase}" and language="{lang}" (pgsrip's
// MediaPath splits the extension off, then splits what's left again — that
// second, innermost extension is the language code). Only that innermost
// segment is read as a language, so everything scratchBase folds in — the
// stream index, the path hash — is never mistaken for one.
func scratchName(mkvPath string, streamIndex int, lang string) string {
	return scratchBase(mkvPath, streamIndex) + "." + lang
}

// truncate shortens s to at most n bytes, marking that it was cut. It backs
// off to a UTF-8 rune boundary first: s is subprocess stderr and regularly
// carries non-ASCII (an accented disc title, a tesseract message), and this
// string is later JSON-marshalled into the sidecar, so cutting mid-rune would
// persist an invalid UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "… (truncated)"
}

func (c *Converter) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c *Converter) ffmpeg() string {
	if c.FFmpegPath != "" {
		return c.FFmpegPath
	}
	return defaultFFmpeg
}

func (c *Converter) pgsrip() string {
	if c.PGSRipPath != "" {
		return c.PGSRipPath
	}
	return defaultPGSRip
}

func (c *Converter) tempDir() string {
	if c.TempDir != "" {
		return c.TempDir
	}
	return os.TempDir()
}

func (c *Converter) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

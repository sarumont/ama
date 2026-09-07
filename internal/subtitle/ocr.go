package subtitle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
// mkvPath is only ever read. Each .sup is removed as soon as its OCR finishes,
// as is the .srt of a failed conversion; the SRTs of successful conversions are
// left in TempDir for the muxing stage, which owns their cleanup.
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

// convertStream extracts one stream to .sup, OCRs it to .srt, and returns the
// SRT path. The .sup is always removed; on failure any partial .srt is too.
func (c *Converter) convertStream(ctx context.Context, mkvPath, dir string, s SubtitleStream) (string, error) {
	base := filepath.Join(dir, scratchName(mkvPath, s.StreamIndex))
	supPath, srtPath := base+".sup", base+".srt"
	defer os.Remove(supPath)

	if _, err := c.runner().Run(ctx, c.ffmpeg(),
		"-nostdin",
		"-v", "error",
		"-y",
		"-i", mkvPath,
		"-map", "0:"+strconv.Itoa(s.StreamIndex),
		"-c", "copy",
		"-f", "sup",
		supPath,
	); err != nil {
		return "", fmt.Errorf("extracting stream %d: %w", s.StreamIndex, err)
	}

	args := []string{"--force"}
	for _, lang := range c.languagesFor(s) {
		args = append(args, "--language", lang)
	}
	if _, err := c.runner().Run(ctx, c.pgsrip(), append(args, supPath)...); err != nil {
		os.Remove(srtPath)
		return "", fmt.Errorf("OCRing stream %d: %w", s.StreamIndex, err)
	}

	// pgsrip can exit cleanly having written nothing, so confirm the SRT is
	// really there before reporting the stream converted.
	if _, err := os.Stat(srtPath); err != nil {
		os.Remove(srtPath)
		return "", fmt.Errorf("OCRing stream %d: pgsrip produced no %s: %w",
			s.StreamIndex, filepath.Base(srtPath), err)
	}
	return srtPath, nil
}

// languagesFor picks the tesseract codes for one stream: its own language when
// the configured list covers it, so OCR is not confused by unrelated packs, and
// otherwise the whole configured list — an untagged or foreign track gets every
// language we were told to try.
func (c *Converter) languagesFor(s SubtitleStream) []string {
	configured := c.Languages
	if len(configured) == 0 {
		configured = []string{LanguageEnglish}
	}
	if s.Language != "" && s.Language != LanguageUndetermined && slices.Contains(configured, s.Language) {
		return []string{s.Language}
	}
	return configured
}

// scratchName builds a temp-file base name that is unique per source file and
// stream, keeping concurrent rips from colliding in a shared temp dir.
func scratchName(mkvPath string, streamIndex int) string {
	base := filepath.Base(mkvPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return fmt.Sprintf("%s.%d", base, streamIndex)
}

// truncate shortens s to at most n bytes, marking that it was cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
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

package subtitle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ProcessedSuffix replaces the MKV extension to form the muxed output name:
// "Iron Man 3 (2013).mkv" becomes "Iron Man 3 (2013).processed.mkv", written
// beside the original per docs/MANIFEST.md.
const ProcessedSuffix = ".processed.mkv"

// defaultMKVMerge is the binary used when Muxer.MKVMergePath is empty.
const defaultMKVMerge = "mkvmerge"

// srtTrackID is the track ID every option is applied to for an appended SRT: a
// SubRip file holds exactly one track, and mkvmerge always numbers it 0.
const srtTrackID = "0"

// mkvmergeWarningExit is mkvmerge's "completed with warnings" status. Unlike
// exit 2 (hard error) the output file is still written and usable, so a rip is
// not failed for it; the warning is logged instead.
const mkvmergeWarningExit = 1

// forcedTrackName is the track name given to the muxed forced track, so players
// show something more useful than a bare language for a signs-only track.
const forcedTrackName = "Forced"

// Muxer builds the processed MKV: it feeds the original file plus every OCRed
// SRT to mkvmerge and writes a new {movie}.processed.mkv beside the source. The
// zero value is usable and shells out to mkvmerge on PATH.
//
// The source MKV is only ever read — never modified, renamed, or replaced, per
// the "no in-place MKV modification" rule in docs/CLAUDE.md — and every stream
// of the original (video, audio, chapters, and the original PGS subtitles) is
// copied through untouched, since mkvmerge remuxes rather than transcodes.
type Muxer struct {
	// Runner executes mkvmerge. Nil means ExecRunner.
	Runner CommandRunner
	// MKVMergePath overrides the mkvmerge binary. Empty means "mkvmerge".
	MKVMergePath string
	// Log receives the mux result and any cleanup problems. Nil means
	// slog.Default().
	Log *slog.Logger
}

// Mux muxes results into a processed MKV beside mkvPath using mkvmerge on PATH.
// sourceStreams is the source's own subtitle stream list from Analyze, used to
// clear the disposition of the source's existing subtitle tracks in the output.
func Mux(ctx context.Context, mkvPath string, sourceStreams []SubtitleStream, results []ConversionResult) (string, error) {
	return (&Muxer{}).Mux(ctx, mkvPath, sourceStreams, results)
}

// Mux writes {movie}.processed.mkv beside mkvPath, containing every stream of
// the original plus one text subtitle track per successfully converted result,
// and returns the path it wrote.
//
// Results that failed OCR are skipped: their failure is already recorded on the
// stream as ConversionError. When no result converted there is nothing to add,
// so no file is produced — Mux returns an empty path and a nil error, and the
// caller should keep using the original MKV as the final output rather than
// recording a processed_feature in the manifest.
//
// Each added track carries the language of the PGS stream it came from, and the
// track built from the confirmed forced stream gets both the forced and default
// flags (a user's override of which stream that is arrives here already applied
// to ConversionResult.Forced); every other added track gets neither flag. At
// most one added track ever gets the default flag, even if more than one
// result carries Default, since a source can have a forced-flagged track per
// language and only one output track may be marked default.
//
// The source's own existing subtitle tracks have their default and forced
// flags explicitly cleared in the output: mkvmerge otherwise carries a source
// track's disposition through unchanged, and source PGS tracks commonly
// already carry the default flag, which would leave two default subtitle
// tracks in the processed file and defeat the point of this stage.
//
// mkvmerge writes to a temp file in the destination directory which is renamed
// into place only once it exits successfully, so a failed or killed mux never
// leaves a half-written .processed.mkv in the library. mkvmerge's "finished
// with warnings" exit status is not a failure: the output is complete, so it is
// logged and the mux succeeds.
//
// On success the consumed SRTs — which Converter.Convert deliberately leaves in
// its temp dir — are deleted. On failure they are left alone so a retry does
// not have to OCR them again.
func (m *Muxer) Mux(ctx context.Context, mkvPath string, sourceStreams []SubtitleStream, results []ConversionResult) (string, error) {
	converted := convertedResults(results)
	if len(converted) == 0 {
		m.logger().Info("subtitle: nothing to mux, keeping original", "file", mkvPath)
		return "", nil
	}

	if strings.HasSuffix(mkvPath, ProcessedSuffix) {
		return "", fmt.Errorf("subtitle: refusing to mux already-processed file %s", mkvPath)
	}
	outPath := ProcessedPath(mkvPath)

	sweepStaleTemp(m.logger(), outPath)
	tmpPath, err := reserveOutput(outPath)
	if err != nil {
		return "", err
	}
	cleanup := func() {
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.logger().Warn("subtitle: could not remove partial mux output",
				"file", tmpPath, "error", err)
		}
	}

	args := append([]string{"-o", tmpPath}, sourceSubtitleArgs(sourceStreams)...)
	args = append(args, mkvPath)
	args = append(args, subtitleArgs(converted)...)
	if _, err := m.runner().Run(ctx, m.mkvmerge(), args...); err != nil {
		if !isWarningExit(err) {
			cleanup()
			return "", fmt.Errorf("subtitle: muxing %s: %w", mkvPath, err)
		}
		m.logger().Warn("subtitle: mkvmerge finished with warnings",
			"file", mkvPath, "error", err)
	}

	// reserveOutput pre-creates tmpPath, so its existence alone proves nothing:
	// confirm mkvmerge actually wrote to it before promoting it into the library.
	if info, err := os.Stat(tmpPath); err != nil {
		cleanup()
		return "", fmt.Errorf("subtitle: checking mux output %s: %w", tmpPath, err)
	} else if info.Size() == 0 {
		cleanup()
		return "", fmt.Errorf("subtitle: mkvmerge produced no output for %s", mkvPath)
	}

	// The temp file is created with the mode of a private scratch file; the
	// library expects the same permissions the rip itself produced.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return "", fmt.Errorf("subtitle: setting mode on %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		cleanup()
		return "", fmt.Errorf("subtitle: renaming %s to %s: %w", tmpPath, outPath, err)
	}

	m.removeSRTs(converted)
	m.logger().Info("subtitle: mux complete",
		"file", mkvPath, "output", outPath, "tracks_added", len(converted))
	return outPath, nil
}

// ProcessedPath returns the muxed output path for an MKV: the file's extension
// is replaced with ".processed.mkv", keeping it beside the original.
func ProcessedPath(mkvPath string) string {
	return strings.TrimSuffix(mkvPath, filepath.Ext(mkvPath)) + ProcessedSuffix
}

// convertedResults keeps the results that produced an SRT, in order.
func convertedResults(results []ConversionResult) []ConversionResult {
	var out []ConversionResult
	for _, r := range results {
		if r.Converted && r.SRTPath != "" {
			out = append(out, r)
		}
	}
	return out
}

// sourceSubtitleArgs clears the default and forced flags on every one of the
// source's existing subtitle tracks, keyed by SubtitleStream.StreamIndex — the
// same track ID mkvmerge assigns, since both number tracks by their order in
// the container. Without this, mkvmerge carries each source track's original
// disposition straight through, and a source PGS track flagged default (common
// on Blu-ray discs) would leave two default subtitle tracks in the output: the
// original bitmap track and the new OCRed one.
func sourceSubtitleArgs(streams []SubtitleStream) []string {
	var args []string
	for _, s := range streams {
		id := strconv.Itoa(s.StreamIndex)
		args = append(args,
			"--default-track-flag", id+":0",
			"--forced-display-flag", id+":0",
		)
	}
	return args
}

// subtitleArgs builds the mkvmerge arguments appending each SRT as a new track.
// mkvmerge applies per-file options to the input file that follows them, so
// every flag for a track is emitted immediately before its own SRT path. At
// most one track is ever emitted with the default flag set: ForcedCandidate is
// capped at one per file, but ForcedFlagInSource is not, so more than one
// result can carry Default here — only the first is honored.
func subtitleArgs(converted []ConversionResult) []string {
	var args []string
	defaultAssigned := false
	for _, r := range converted {
		isDefault := r.Default && !defaultAssigned
		defaultAssigned = defaultAssigned || isDefault

		args = append(args,
			"--language", srtTrackID+":"+muxLanguage(r.Language),
			// pgsrip writes UTF-8; saying so keeps mkvmerge from guessing.
			"--sub-charset", srtTrackID+":UTF-8",
		)
		if r.Forced {
			args = append(args, "--track-name", srtTrackID+":"+forcedTrackName)
		}
		args = append(args,
			"--forced-display-flag", srtTrackID+":"+mkvBool(r.Forced),
			"--default-track-flag", srtTrackID+":"+mkvBool(isDefault),
			r.SRTPath,
		)
	}
	return args
}

// languageTagPattern is a loose shape check for the ISO 639-2 / BCP 47 codes
// mkvmerge accepts for --language: a 2-3 letter base code, optionally followed
// by one or more "-" separated subtags. It is intentionally permissive — it
// exists only to catch tags mkvmerge would flatly reject, not to validate
// against the real registry.
var languageTagPattern = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]+)+$|^[a-z]{2,3}$`)

// muxLanguage returns the language tag to set on a muxed track, falling back to
// "und" for an untagged source stream or a tag mkvmerge would reject outright.
// A single mislabelled stream then costs one mislabelled track instead of
// failing the whole mux: mkvmerge exits 2 on an unrecognized --language value,
// which would otherwise lose every OCRed track over one bad tag.
func muxLanguage(lang string) string {
	if lang = strings.ToLower(strings.TrimSpace(lang)); lang != "" && languageTagPattern.MatchString(lang) {
		return lang
	}
	return LanguageUndetermined
}

// mkvBool renders a flag the way mkvmerge documents its boolean arguments.
func mkvBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// sweepStaleTemp removes any leftover reserveOutput temp file beside outPath
// from a previous mux of the same file that never got to clean up after
// itself — a SIGKILL, OOM kill, or power loss mid-mux leaves one behind, since
// cleanup only runs on paths where this process is still alive. It is
// best-effort: a removal failure is logged and otherwise ignored, since a
// leftover temp file does not block the mux about to run.
func sweepStaleTemp(log *slog.Logger, outPath string) {
	dir := filepath.Dir(outPath)
	matches, err := filepath.Glob(filepath.Join(dir, "."+filepath.Base(outPath)+".tmp*"))
	if err != nil {
		return
	}
	for _, stale := range matches {
		if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("subtitle: could not remove orphaned mux temp file",
				"file", stale, "error", err)
		} else if err == nil {
			log.Warn("subtitle: removed orphaned mux temp file from a previous interrupted run",
				"file", stale)
		}
	}
}

// reserveOutput creates the temp file mkvmerge writes to, in the same directory
// as the final output so the rename into place is atomic.
func reserveOutput(outPath string) (string, error) {
	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(outPath)+".tmp*")
	if err != nil {
		return "", fmt.Errorf("subtitle: creating temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("subtitle: closing %s: %w", name, err)
	}
	return name, nil
}

// removeSRTs deletes the temp SRTs the mux consumed. A file that will not go
// away is only worth a warning: the processed MKV is already in place.
func (m *Muxer) removeSRTs(converted []ConversionResult) {
	for _, r := range converted {
		if err := os.Remove(r.SRTPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.logger().Warn("subtitle: could not remove converted SRT",
				"file", r.SRTPath, "error", err)
		}
	}
}

// isWarningExit reports whether err is mkvmerge exiting 1, which means it
// finished and wrote its output but had something to complain about. Exit 2 —
// and anything that is not an exit status at all — is a real failure.
func isWarningExit(err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == mkvmergeWarningExit
}

func (m *Muxer) runner() CommandRunner {
	if m.Runner != nil {
		return m.Runner
	}
	return ExecRunner{}
}

func (m *Muxer) mkvmerge() string {
	if m.MKVMergePath != "" {
		return m.MKVMergePath
	}
	return defaultMKVMerge
}

func (m *Muxer) logger() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

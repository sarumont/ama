package subtitle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
func Mux(ctx context.Context, mkvPath string, results []ConversionResult) (string, error) {
	return (&Muxer{}).Mux(ctx, mkvPath, results)
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
// to ConversionResult.Forced); every other added track gets neither flag.
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
func (m *Muxer) Mux(ctx context.Context, mkvPath string, results []ConversionResult) (string, error) {
	converted := convertedResults(results)
	if len(converted) == 0 {
		m.logger().Info("subtitle: nothing to mux, keeping original", "file", mkvPath)
		return "", nil
	}

	outPath := ProcessedPath(mkvPath)
	if outPath == mkvPath {
		return "", fmt.Errorf("subtitle: refusing to mux %s onto itself", mkvPath)
	}

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

	args := append([]string{"-o", tmpPath, mkvPath}, subtitleArgs(converted)...)
	if _, err := m.runner().Run(ctx, m.mkvmerge(), args...); err != nil {
		if !isWarningExit(err) {
			cleanup()
			return "", fmt.Errorf("subtitle: muxing %s: %w", mkvPath, err)
		}
		m.logger().Warn("subtitle: mkvmerge finished with warnings",
			"file", mkvPath, "error", err)
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

// subtitleArgs builds the mkvmerge arguments appending each SRT as a new track.
// mkvmerge applies per-file options to the input file that follows them, so
// every flag for a track is emitted immediately before its own SRT path.
func subtitleArgs(converted []ConversionResult) []string {
	var args []string
	for _, r := range converted {
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
			"--default-track-flag", srtTrackID+":"+mkvBool(r.Default),
			r.SRTPath,
		)
	}
	return args
}

// muxLanguage returns the language tag to set on a muxed track, falling back to
// "und" so an untagged source stream still produces a valid track.
func muxLanguage(lang string) string {
	if lang = strings.ToLower(strings.TrimSpace(lang)); lang != "" {
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
		os.Remove(name)
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

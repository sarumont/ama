// Package subtitle analyzes the subtitle streams of a ripped MKV and records
// what it finds in a {movie}.subtitles.json sidecar next to the file.
//
// Analysis is deliberately split from writing the sidecar: Analyze only probes
// the file, and WriteSidecar persists whatever the caller hands it. That leaves
// room for later passes (forced-candidate detection, OCR results) to enrich the
// stream list before it is written.
package subtitle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// CodecPGS is the ffprobe codec name for Blu-ray bitmap subtitles. These are
// the only streams that need OCR to become text.
const CodecPGS = "hdmv_pgs_subtitle"

// LanguageUndetermined is recorded when a stream carries no usable language
// tag, matching the ISO 639-2 code ffmpeg itself uses.
const LanguageUndetermined = "und"

// SidecarSuffix replaces the MKV extension to form the sidecar file name.
const SidecarSuffix = ".subtitles.json"

// defaultFFprobe is the binary used when Analyzer.FFprobePath is empty.
const defaultFFprobe = "ffprobe"

// SubtitleStream is one subtitle stream of an MKV, in the shape of a
// docs/MANIFEST.md subtitles[] entry. Fields derived by later pipeline stages
// (forced-candidate pairing, OCR results) are added by those stages.
type SubtitleStream struct {
	// StreamIndex is the stream's index within the whole file, not within the
	// subtitle streams alone, so it can be passed straight to ffmpeg.
	StreamIndex int    `json:"stream_index"`
	Codec       string `json:"codec"`
	// Language is an ISO 639-2 code, or "und" when the source has no tag.
	Language string `json:"language"`
	// SizeBytes is the stream's payload size. It is 0 when neither the MKV
	// statistics tags nor a packet scan could produce a figure.
	SizeBytes           int64 `json:"size_bytes"`
	ForcedFlagInSource  bool  `json:"forced_flag_in_source"`
	DefaultFlagInSource bool  `json:"default_flag_in_source"`
	HearingImpaired     bool  `json:"hearing_impaired"`
	// ForcedCandidate is set by DetectForcedCandidates when the size-ratio
	// heuristic infers that this stream is the forced-subtitle track.
	ForcedCandidate bool `json:"forced_candidate"`
	// ForcedCandidateReason explains why ForcedCandidate was set. It is nil
	// whenever ForcedCandidate is false.
	ForcedCandidateReason *string `json:"forced_candidate_reason"`
	// PairedWithStreamIndex is the stream this one was compared against during
	// forced detection. Both streams of a matched pair point at each other; it
	// is nil for streams that were never paired.
	PairedWithStreamIndex *int `json:"paired_with_stream_index"`
	// NeedsOCR is true for bitmap (PGS) streams, which must be OCRed before
	// they can be muxed as text subtitles.
	NeedsOCR bool `json:"needs_ocr"`
}

// Sidecar is the on-disk form of {movie}.subtitles.json. It wraps the stream
// list in an object so the file can grow new top-level fields later.
type Sidecar struct {
	Subtitles []SubtitleStream `json:"subtitles"`
}

// CommandRunner runs an external command and returns its standard output. It
// exists so tests can supply recorded ffprobe output instead of a real process.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands as real subprocesses. Cancelling ctx kills the
// process, and a non-zero exit produces an error carrying the command's stderr.
type ExecRunner struct{}

// Run implements CommandRunner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.Bytes(), fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

// Analyzer enumerates the subtitle streams of MKV files. The zero value is
// usable and shells out to ffprobe on PATH.
type Analyzer struct {
	// Runner executes ffprobe. Nil means ExecRunner.
	Runner CommandRunner
	// FFprobePath overrides the ffprobe binary. Empty means "ffprobe".
	FFprobePath string
	// Log receives warnings about individual streams. Nil means slog.Default().
	Log *slog.Logger
}

// Analyze probes mkvPath with ffprobe and returns one entry per subtitle
// stream, in file order. A file with no subtitle streams yields an empty slice
// and no error; a failed, empty, or unparsable ffprobe run is an error.
func Analyze(ctx context.Context, mkvPath string) ([]SubtitleStream, error) {
	return (&Analyzer{}).Analyze(ctx, mkvPath)
}

// Analyze probes mkvPath and returns its subtitle streams.
func (a *Analyzer) Analyze(ctx context.Context, mkvPath string) ([]SubtitleStream, error) {
	out, err := a.runner().Run(ctx, a.ffprobe(),
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		mkvPath,
	)
	if err != nil {
		return nil, fmt.Errorf("subtitle: probing %s: %w", mkvPath, err)
	}

	var probe ffprobeOutput
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("subtitle: parsing ffprobe output for %s: %w", mkvPath, err)
	}
	if probe.Streams == nil {
		return nil, fmt.Errorf("subtitle: ffprobe reported no streams object for %s", mkvPath)
	}

	streams := make([]SubtitleStream, 0, len(probe.Streams))
	for _, s := range probe.Streams {
		if s.CodecType != "subtitle" {
			continue
		}
		streams = append(streams, SubtitleStream{
			StreamIndex:         s.Index,
			Codec:               s.CodecName,
			Language:            language(s.Tags),
			SizeBytes:           a.size(ctx, mkvPath, s),
			ForcedFlagInSource:  s.Disposition.Forced != 0,
			DefaultFlagInSource: s.Disposition.Default != 0,
			HearingImpaired:     s.Disposition.HearingImpaired != 0,
			NeedsOCR:            s.CodecName == CodecPGS,
		})
	}
	return streams, nil
}

// WriteSidecar writes streams to {movie}.subtitles.json beside mkvPath,
// creating the file atomically so a reader never sees a partial write.
func WriteSidecar(mkvPath string, streams []SubtitleStream) error {
	if streams == nil {
		streams = []SubtitleStream{}
	}
	data, err := json.MarshalIndent(Sidecar{Subtitles: streams}, "", "  ")
	if err != nil {
		return fmt.Errorf("subtitle: encoding sidecar for %s: %w", mkvPath, err)
	}
	return writeFileAtomic(SidecarPath(mkvPath), append(data, '\n'))
}

// SidecarPath returns the sidecar path for an MKV: the file's extension is
// replaced with ".subtitles.json".
func SidecarPath(mkvPath string) string {
	return strings.TrimSuffix(mkvPath, filepath.Ext(mkvPath)) + SidecarSuffix
}

// writeFileAtomic writes data to a temp file in the destination directory and
// renames it into place, so the result is either the old file or the new one.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("subtitle: creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("subtitle: writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("subtitle: syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("subtitle: closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("subtitle: setting mode on %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("subtitle: renaming %s to %s: %w", tmpName, path, err)
	}
	return nil
}

func (a *Analyzer) runner() CommandRunner {
	if a.Runner != nil {
		return a.Runner
	}
	return ExecRunner{}
}

func (a *Analyzer) ffprobe() string {
	if a.FFprobePath != "" {
		return a.FFprobePath
	}
	return defaultFFprobe
}

func (a *Analyzer) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// size resolves a stream's payload size, preferring the NUMBER_OF_BYTES
// statistics tag that mkvmerge (and so MakeMKV) writes. When that tag is
// absent it falls back to summing the stream's packet sizes, which costs a
// full read of the file. A stream whose size cannot be determined is reported
// as 0 with a warning rather than failing the whole analysis.
func (a *Analyzer) size(ctx context.Context, mkvPath string, s ffprobeStream) int64 {
	if n, ok := taggedSize(s.Tags); ok {
		return n
	}
	n, err := a.packetSize(ctx, mkvPath, s.Index)
	if err != nil {
		a.logger().Warn("subtitle: could not determine stream size",
			"file", mkvPath, "stream_index", s.Index, "error", err)
		return 0
	}
	return n
}

// packetSize sums the sizes of every packet belonging to one stream.
func (a *Analyzer) packetSize(ctx context.Context, mkvPath string, index int) (int64, error) {
	out, err := a.runner().Run(ctx, a.ffprobe(),
		"-v", "error",
		"-select_streams", strconv.Itoa(index),
		"-show_entries", "packet=size",
		"-of", "csv=p=0",
		mkvPath,
	)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" {
			continue
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("unexpected packet size %q: %w", line, err)
		}
		total += n
	}
	return total, nil
}

// taggedSize reads the mkvmerge statistics tag, which ffprobe surfaces either
// as NUMBER_OF_BYTES or, when the tag is language-qualified, as
// NUMBER_OF_BYTES-eng. Tag case varies between ffprobe builds.
func taggedSize(tags map[string]string) (int64, bool) {
	for k, v := range tags {
		if !strings.HasPrefix(strings.ToLower(k), "number_of_bytes") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		return n, true
	}
	return 0, false
}

// language reads the stream's language tag, falling back to "und" so a stream
// with no tag is still recorded.
func language(tags map[string]string) string {
	for k, v := range tags {
		if strings.EqualFold(k, "language") {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				return v
			}
		}
	}
	return LanguageUndetermined
}

// ffprobeOutput is the subset of `ffprobe -show_streams -print_format json`
// that analysis needs.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
}

type ffprobeStream struct {
	Index       int                `json:"index"`
	CodecName   string             `json:"codec_name"`
	CodecType   string             `json:"codec_type"`
	Tags        map[string]string  `json:"tags"`
	Disposition ffprobeDisposition `json:"disposition"`
}

type ffprobeDisposition struct {
	Default         int `json:"default"`
	Forced          int `json:"forced"`
	HearingImpaired int `json:"hearing_impaired"`
}

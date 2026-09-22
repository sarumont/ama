package subtitle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeMuxRunner stands in for mkvmerge: it records how it was called and, when
// it is not told to fail, writes the output file the real tool would write.
type fakeMuxRunner struct {
	// err is returned instead of running, simulating a non-zero exit.
	err error
	// writeOutputOnErr mirrors mkvmerge exiting non-zero having already written
	// (possibly partial) output.
	writeOutputOnErr bool

	calls [][]string
}

func (f *fakeMuxRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))

	out := ""
	if i := slices.Index(args, "-o"); i >= 0 && i+1 < len(args) {
		out = args[i+1]
	}
	if f.err != nil {
		if f.writeOutputOnErr && out != "" {
			if err := os.WriteFile(out, []byte("partial mkv"), 0o600); err != nil {
				return nil, err
			}
		}
		return nil, f.err
	}
	if out == "" {
		return nil, errors.New("mkvmerge: no -o output path")
	}
	return nil, os.WriteFile(out, []byte("muxed mkv"), 0o600)
}

// exitError is an error carrying a process exit status, as *exec.ExitError does.
type exitError struct {
	code int
}

func (e exitError) Error() string { return "exit status" }
func (e exitError) ExitCode() int { return e.code }

func testMuxer(t *testing.T, runner CommandRunner) *Muxer {
	t.Helper()
	return &Muxer{
		Runner: runner,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// muxFixture lays out a source MKV in a library dir plus SRTs in a separate
// temp dir, matching how Converter leaves its output.
func muxFixture(t *testing.T, srtNames ...string) (mkvPath string, srtPaths []string) {
	t.Helper()

	libDir, tempDir := t.TempDir(), t.TempDir()
	mkvPath = filepath.Join(libDir, "Iron Man 3 (2013).mkv")
	if err := os.WriteFile(mkvPath, []byte("original rip"), 0o644); err != nil {
		t.Fatalf("writing source MKV: %v", err)
	}
	for _, name := range srtNames {
		p := filepath.Join(tempDir, name)
		if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
			t.Fatalf("writing SRT: %v", err)
		}
		srtPaths = append(srtPaths, p)
	}
	return mkvPath, srtPaths
}

// optionsBefore returns the arguments that apply to the given input file: every
// argument after the previous file argument, up to the file itself.
func optionsBefore(t *testing.T, call []string, file string) []string {
	t.Helper()

	i := slices.Index(call, file)
	if i < 0 {
		t.Fatalf("%s missing from mkvmerge call: %v", file, call)
	}
	// Per-file options are "--opt value" pairs, so walk back in twos until the
	// preceding argument is no longer an option name.
	start := i
	for start >= 2 && strings.HasPrefix(call[start-2], "--") {
		start -= 2
	}
	return call[start:i]
}

// hasOption reports whether args contains "name value" in that order.
func hasOption(args []string, name, value string) bool {
	for i, a := range args {
		if a == name && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestMuxWritesProcessedFile(t *testing.T) {
	mkvPath, srts := muxFixture(t, "forced.srt", "full.srt")
	runner := &fakeMuxRunner{}
	sourceStreams := []SubtitleStream{
		{StreamIndex: 7, DefaultFlagInSource: true},
		{StreamIndex: 12},
	}
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
		{StreamIndex: 12, Language: "eng", SRTPath: srts[1], Converted: true},
	}

	before, err := os.ReadFile(mkvPath)
	if err != nil {
		t.Fatalf("reading source: %v", err)
	}

	out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results)
	if err != nil {
		t.Fatalf("Mux: %v", err)
	}

	want := filepath.Join(filepath.Dir(mkvPath), "Iron Man 3 (2013).processed.mkv")
	if out != want {
		t.Errorf("got output %q, want %q", out, want)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("processed file not in place: %v", err)
	}

	// The source is never modified, renamed, or deleted.
	after, err := os.ReadFile(mkvPath)
	if err != nil {
		t.Fatalf("source MKV gone: %v", err)
	}
	if string(after) != string(before) {
		t.Error("source MKV was modified")
	}

	if len(runner.calls) != 1 {
		t.Fatalf("got %d mkvmerge calls, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call[0] != "mkvmerge" {
		t.Errorf("got binary %q, want mkvmerge", call[0])
	}

	// Output goes to a temp path, never onto the source, and the source is the
	// first input so its streams lead the output.
	o := slices.Index(call, "-o")
	if o != 1 {
		t.Fatalf("expected -o first: %v", call)
	}
	if call[o+1] == mkvPath || call[o+1] == want {
		t.Errorf("mkvmerge wrote directly to %q, want a temp path", call[o+1])
	}
	// The source is the first *file* input (its own disposition-clearing flags
	// precede it), so its streams lead the output.
	srcIdx := slices.Index(call, mkvPath)
	if srcIdx < 0 {
		t.Fatalf("source %q missing from mkvmerge call: %v", mkvPath, call)
	}
	if slices.Contains(call[o+2:srcIdx], srts[0]) || slices.Contains(call[o+2:srcIdx], srts[1]) {
		t.Errorf("an SRT appears before the source input: %v", call)
	}

	// The source's own existing subtitle tracks have their disposition
	// explicitly cleared, so a source track already flagged default (as PGS
	// tracks commonly are) does not survive into the output as a second
	// default track alongside the new OCRed one.
	source := optionsBefore(t, call, mkvPath)
	for _, want := range [][2]string{
		{"--default-track-flag", "7:0"},
		{"--forced-display-flag", "7:0"},
		{"--default-track-flag", "12:0"},
		{"--forced-display-flag", "12:0"},
	} {
		if !hasOption(source, want[0], want[1]) {
			t.Errorf("source input missing %s %s: %v", want[0], want[1], source)
		}
	}

	// Per-file options precede the SRT they apply to: the forced track gets both
	// flags and a name, the other track gets neither flag.
	forced := optionsBefore(t, call, srts[0])
	for _, want := range [][2]string{
		{"--language", "0:eng"},
		{"--sub-charset", "0:UTF-8"},
		{"--track-name", "0:Forced"},
		{"--forced-display-flag", "0:1"},
		{"--default-track-flag", "0:1"},
	} {
		if !hasOption(forced, want[0], want[1]) {
			t.Errorf("forced track missing %s %s: %v", want[0], want[1], forced)
		}
	}

	full := optionsBefore(t, call, srts[1])
	for _, want := range [][2]string{
		{"--language", "0:eng"},
		{"--forced-display-flag", "0:0"},
		{"--default-track-flag", "0:0"},
	} {
		if !hasOption(full, want[0], want[1]) {
			t.Errorf("non-forced track missing %s %s: %v", want[0], want[1], full)
		}
	}
	if slices.Contains(full, "--track-name") {
		t.Errorf("non-forced track should not be named: %v", full)
	}

	// The SRTs the mux consumed are cleaned up, and no temp output is left.
	for _, srt := range srts {
		if _, err := os.Stat(srt); !os.IsNotExist(err) {
			t.Errorf("SRT %s not cleaned up: %v", srt, err)
		}
	}
	assertOnlyFiles(t, filepath.Dir(mkvPath), "Iron Man 3 (2013).mkv", "Iron Man 3 (2013).processed.mkv")
}

func TestMuxUndeterminedLanguage(t *testing.T) {
	mkvPath, srts := muxFixture(t, "und.srt")
	runner := &fakeMuxRunner{}
	sourceStreams := []SubtitleStream{{StreamIndex: 3}}
	results := []ConversionResult{{StreamIndex: 3, SRTPath: srts[0], Converted: true}}

	if _, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results); err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if !hasOption(runner.calls[0], "--language", "0:und") {
		t.Errorf("untagged track: want --language 0:und, got %v", runner.calls[0])
	}
}

func TestMuxInvalidLanguageFallsBackToUndetermined(t *testing.T) {
	mkvPath, srts := muxFixture(t, "junk.srt")
	runner := &fakeMuxRunner{}
	sourceStreams := []SubtitleStream{{StreamIndex: 3}}
	// A junk language tag must not fail the whole mux; it degrades to "und".
	results := []ConversionResult{{StreamIndex: 3, Language: "not-a-real-code!!", SRTPath: srts[0], Converted: true}}

	if _, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results); err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if !hasOption(runner.calls[0], "--language", "0:und") {
		t.Errorf("junk language tag: want --language 0:und, got %v", runner.calls[0])
	}
}

func TestMuxSkippedWhenNothingConverted(t *testing.T) {
	msg := "pgsrip: exit status 1"
	cases := map[string][]ConversionResult{
		"nil results": nil,
		"all failed": {
			{StreamIndex: 7, Language: "eng", Error: &msg},
			{StreamIndex: 12, Language: "eng", Error: &msg},
		},
	}

	for name, results := range cases {
		t.Run(name, func(t *testing.T) {
			mkvPath, _ := muxFixture(t)
			runner := &fakeMuxRunner{}

			out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, nil, results)
			if err != nil {
				t.Fatalf("Mux: %v", err)
			}
			if out != "" {
				t.Errorf("got output %q, want none", out)
			}
			if len(runner.calls) != 0 {
				t.Errorf("mkvmerge ran with nothing to mux: %v", runner.calls)
			}
			assertOnlyFiles(t, filepath.Dir(mkvPath), "Iron Man 3 (2013).mkv")
		})
	}
}

func TestMuxSkipsFailedResults(t *testing.T) {
	mkvPath, srts := muxFixture(t, "ok.srt")
	runner := &fakeMuxRunner{}
	msg := "pgsrip: exit status 1"
	sourceStreams := []SubtitleStream{{StreamIndex: 7}, {StreamIndex: 12}}
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", Error: &msg},
		{StreamIndex: 12, Language: "eng", SRTPath: srts[0], Converted: true},
	}

	if _, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results); err != nil {
		t.Fatalf("Mux: %v", err)
	}
	call := runner.calls[0]
	// One SRT input only: source + one subtitle file.
	var srtInputs int
	for _, a := range call {
		if strings.HasSuffix(a, ".srt") {
			srtInputs++
		}
	}
	if srtInputs != 1 {
		t.Errorf("got %d SRT inputs, want 1: %v", srtInputs, call)
	}
}

func TestMuxFailureLeavesNoPartialOutput(t *testing.T) {
	mkvPath, srts := muxFixture(t, "forced.srt")
	runner := &fakeMuxRunner{err: exitError{code: 2}, writeOutputOnErr: true}
	sourceStreams := []SubtitleStream{{StreamIndex: 7}}
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
	}

	out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results)
	if err == nil {
		t.Fatal("Mux succeeded, want error")
	}
	if out != "" {
		t.Errorf("got output path %q on failure, want none", out)
	}
	if !strings.Contains(err.Error(), mkvPath) {
		t.Errorf("error does not name the file: %v", err)
	}

	// No processed file, and no partial temp file, left behind.
	assertOnlyFiles(t, filepath.Dir(mkvPath), "Iron Man 3 (2013).mkv")

	// The SRTs survive so a retry does not have to OCR again.
	if _, err := os.Stat(srts[0]); err != nil {
		t.Errorf("SRT removed after a failed mux: %v", err)
	}
}

func TestMuxWarningExitSucceeds(t *testing.T) {
	mkvPath, srts := muxFixture(t, "forced.srt")
	// mkvmerge exit 1 means "finished with warnings": the output is complete.
	runner := &fakeMuxRunner{err: exitError{code: 1}, writeOutputOnErr: true}
	sourceStreams := []SubtitleStream{{StreamIndex: 7}}
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
	}

	out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results)
	if err != nil {
		t.Fatalf("Mux on warning exit: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("processed file not in place: %v", err)
	}
	assertOnlyFiles(t, filepath.Dir(mkvPath), "Iron Man 3 (2013).mkv", "Iron Man 3 (2013).processed.mkv")
}

// TestMuxCapsDefaultAtOneTrack covers the case where more than one source
// stream is forced-flagged (a multi-language disc), which makes
// ConversionResult.Default true for more than one result: only the first
// converted track may end up default-flagged in the output, or players have
// no deterministic default subtitle to pick.
func TestMuxCapsDefaultAtOneTrack(t *testing.T) {
	mkvPath, srts := muxFixture(t, "eng.srt", "fra.srt", "spa.srt")
	runner := &fakeMuxRunner{}
	sourceStreams := []SubtitleStream{{StreamIndex: 7}, {StreamIndex: 8}, {StreamIndex: 9}}
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
		{StreamIndex: 8, Language: "fra", SRTPath: srts[1], Converted: true, Forced: true, Default: true},
		{StreamIndex: 9, Language: "spa", SRTPath: srts[2], Converted: true, Forced: true, Default: true},
	}

	if _, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results); err != nil {
		t.Fatalf("Mux: %v", err)
	}

	call := runner.calls[0]
	eng := optionsBefore(t, call, srts[0])
	fra := optionsBefore(t, call, srts[1])
	spa := optionsBefore(t, call, srts[2])

	if !hasOption(eng, "--default-track-flag", "0:1") {
		t.Errorf("first forced track should be default: %v", eng)
	}
	if !hasOption(fra, "--default-track-flag", "0:0") {
		t.Errorf("second forced track should not be default: %v", fra)
	}
	if !hasOption(spa, "--default-track-flag", "0:0") {
		t.Errorf("third forced track should not be default: %v", spa)
	}
	// The forced flag itself is untouched: every language can still carry it.
	for name, opts := range map[string][]string{"eng": eng, "fra": fra, "spa": spa} {
		if !hasOption(opts, "--forced-display-flag", "0:1") {
			t.Errorf("%s track should still be forced: %v", name, opts)
		}
	}
}

// TestMuxRefusesAlreadyProcessedInput covers the "outPath == mkvPath" guard's
// real hazard: muxing a path that already carries ProcessedSuffix (e.g. a
// retry that fed Mux's own output back in) would otherwise silently produce
// "movie.processed.processed.mkv" instead of failing loudly.
func TestMuxRefusesAlreadyProcessedInput(t *testing.T) {
	libDir := t.TempDir()
	mkvPath := filepath.Join(libDir, "Iron Man 3 (2013).processed.mkv")
	if err := os.WriteFile(mkvPath, []byte("already processed"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	runner := &fakeMuxRunner{}
	results := []ConversionResult{{StreamIndex: 7, Language: "eng", SRTPath: "unused.srt", Converted: true}}

	_, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, nil, results)
	if err == nil {
		t.Fatal("Mux succeeded on an already-processed input, want error")
	}
	if len(runner.calls) != 0 {
		t.Errorf("mkvmerge ran on an already-processed input: %v", runner.calls)
	}
}

// TestMuxEmptyOutputFails covers mkvmerge exiting success (or the exit-1
// warning path) without actually writing anything to the pre-created temp
// file: that must not be promoted into the library as a 0-byte
// .processed.mkv.
func TestMuxEmptyOutputFails(t *testing.T) {
	mkvPath, srts := muxFixture(t, "forced.srt")
	runner := &emptyOutputMuxRunner{}
	sourceStreams := []SubtitleStream{{StreamIndex: 7}}
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
	}

	out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results)
	if err == nil {
		t.Fatal("Mux succeeded on empty mkvmerge output, want error")
	}
	if out != "" {
		t.Errorf("got output path %q on empty output, want none", out)
	}
	assertOnlyFiles(t, filepath.Dir(mkvPath), "Iron Man 3 (2013).mkv")
}

// emptyOutputMuxRunner simulates mkvmerge exiting 0 without writing anything
// to the pre-created -o temp file, leaving it at its original zero length.
type emptyOutputMuxRunner struct{}

func (emptyOutputMuxRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, nil
}

// TestMuxSweepsStaleTempFile covers a temp file orphaned by a previous mux of
// the same output that was killed before it could clean up (SIGKILL, OOM,
// power loss): it must not accumulate forever, and must not be mistaken for
// this run's own output.
func TestMuxSweepsStaleTempFile(t *testing.T) {
	mkvPath, srts := muxFixture(t, "forced.srt")
	stale := filepath.Join(filepath.Dir(mkvPath), ".Iron Man 3 (2013).processed.mkv.tmpSTALE123")
	if err := os.WriteFile(stale, []byte("leftover from a killed mux"), 0o600); err != nil {
		t.Fatalf("writing stale temp fixture: %v", err)
	}

	runner := &fakeMuxRunner{}
	sourceStreams := []SubtitleStream{{StreamIndex: 7}}
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
	}

	if _, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, sourceStreams, results); err != nil {
		t.Fatalf("Mux: %v", err)
	}
	assertOnlyFiles(t, filepath.Dir(mkvPath), "Iron Man 3 (2013).mkv", "Iron Man 3 (2013).processed.mkv")
}

func TestProcessedPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/rips/Iron Man 3 (2013)/Iron Man 3 (2013).mkv", "/rips/Iron Man 3 (2013)/Iron Man 3 (2013).processed.mkv"},
		{"movie.mkv", "movie.processed.mkv"},
		{"/rips/no extension", "/rips/no extension.processed.mkv"},
	}
	for _, c := range cases {
		if got := ProcessedPath(c.in); got != c.want {
			t.Errorf("ProcessedPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// funcRunner adapts a plain function to CommandRunner.
type funcRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f funcRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestMuxerCheckTools(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		runErr  error
		wantErr string
	}{
		{name: "new enough", out: "mkvmerge v74.0.0 ('Panic Station') 64-bit\n"},
		{name: "newer than minimum", out: "mkvmerge v92.0.0 64-bit\n"},
		{name: "too old", out: "mkvmerge v69.0.0 ('Nice Try') 64-bit\n", wantErr: "need >= 74"},
		{name: "not found", runErr: errors.New(`exec: "mkvmerge": executable file not found in $PATH`), wantErr: "not found or not runnable"},
		{name: "unparseable version", out: "not a version string\n", wantErr: "could not parse mkvmerge version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Muxer{Runner: funcRunner(func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "mkvmerge" || len(args) != 1 || args[0] != "--version" {
					t.Fatalf("unexpected call: %s %v", name, args)
				}
				if tt.runErr != nil {
					return nil, tt.runErr
				}
				return []byte(tt.out), nil
			})}

			err := m.CheckTools(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("CheckTools() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CheckTools() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// assertOnlyFiles fails when dir holds anything other than the named files,
// catching both missing output and leftover temp files.
func assertOnlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("directory contents = %v, want %v", got, want)
	}
}

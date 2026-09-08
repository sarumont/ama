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
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
		{StreamIndex: 12, Language: "eng", SRTPath: srts[1], Converted: true},
	}

	before, err := os.ReadFile(mkvPath)
	if err != nil {
		t.Fatalf("reading source: %v", err)
	}

	out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, results)
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
	if call[o+2] != mkvPath {
		t.Errorf("got first input %q, want the source %q", call[o+2], mkvPath)
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
	results := []ConversionResult{{StreamIndex: 3, SRTPath: srts[0], Converted: true}}

	if _, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, results); err != nil {
		t.Fatalf("Mux: %v", err)
	}
	if !hasOption(runner.calls[0], "--language", "0:und") {
		t.Errorf("untagged track: want --language 0:und, got %v", runner.calls[0])
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

			out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, results)
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
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", Error: &msg},
		{StreamIndex: 12, Language: "eng", SRTPath: srts[0], Converted: true},
	}

	if _, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, results); err != nil {
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
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
	}

	out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, results)
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
	results := []ConversionResult{
		{StreamIndex: 7, Language: "eng", SRTPath: srts[0], Converted: true, Forced: true, Default: true},
	}

	out, err := testMuxer(t, runner).Mux(context.Background(), mkvPath, results)
	if err != nil {
		t.Fatalf("Mux on warning exit: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("processed file not in place: %v", err)
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

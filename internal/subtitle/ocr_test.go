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

// fakeOCRRunner stands in for ffmpeg and pgsrip: it creates the files the real
// tools would create, records how it was called, and can be told to fail for
// one stream so per-stream error isolation is testable.
type fakeOCRRunner struct {
	// failExtractFor and failOCRFor are matched against the "0:N" -map value
	// and the .sup path respectively, keyed by stream index.
	failExtractFor map[int]error
	failOCRFor     map[int]error
	// skipSRTFor names stream indexes whose pgsrip run exits cleanly without
	// writing an SRT.
	skipSRTFor map[int]bool

	calls [][]string
}

func (f *fakeOCRRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))

	sup := args[len(args)-1]
	index := supStreamIndex(sup)

	if slices.Contains(args, "-map") { // ffmpeg extraction
		if err := f.failExtractFor[index]; err != nil {
			return nil, err
		}
		return nil, os.WriteFile(sup, []byte("pgs"), 0o644)
	}

	// pgsrip OCR
	if err := f.failOCRFor[index]; err != nil {
		// pgsrip may leave a partial file behind on failure.
		if err := os.WriteFile(srtFor(sup), []byte("partial"), 0o644); err != nil {
			return nil, err
		}
		return nil, f.failOCRFor[index]
	}
	if f.skipSRTFor[index] {
		return nil, nil
	}
	return nil, os.WriteFile(srtFor(sup), []byte("1\n"), 0o644)
}

// commandsFor returns every recorded invocation of the named binary.
func (f *fakeOCRRunner) commandsFor(name string) [][]string {
	var out [][]string
	for _, call := range f.calls {
		if call[0] == name {
			out = append(out, call)
		}
	}
	return out
}

func srtFor(sup string) string {
	return strings.TrimSuffix(sup, ".sup") + ".srt"
}

// supStreamIndex recovers the stream index from a "{base}.{index}.sup" path.
func supStreamIndex(sup string) int {
	base := strings.TrimSuffix(filepath.Base(sup), ".sup")
	i := strings.LastIndex(base, ".")
	if i < 0 {
		return -1
	}
	var n int
	for _, r := range base[i+1:] {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func testConverter(t *testing.T, runner CommandRunner) *Converter {
	t.Helper()
	return &Converter{
		Runner:    runner,
		TempDir:   t.TempDir(),
		Languages: []string{"eng"},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestConvertForcedCandidate(t *testing.T) {
	runner := &fakeOCRRunner{}
	c := testConverter(t, runner)
	reason := "size ratio 0.06 vs stream 12"
	streams := []SubtitleStream{{
		StreamIndex:           7,
		Codec:                 CodecPGS,
		Language:              "eng",
		ForcedCandidate:       true,
		ForcedCandidateReason: &reason,
		NeedsOCR:              true,
	}}

	results, err := c.Convert(context.Background(), "/rips/Iron Man 3 (2013).mkv", streams)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	got := results[0]
	if !got.Converted || got.Error != nil {
		t.Fatalf("conversion failed: converted=%v error=%v", got.Converted, got.Error)
	}
	if !got.Forced || !got.Default {
		t.Errorf("forced candidate: got forced=%v default=%v, want both true", got.Forced, got.Default)
	}
	if got.StreamIndex != 7 || got.Language != "eng" {
		t.Errorf("got stream %d/%q, want 7/\"eng\"", got.StreamIndex, got.Language)
	}
	want := filepath.Join(c.TempDir, "Iron Man 3 (2013).7.srt")
	if got.SRTPath != want {
		t.Errorf("got SRT %q, want %q", got.SRTPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("SRT not left in place for muxing: %v", err)
	}

	// The stream itself is annotated for the manifest/sidecar.
	if !streams[0].Converted || streams[0].ConversionError != nil {
		t.Errorf("stream not annotated: converted=%v error=%v",
			streams[0].Converted, streams[0].ConversionError)
	}

	// The .sup scratch file is cleaned up.
	if _, err := os.Stat(filepath.Join(c.TempDir, "Iron Man 3 (2013).7.sup")); !os.IsNotExist(err) {
		t.Errorf("sup file not cleaned up: %v", err)
	}

	// Command construction: read-only stream copy of exactly that stream.
	extract := runner.commandsFor("ffmpeg")
	if len(extract) != 1 {
		t.Fatalf("got %d ffmpeg calls, want 1", len(extract))
	}
	for _, want := range [][2]string{{"-i", "/rips/Iron Man 3 (2013).mkv"}, {"-map", "0:7"}, {"-c", "copy"}} {
		i := slices.Index(extract[0], want[0])
		if i < 0 || i+1 >= len(extract[0]) || extract[0][i+1] != want[1] {
			t.Errorf("ffmpeg call missing %s %s: %v", want[0], want[1], extract[0])
		}
	}

	ocr := runner.commandsFor("pgsrip")
	if len(ocr) != 1 {
		t.Fatalf("got %d pgsrip calls, want 1", len(ocr))
	}
	if i := slices.Index(ocr[0], "--language"); i < 0 || ocr[0][i+1] != "eng" {
		t.Errorf("pgsrip call missing --language eng: %v", ocr[0])
	}
	if ocr[0][len(ocr[0])-1] != filepath.Join(c.TempDir, "Iron Man 3 (2013).7.sup") {
		t.Errorf("pgsrip did not run against the extracted sup: %v", ocr[0])
	}
}

func TestConvertNonForcedTrack(t *testing.T) {
	c := testConverter(t, &fakeOCRRunner{})
	streams := []SubtitleStream{{
		StreamIndex: 12,
		Codec:       CodecPGS,
		Language:    "eng",
		NeedsOCR:    true,
	}}

	results, err := c.Convert(context.Background(), "/rips/movie.mkv", streams)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	got := results[0]
	if !got.Converted || got.Error != nil {
		t.Fatalf("conversion failed: converted=%v error=%v", got.Converted, got.Error)
	}
	if got.Forced || got.Default {
		t.Errorf("non-forced track: got forced=%v default=%v, want both false", got.Forced, got.Default)
	}
	if !streams[0].Converted {
		t.Error("stream not marked converted")
	}
}

func TestConvertIsolatesPerStreamFailures(t *testing.T) {
	runner := &fakeOCRRunner{
		failExtractFor: map[int]error{3: errors.New("ffmpeg: Invalid data found when processing input")},
		failOCRFor:     map[int]error{5: errors.New("pgsrip: tesseract crashed")},
		skipSRTFor:     map[int]bool{9: true},
	}
	c := testConverter(t, runner)
	streams := []SubtitleStream{
		{StreamIndex: 2, Codec: CodecPGS, Language: "eng", NeedsOCR: true},
		{StreamIndex: 3, Codec: CodecPGS, Language: "eng", NeedsOCR: true},
		{StreamIndex: 5, Codec: CodecPGS, Language: "eng", NeedsOCR: true},
		{StreamIndex: 9, Codec: CodecPGS, Language: "eng", NeedsOCR: true},
		{StreamIndex: 11, Codec: CodecPGS, Language: "eng", ForcedCandidate: true, NeedsOCR: true},
	}

	results, err := c.Convert(context.Background(), "/rips/movie.mkv", streams)
	if err != nil {
		t.Fatalf("Convert returned a batch error for per-stream failures: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5", len(results))
	}

	wantConverted := map[int]bool{2: true, 3: false, 5: false, 9: false, 11: true}
	for i, got := range results {
		want := wantConverted[got.StreamIndex]
		if got.Converted != want {
			t.Errorf("stream %d: converted=%v, want %v (error=%v)",
				got.StreamIndex, got.Converted, want, got.Error)
		}
		if want && got.Error != nil {
			t.Errorf("stream %d: unexpected error %q", got.StreamIndex, *got.Error)
		}
		if !want && (got.Error == nil || *got.Error == "") {
			t.Errorf("stream %d: failure recorded without a reason", got.StreamIndex)
		}
		if !want && got.SRTPath != "" {
			t.Errorf("stream %d: failed conversion reported SRT %q", got.StreamIndex, got.SRTPath)
		}
		if streams[i].Converted != want {
			t.Errorf("stream %d: slice not annotated, converted=%v want %v",
				got.StreamIndex, streams[i].Converted, want)
		}
		if (streams[i].ConversionError != nil) == want {
			t.Errorf("stream %d: ConversionError=%v does not match converted=%v",
				got.StreamIndex, streams[i].ConversionError, want)
		}
	}

	// The failing subprocess's message reaches the recorded error.
	if got := *results[1].Error; !strings.Contains(got, "Invalid data found") {
		t.Errorf("extraction error lost stderr: %q", got)
	}
	if got := *results[2].Error; !strings.Contains(got, "tesseract crashed") {
		t.Errorf("OCR error lost stderr: %q", got)
	}

	// The last stream still converted, so the batch was not aborted, and the
	// failures left no scratch files behind.
	if !results[4].Converted || !results[4].Forced {
		t.Errorf("last stream not converted with forced flags: %+v", results[4])
	}
	entries, err := os.ReadDir(c.TempDir)
	if err != nil {
		t.Fatalf("reading temp dir: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".srt") {
			t.Errorf("scratch file left behind: %s", e.Name())
		}
		if strings.HasPrefix(e.Name(), "movie.3.") || strings.HasPrefix(e.Name(), "movie.5.") ||
			strings.HasPrefix(e.Name(), "movie.9.") {
			t.Errorf("failed conversion left %s behind", e.Name())
		}
	}
}

func TestConvertSkipsStreamsThatDoNotNeedOCR(t *testing.T) {
	runner := &fakeOCRRunner{}
	c := testConverter(t, runner)
	streams := []SubtitleStream{
		{StreamIndex: 4, Codec: "subrip", Language: "eng"},
		{StreamIndex: 6, Codec: "subrip", Language: "fra", ForcedCandidate: true},
	}

	results, err := c.Convert(context.Background(), "/rips/movie.mkv", streams)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results for streams that need no OCR, want 0", len(results))
	}
	if len(runner.calls) != 0 {
		t.Fatalf("shelled out for streams that need no OCR: %v", runner.calls)
	}
	for i := range streams {
		if streams[i].Converted || streams[i].ConversionError != nil {
			t.Errorf("stream %d annotated despite needing no OCR", streams[i].StreamIndex)
		}
	}
}

func TestConvertMixedStreamsOnlyOCRsBitmapTracks(t *testing.T) {
	runner := &fakeOCRRunner{}
	c := testConverter(t, runner)
	streams := []SubtitleStream{
		{StreamIndex: 4, Codec: "subrip", Language: "eng"},
		{StreamIndex: 7, Codec: CodecPGS, Language: "eng", NeedsOCR: true},
	}

	results, err := c.Convert(context.Background(), "/rips/movie.mkv", streams)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(results) != 1 || results[0].StreamIndex != 7 {
		t.Fatalf("got %+v, want a single result for stream 7", results)
	}
	for _, call := range runner.calls {
		if slices.Contains(call, "0:4") {
			t.Errorf("text stream was extracted: %v", call)
		}
	}
}

func TestConvertLanguageSelection(t *testing.T) {
	tests := []struct {
		name       string
		configured []string
		language   string
		want       []string
	}{
		{"stream language is configured", []string{"eng", "fra"}, "fra", []string{"fra"}},
		{"stream language is not configured", []string{"eng", "fra"}, "jpn", []string{"eng", "fra"}},
		{"undetermined stream tries all", []string{"eng", "fra"}, LanguageUndetermined, []string{"eng", "fra"}},
		{"no configured languages falls back", nil, "und", []string{"eng"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeOCRRunner{}
			c := testConverter(t, runner)
			c.Languages = tt.configured
			streams := []SubtitleStream{{StreamIndex: 3, Codec: CodecPGS, Language: tt.language, NeedsOCR: true}}

			if _, err := c.Convert(context.Background(), "/rips/movie.mkv", streams); err != nil {
				t.Fatalf("Convert: %v", err)
			}

			var got []string
			call := runner.commandsFor("pgsrip")[0]
			for i, arg := range call {
				if arg == "--language" {
					got = append(got, call[i+1])
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got languages %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConvertHonoursContextCancellation(t *testing.T) {
	c := testConverter(t, &fakeOCRRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	streams := []SubtitleStream{{StreamIndex: 7, Codec: CodecPGS, Language: "eng", NeedsOCR: true}}
	if _, err := c.Convert(ctx, "/rips/movie.mkv", streams); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestValidateLanguages(t *testing.T) {
	tests := []struct {
		name       string
		configured []string
		bundled    []string
		wantErr    string
	}{
		{"single bundled language", []string{"eng"}, BundledLanguages, ""},
		{"several bundled languages", []string{"eng", "fra", "jpn"}, BundledLanguages, ""},
		{"nil bundled falls back to BundledLanguages", []string{"deu"}, nil, ""},
		{"case and space insensitive", []string{" ENG "}, BundledLanguages, ""},
		{"unbundled language", []string{"eng", "kor"}, BundledLanguages, "kor"},
		{"every language unbundled", []string{"kor", "swe"}, BundledLanguages, "kor, swe"},
		{"no languages configured", nil, BundledLanguages, "no OCR languages configured"},
		{"empty entry", []string{"eng", ""}, BundledLanguages, "empty entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLanguages(tt.configured, tt.bundled)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("got error %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("got nil error, want one mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateLanguagesErrorNamesBundledSet(t *testing.T) {
	err := ValidateLanguages([]string{"kor"}, []string{"eng", "fra"})
	if err == nil {
		t.Fatal("got nil error for an unbundled language")
	}
	for _, want := range []string{"eng, fra", "TESSERACT_LANGS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q, want %q", got, "short")
	}
	got := truncate(strings.Repeat("x", 20), 10)
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) || !strings.Contains(got, "truncated") {
		t.Errorf("got %q, want the first 10 bytes plus a truncation marker", got)
	}
}

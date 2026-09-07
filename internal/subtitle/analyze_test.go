package subtitle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fakeRunner stands in for ffprobe: it replays recorded output and records how
// it was called.
type fakeRunner struct {
	// streamsOut is returned for a -show_streams invocation.
	streamsOut []byte
	streamsErr error
	// packetsOut is returned, keyed by the -select_streams value, for a
	// packet=size invocation.
	packetsOut map[string][]byte
	packetsErr error

	calls [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))

	if slices.Contains(args, "packet=size") {
		if f.packetsErr != nil {
			return nil, f.packetsErr
		}
		i := slices.Index(args, "-select_streams")
		if i < 0 || i+1 >= len(args) {
			return nil, errors.New("fake: packet probe without -select_streams")
		}
		return f.packetsOut[args[i+1]], nil
	}
	return f.streamsOut, f.streamsErr
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

func TestAnalyzeMultiStream(t *testing.T) {
	runner := &fakeRunner{
		streamsOut: fixture(t, "ffprobe_streams.json"),
		// Stream 6 has no NUMBER_OF_BYTES tag, so it falls back to packets.
		packetsOut: map[string][]byte{"6": []byte("1500\n2500\n3000\n")},
	}
	a := &Analyzer{Runner: runner}

	got, err := a.Analyze(context.Background(), "/media/Iron Man 3 (2013).mkv")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	want := []SubtitleStream{
		{StreamIndex: 3, Codec: CodecPGS, Language: "eng", SizeBytes: 2100000, NeedsOCR: true},
		{StreamIndex: 4, Codec: CodecPGS, Language: "eng", SizeBytes: 33000000, DefaultFlagInSource: true, NeedsOCR: true},
		{StreamIndex: 5, Codec: "subrip", Language: "spa", SizeBytes: 45210, ForcedFlagInSource: true},
		{StreamIndex: 6, Codec: CodecPGS, Language: LanguageUndetermined, SizeBytes: 7000, HearingImpaired: true, NeedsOCR: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Analyze streams:\n got %+v\nwant %+v", got, want)
	}

	if len(runner.calls) != 2 {
		t.Fatalf("expected a stream probe plus one packet probe, got %d calls: %v", len(runner.calls), runner.calls)
	}
	first := runner.calls[0]
	if first[0] != "ffprobe" {
		t.Errorf("probe binary = %q, want ffprobe", first[0])
	}
	for _, want := range []string{"-print_format", "json", "-show_streams", "/media/Iron Man 3 (2013).mkv"} {
		if !slices.Contains(first, want) {
			t.Errorf("stream probe args %v missing %q", first, want)
		}
	}
}

func TestAnalyzePGSDetection(t *testing.T) {
	runner := &fakeRunner{
		streamsOut: fixture(t, "ffprobe_streams.json"),
		packetsOut: map[string][]byte{"6": []byte("7000\n")},
	}

	got, err := (&Analyzer{Runner: runner}).Analyze(context.Background(), "movie.mkv")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	var pgs []int
	for _, s := range got {
		if s.NeedsOCR != (s.Codec == CodecPGS) {
			t.Errorf("stream %d: codec %q got NeedsOCR=%v", s.StreamIndex, s.Codec, s.NeedsOCR)
		}
		if s.NeedsOCR {
			pgs = append(pgs, s.StreamIndex)
		}
	}
	if want := []int{3, 4, 6}; !slices.Equal(pgs, want) {
		t.Errorf("PGS stream indexes = %v, want %v", pgs, want)
	}
}

func TestAnalyzeNoSubtitleStreams(t *testing.T) {
	runner := &fakeRunner{streamsOut: fixture(t, "ffprobe_no_subtitles.json")}

	got, err := (&Analyzer{Runner: runner}).Analyze(context.Background(), "movie.mkv")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d subtitle streams, want none", len(got))
	}
	if got == nil {
		t.Error("got nil slice, want empty slice")
	}
	if len(runner.calls) != 1 {
		t.Errorf("expected only the stream probe, got %v", runner.calls)
	}
}

func TestAnalyzeDispositionFlags(t *testing.T) {
	tests := []struct {
		name        string
		disposition string
		want        SubtitleStream
	}{
		{
			name:        "no flags",
			disposition: `{"default": 0, "forced": 0, "hearing_impaired": 0}`,
			want:        SubtitleStream{StreamIndex: 2, Codec: CodecPGS, Language: "eng", SizeBytes: 100, NeedsOCR: true},
		},
		{
			name:        "forced only",
			disposition: `{"default": 0, "forced": 1, "hearing_impaired": 0}`,
			want:        SubtitleStream{StreamIndex: 2, Codec: CodecPGS, Language: "eng", SizeBytes: 100, ForcedFlagInSource: true, NeedsOCR: true},
		},
		{
			name:        "default and hearing impaired",
			disposition: `{"default": 1, "forced": 0, "hearing_impaired": 1}`,
			want:        SubtitleStream{StreamIndex: 2, Codec: CodecPGS, Language: "eng", SizeBytes: 100, DefaultFlagInSource: true, HearingImpaired: true, NeedsOCR: true},
		},
		{
			name:        "all flags set",
			disposition: `{"default": 1, "forced": 1, "hearing_impaired": 1}`,
			want:        SubtitleStream{StreamIndex: 2, Codec: CodecPGS, Language: "eng", SizeBytes: 100, ForcedFlagInSource: true, DefaultFlagInSource: true, HearingImpaired: true, NeedsOCR: true},
		},
		{
			name:        "disposition object absent",
			disposition: "",
			want:        SubtitleStream{StreamIndex: 2, Codec: CodecPGS, Language: "eng", SizeBytes: 100, NeedsOCR: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := `{"index": 2, "codec_name": "hdmv_pgs_subtitle", "codec_type": "subtitle",
				"tags": {"language": "eng", "NUMBER_OF_BYTES": "100"}`
			if tt.disposition != "" {
				stream += `, "disposition": ` + tt.disposition
			}
			out := []byte(`{"streams": [` + stream + `}]}`)

			got, err := (&Analyzer{Runner: &fakeRunner{streamsOut: out}}).Analyze(context.Background(), "movie.mkv")
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d streams, want 1", len(got))
			}
			if got[0] != tt.want {
				t.Errorf("got %+v, want %+v", got[0], tt.want)
			}
		})
	}
}

func TestAnalyzeBadOutput(t *testing.T) {
	tests := []struct {
		name   string
		runner *fakeRunner
		want   string
	}{
		{
			name:   "empty output",
			runner: &fakeRunner{streamsOut: nil},
			want:   "parsing ffprobe output",
		},
		{
			name:   "truncated json",
			runner: &fakeRunner{streamsOut: []byte(`{"streams": [{"index": 3,`)},
			want:   "parsing ffprobe output",
		},
		{
			name:   "not json at all",
			runner: &fakeRunner{streamsOut: []byte("ffprobe version 6.1\n")},
			want:   "parsing ffprobe output",
		},
		{
			name:   "no streams object",
			runner: &fakeRunner{streamsOut: []byte(`{"format": {"filename": "movie.mkv"}}`)},
			want:   "no streams object",
		},
		{
			name:   "ffprobe failed",
			runner: &fakeRunner{streamsErr: errors.New("exit status 1: movie.mkv: No such file or directory")},
			want:   "No such file or directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (&Analyzer{Runner: tt.runner}).Analyze(context.Background(), "movie.mkv")
			if err == nil {
				t.Fatalf("expected an error, got streams %+v", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
			if got != nil {
				t.Errorf("expected nil streams on error, got %+v", got)
			}
		})
	}
}

func TestAnalyzeSizeFallbackFailureIsWarning(t *testing.T) {
	runner := &fakeRunner{
		streamsOut: []byte(`{"streams": [{"index": 4, "codec_name": "hdmv_pgs_subtitle",
			"codec_type": "subtitle", "tags": {"language": "eng"}}]}`),
		packetsErr: errors.New("exit status 1: packet probe failed"),
	}
	var logs bytes.Buffer
	a := &Analyzer{Runner: runner, Log: slog.New(slog.NewTextHandler(&logs, nil))}

	got, err := a.Analyze(context.Background(), "movie.mkv")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(got) != 1 || got[0].SizeBytes != 0 {
		t.Fatalf("got %+v, want a single stream with size 0", got)
	}
	if !strings.Contains(logs.String(), "stream_index=4") {
		t.Errorf("expected a warning naming the stream, got: %s", logs.String())
	}
}

func TestAnalyzeUnparsablePacketSizeIsWarning(t *testing.T) {
	runner := &fakeRunner{
		streamsOut: []byte(`{"streams": [{"index": 4, "codec_name": "subrip",
			"codec_type": "subtitle", "tags": {"language": "eng"}}]}`),
		packetsOut: map[string][]byte{"4": []byte("N/A\n")},
	}

	got, err := (&Analyzer{Runner: runner, Log: slog.New(slog.DiscardHandler)}).Analyze(context.Background(), "movie.mkv")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(got) != 1 || got[0].SizeBytes != 0 {
		t.Fatalf("got %+v, want a single stream with size 0", got)
	}
}

func TestAnalyzeFFprobePathOverride(t *testing.T) {
	runner := &fakeRunner{streamsOut: fixture(t, "ffprobe_no_subtitles.json")}
	a := &Analyzer{Runner: runner, FFprobePath: "/opt/bin/ffprobe"}

	if _, err := a.Analyze(context.Background(), "movie.mkv"); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if runner.calls[0][0] != "/opt/bin/ffprobe" {
		t.Errorf("probe binary = %q, want /opt/bin/ffprobe", runner.calls[0][0])
	}
}

func TestSidecarPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/media/Iron Man 3 (2013)/Iron Man 3 (2013).mkv", "/media/Iron Man 3 (2013)/Iron Man 3 (2013).subtitles.json"},
		{"movie.mkv", "movie.subtitles.json"},
		{"/media/no-extension", "/media/no-extension.subtitles.json"},
		{"/media/Movie.Name.2013.mkv", "/media/Movie.Name.2013.subtitles.json"},
	}
	for _, tt := range tests {
		if got := SidecarPath(tt.in); got != tt.want {
			t.Errorf("SidecarPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWriteSidecar(t *testing.T) {
	dir := t.TempDir()
	mkv := filepath.Join(dir, "Iron Man 3 (2013).mkv")
	streams := []SubtitleStream{
		{StreamIndex: 3, Codec: CodecPGS, Language: "eng", SizeBytes: 2100000, NeedsOCR: true},
		{StreamIndex: 5, Codec: "subrip", Language: "spa", SizeBytes: 45210, ForcedFlagInSource: true},
	}

	if err := WriteSidecar(mkv, streams); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}

	data, err := os.ReadFile(SidecarPath(mkv))
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}
	var got Sidecar
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding sidecar: %v", err)
	}
	if !reflect.DeepEqual(got.Subtitles, streams) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got.Subtitles, streams)
	}
	if !strings.Contains(string(data), `"forced_flag_in_source"`) {
		t.Errorf("sidecar does not use manifest field names:\n%s", data)
	}

	// Overwriting must replace the file and leave no temp files behind.
	if err := WriteSidecar(mkv, streams[:1]); err != nil {
		t.Fatalf("WriteSidecar (overwrite): %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(SidecarPath(mkv)) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want only the sidecar", names)
	}
}

func TestWriteSidecarEmpty(t *testing.T) {
	mkv := filepath.Join(t.TempDir(), "movie.mkv")
	if err := WriteSidecar(mkv, nil); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	data, err := os.ReadFile(SidecarPath(mkv))
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), "{\n  \"subtitles\": []\n}"; got != want {
		t.Errorf("sidecar = %q, want %q", got, want)
	}
}

func TestWriteSidecarUnwritableDirectory(t *testing.T) {
	mkv := filepath.Join(t.TempDir(), "missing", "movie.mkv")
	if err := WriteSidecar(mkv, nil); err == nil {
		t.Fatal("expected an error writing into a missing directory")
	}
}

func TestExecRunnerReportsStderr(t *testing.T) {
	_, err := ExecRunner{}.Run(context.Background(), "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected an error from a failing command")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not include stderr", err)
	}
}

func TestExecRunnerHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (ExecRunner{}).Run(ctx, "sh", "-c", "sleep 30"); err == nil {
		t.Fatal("expected an error from a canceled context")
	}
}

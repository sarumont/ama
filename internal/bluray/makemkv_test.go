package bluray

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeRunner replays recorded makemkvcon output instead of shelling out, and
// records how it was called.
type fakeRunner struct {
	stdout []byte
	err    error

	name string
	args []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.name = name
	f.args = args
	return f.stdout, f.err
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

func TestClientInfo(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []Track
	}{
		{
			name:    "multiple titles with commentary",
			fixture: "info_multi_title.txt",
			want: []Track{
				{
					Index:              0,
					Name:               "IRON_MAN_3",
					DurationSeconds:    7807,
					SizeBytes:          27958604800,
					ChapterCount:       25,
					AudioTrackCount:    2,
					HasCommentaryAudio: true,
					AudioTracks: []AudioTrack{
						{Index: 1, LanguageCode: "eng", Name: "Surround 7.1"},
						// Flagged commentary: ap_iaStreamFlags carries
						// AP_AVStreamFlag_DirectorsComments.
						{Index: 2, LanguageCode: "eng", Name: "Director's Comments", Flags: 1, HasCommentaryFlag: true},
					},
					OutputFileName: "IRON_MAN_3_t00.mkv",
				},
				{
					Index:              1,
					Name:               "IRON_MAN_3",
					DurationSeconds:    7170,
					SizeBytes:          25877416960,
					ChapterCount:       24,
					AudioTrackCount:    2,
					HasCommentaryAudio: true,
					AudioTracks: []AudioTrack{
						{Index: 1, LanguageCode: "eng", Name: "Surround 5.1"},
						// Unflagged commentary: only the track description says
						// so, which is common on real discs.
						{
							Index:             2,
							LanguageCode:      "eng",
							Name:              "Commentary by Shane Black, Dolby Digital English 2.0ch 48kHz",
							HasCommentaryFlag: true,
						},
					},
					OutputFileName: "IRON_MAN_3_t01.mkv",
				},
				{
					Index:           2,
					Name:            "IRON_MAN_3",
					DurationSeconds: 659,
					SizeBytes:       890000000,
					ChapterCount:    1,
					AudioTrackCount: 1,
					AudioTracks: []AudioTrack{
						{Index: 1, LanguageCode: "eng", Name: "Surround 5.1"},
					},
					OutputFileName: "IRON_MAN_3_t02.mkv",
				},
			},
		},
		{
			name:    "no titles is not an error",
			fixture: "info_no_titles.txt",
			want:    []Track{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{stdout: fixture(t, tt.fixture)}
			client := &Client{Runner: runner, MinLengthSeconds: 60}

			got, err := client.Info(context.Background(), "disc:0")
			if err != nil {
				t.Fatalf("Info: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Info tracks:\ngot  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestClientInfoArgs(t *testing.T) {
	tests := []struct {
		name      string
		minLength int
		want      []string
	}{
		{
			name:      "minimum title length applied",
			minLength: 60,
			want:      []string{"-r", "--cache=1", "--minlength=60", "info", "disc:0"},
		},
		{
			name:      "no minimum",
			minLength: 0,
			want:      []string{"-r", "--cache=1", "info", "disc:0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{stdout: fixture(t, "info_no_titles.txt")}
			client := &Client{Runner: runner, MinLengthSeconds: tt.minLength}

			if _, err := client.Info(context.Background(), "disc:0"); err != nil {
				t.Fatalf("Info: %v", err)
			}
			if runner.name != DefaultBinary {
				t.Errorf("binary = %q, want %q", runner.name, DefaultBinary)
			}
			if !reflect.DeepEqual(runner.args, tt.want) {
				t.Errorf("args = %v, want %v", runner.args, tt.want)
			}
		})
	}
}

func TestClientInfoErrors(t *testing.T) {
	tests := []struct {
		name     string
		stdout   []byte
		fixture  string
		runErr   error
		wantIs   error
		contains []string
	}{
		{
			name:     "error message record",
			fixture:  "info_disc_error.txt",
			contains: []string{"Scsi error", "UNRECOVERED READ ERROR"},
		},
		{
			name:     "non-zero exit carries stderr",
			fixture:  "info_no_titles.txt",
			runErr:   &CommandError{Name: DefaultBinary, Stderr: "makemkvcon: cannot open /dev/sr0\n", Err: errors.New("exit status 1")},
			contains: []string{"exit status 1", "cannot open /dev/sr0"},
		},
		{
			name:     "non-zero exit prefers the MakeMKV message",
			fixture:  "info_disc_error.txt",
			runErr:   &CommandError{Name: DefaultBinary, Stderr: "", Err: errors.New("exit status 1")},
			contains: []string{"Scsi error", "exit status 1"},
		},
		{
			name:     "binary not installed",
			runErr:   &exec.Error{Name: DefaultBinary, Err: exec.ErrNotFound},
			wantIs:   ErrNotInstalled,
			contains: []string{"makemkvcon not found in PATH"},
		},
		{
			name:     "unterminated quoted value",
			fixture:  "info_truncated.txt",
			contains: []string{"parsing output", "unterminated"},
		},
		{
			name:     "short TINFO record",
			stdout:   []byte("TCOUNT:1\nTINFO:0,9,0\n"),
			contains: []string{"TINFO", "want 4 fields"},
		},
		{
			name:     "short SINFO record",
			stdout:   []byte("TCOUNT:1\nSINFO:0,1,22,0\n"),
			contains: []string{"SINFO", "want 5 fields"},
		},
		{
			name:     "non-numeric title index",
			stdout:   []byte("TINFO:first,9,0,\"1:00:00\"\n"),
			contains: []string{"TINFO", "index"},
		},
		{
			name:     "non-numeric duration",
			stdout:   []byte("TINFO:0,9,0,\"about two hours\"\n"),
			contains: []string{"TINFO", "duration"},
		},
		{
			name:     "non-numeric size",
			stdout:   []byte("TINFO:0,11,0,\"26 GB\"\n"),
			contains: []string{"TINFO", "size"},
		},
		{
			name:     "non-numeric stream flags",
			stdout:   []byte("SINFO:0,1,22,0,\"none\"\n"),
			contains: []string{"SINFO", "stream flags"},
		},
		{
			name:     "non-numeric title count",
			stdout:   []byte("TCOUNT:many\n"),
			contains: []string{"TCOUNT"},
		},
		{
			name:     "short MSG record",
			stdout:   []byte("MSG:5010,0\n"),
			contains: []string{"MSG", "at least 4 fields"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout := tt.stdout
			if tt.fixture != "" {
				stdout = fixture(t, tt.fixture)
			}
			client := &Client{Runner: &fakeRunner{stdout: stdout, err: tt.runErr}}

			got, err := client.Info(context.Background(), "disc:0")
			if err == nil {
				t.Fatalf("Info succeeded with %+v, want error", got)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error %v, want errors.Is %v", err, tt.wantIs)
			}
			for _, want := range tt.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestClientInfoIgnoresUnknownRecords guards the "unrecognized record types are
// ignored, not fatal" rule.
func TestClientInfoIgnoresUnknownRecords(t *testing.T) {
	stdout := []byte("SOMETHINGNEW:1,2,3\nno-colon-at-all\n\nTINFO:0,9,0,\"0:01:00\"\n")
	client := &Client{Runner: &fakeRunner{stdout: stdout}}

	got, err := client.Info(context.Background(), "disc:0")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	want := []Track{{Index: 0, DurationSeconds: 60}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tracks = %+v, want %+v", got, want)
	}
}

func TestClientRip(t *testing.T) {
	tests := []struct {
		name     string
		present  []string
		wantErr  bool
		contains string
	}{
		{
			name:    "all output files written",
			present: []string{"IRON_MAN_3_t00.mkv", "IRON_MAN_3_t01.mkv"},
		},
		{
			name:     "missing output file",
			present:  []string{"IRON_MAN_3_t00.mkv"},
			wantErr:  true,
			contains: "IRON_MAN_3_t01.mkv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.present {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("mkv"), 0o600); err != nil {
					t.Fatalf("writing %s: %v", name, err)
				}
			}
			runner := &fakeRunner{stdout: fixture(t, "mkv_success.txt")}
			client := &Client{Runner: runner, MinLengthSeconds: 60}

			tracks, err := client.Rip(context.Background(), "disc:0", dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Rip succeeded, want error")
				}
				if !strings.Contains(err.Error(), tt.contains) {
					t.Errorf("error %q does not contain %q", err, tt.contains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rip: %v", err)
			}

			wantArgs := []string{"-r", "--cache=1", "--minlength=60", "mkv", "disc:0", "all", dir}
			if !reflect.DeepEqual(runner.args, wantArgs) {
				t.Errorf("args = %v, want %v", runner.args, wantArgs)
			}
			for _, arg := range runner.args {
				if strings.Contains(arg, "encode") || strings.Contains(arg, "transcode") {
					t.Errorf("transcoding argument %q passed to makemkvcon", arg)
				}
			}
			want := []Track{
				{
					Index:           0,
					Name:            "IRON_MAN_3",
					DurationSeconds: 7807,
					SizeBytes:       27958604800,
					ChapterCount:    25,
					AudioTrackCount: 1,
					AudioTracks:     []AudioTrack{{Index: 1, LanguageCode: "eng", Name: "Surround 7.1"}},
					OutputFileName:  "IRON_MAN_3_t00.mkv",
					OutputPath:      filepath.Join(dir, "IRON_MAN_3_t00.mkv"),
				},
				{
					Index:           1,
					Name:            "IRON_MAN_3",
					DurationSeconds: 659,
					SizeBytes:       890000000,
					ChapterCount:    1,
					AudioTrackCount: 1,
					AudioTracks:     []AudioTrack{{Index: 1, LanguageCode: "eng", Name: "Surround 5.1"}},
					OutputFileName:  "IRON_MAN_3_t01.mkv",
					OutputPath:      filepath.Join(dir, "IRON_MAN_3_t01.mkv"),
				},
			}
			if !reflect.DeepEqual(tracks, want) {
				t.Errorf("Rip tracks:\ngot  %+v\nwant %+v", tracks, want)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		value   string
		want    int
		wantErr bool
	}{
		{value: "2:10:07", want: 7807},
		{value: "1:59:30", want: 7170},
		{value: "10:59", want: 659},
		{value: "42", want: 42},
		{value: "0:00:00", want: 0},
		{value: "1:2:3:4", wantErr: true},
		{value: "", wantErr: true},
		{value: "two hours", wantErr: true},
		{value: "-1:00", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseDuration(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDuration(%q) = %d, want error", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDuration(%q): %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("parseDuration(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestSplitRobotFields(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "bare values", in: "0,9,0", want: []string{"0", "9", "0"}},
		{
			name: "comma inside quotes",
			in:   `0,30,0,"Commentary by Shane Black, 2.0ch"`,
			want: []string{"0", "30", "0", "Commentary by Shane Black, 2.0ch"},
		},
		{
			name: "escaped quote",
			in:   `0,2,0,"He said \"hi\""`,
			want: []string{"0", "2", "0", `He said "hi"`},
		},
		{name: "empty values", in: `1,256,999,0,"","",""`, want: []string{"1", "256", "999", "0", "", "", ""}},
		{name: "unterminated quote", in: `0,9,0,"2:10:07`, wantErr: true},
		{name: "trailing escape", in: `0,9,0,"2:10:07\`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitRobotFields(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("splitRobotFields(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitRobotFields(%q): %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitRobotFields(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWriteLicenseKeyTo(t *testing.T) {
	const key = "T-T2HdpRvyvJtxjVtKWRv1n92AcFFFlYyl3FX2mazk0obKPR3V1fgIDQ793QfUnT2opR"

	tests := []struct {
		name     string
		existing string
		create   bool
		key      string
		want     string
		wantErr  string
	}{
		{
			name: "creates a new settings file",
			key:  key,
			want: "app_Key = \"" + key + "\"\n",
		},
		{
			name:     "replaces an existing key and keeps other settings",
			create:   true,
			existing: "app_DestinationDir = \"/media\"\napp_Key = \"T-old\"\napp_DefaultSelectionString = \"+sel:all\"\n",
			key:      key,
			want:     "app_DestinationDir = \"/media\"\napp_Key = \"" + key + "\"\napp_DefaultSelectionString = \"+sel:all\"\n",
		},
		{
			name:     "appends to a file without a key",
			create:   true,
			existing: "app_DestinationDir = \"/media\"\n",
			key:      key,
			want:     "app_DestinationDir = \"/media\"\napp_Key = \"" + key + "\"\n",
		},
		{
			name:    "rejects an empty key",
			key:     "   ",
			wantErr: "license key is empty",
		},
		{
			name:    "rejects a key that would corrupt the file",
			key:     "T-bad\"key",
			wantErr: "invalid characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".MakeMKV", "settings.conf")
			if tt.create {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("creating dir: %v", err)
				}
				if err := os.WriteFile(path, []byte(tt.existing), 0o600); err != nil {
					t.Fatalf("seeding settings: %v", err)
				}
			}

			err := writeLicenseKeyTo(path, tt.key)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("writeLicenseKeyTo succeeded, want error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), tt.key) {
					t.Errorf("error %q leaks the license key", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("writeLicenseKeyTo: %v", err)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading settings: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("settings.conf =\n%q\nwant\n%q", got, tt.want)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("settings.conf mode = %o, want 600", perm)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatalf("reading dir: %v", err)
			}
			if len(entries) != 1 {
				t.Errorf("temp files left behind: %v", entries)
			}
		})
	}
}

package cd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sarumont/ama/internal/musicbrainz"
	"github.com/sarumont/ama/internal/testutil"
)

// mbTestClient starts an httptest server that always answers a disc ID
// lookup with body, and returns a musicbrainz.Client pointed at it.
func mbTestClient(t *testing.T, status int, body string) *musicbrainz.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return musicbrainz.New("ama-test/0.0", srv.Client(), srv.URL)
}

const matchBody = `{
	"releases": [
		{
			"id": "11111111-1111-1111-1111-111111111111",
			"title": "Some Album",
			"date": "2020",
			"release-group": {"id": "22222222-2222-2222-2222-222222222222"},
			"artist-credit": [{"name": "Some Artist", "joinphrase": ""}],
			"media": [{"tracks": [{"number": "1", "title": "Track One", "length": 180000}]}]
		}
	]
}`

func TestIdentifyMatch(t *testing.T) {
	runner := testutil.NewFixtureRunner(t, filepath.Join("testdata", "whipper", "cd-info-match"))
	mb := mbTestClient(t, http.StatusOK, matchBody)
	client := &Client{Runner: runner, MusicBrainz: mb}

	result, err := client.Identify(context.Background(), "/dev/sr0")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if result.DiscID != "T3STmatchD1scIDXXXXXXXXXXXX-" {
		t.Errorf("DiscID = %q, want the parsed disc id", result.DiscID)
	}
	if len(result.Releases) != 1 || result.Releases[0].MBReleaseID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("Releases = %+v, want the one release Identify's own musicbrainz lookup returned", result.Releases)
	}
	// The release came from AMA's own MusicBrainz lookup, not from whipper's
	// "Matching releases:" text (which names a different artist/album in the
	// fixture, to make sure it was never scraped).
	if result.Releases[0].Artist == "Whipper's Own Guess Artist" {
		t.Error("Identify used whipper's own 'Matching releases:' text instead of its own MusicBrainz lookup")
	}
}

func TestIdentifyNoMusicBrainzMatchExit255(t *testing.T) {
	runner := testutil.NewFixtureRunner(t, filepath.Join("testdata", "whipper", "cd-info-no-match"))
	// whipper itself found nothing, but AMA's own lookup might still find a
	// release for the same disc id -- Identify must not treat whipper's
	// opinion as authoritative, only its own MusicBrainz call.
	mb := mbTestClient(t, http.StatusOK, matchBody)
	client := &Client{Runner: runner, MusicBrainz: mb}

	result, err := client.Identify(context.Background(), "/dev/sr0")
	if err != nil {
		t.Fatalf("Identify: want no error for a legitimate exit 255 with a disc id, got %v", err)
	}
	if result.DiscID != "N0MATCHd1scIDYYYYYYYYYYYY-" {
		t.Errorf("DiscID = %q, want the parsed disc id", result.DiscID)
	}
	if len(result.Releases) != 1 {
		t.Errorf("Releases = %+v, want AMA's own lookup result regardless of whipper's exit code", result.Releases)
	}
}

func TestIdentifyNoMatchAndNoReleases(t *testing.T) {
	runner := testutil.NewFixtureRunner(t, filepath.Join("testdata", "whipper", "cd-info-no-match"))
	mb := mbTestClient(t, http.StatusNotFound, `{"error": "Not Found"}`)
	client := &Client{Runner: runner, MusicBrainz: mb}

	result, err := client.Identify(context.Background(), "/dev/sr0")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(result.Releases) != 0 {
		t.Errorf("Releases = %+v, want none (a real 'no candidates' case)", result.Releases)
	}
}

func TestIdentifyNoDiscIsHardError(t *testing.T) {
	runner := testutil.NewFixtureRunner(t, filepath.Join("testdata", "whipper", "cd-info-no-disc"))
	mb := mbTestClient(t, http.StatusOK, matchBody)
	client := &Client{Runner: runner, MusicBrainz: mb}

	_, err := client.Identify(context.Background(), "/dev/sr0")
	if err == nil {
		t.Fatal("Identify: want error when no disc id was ever printed, got nil")
	}
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("err = %v, want a *CommandError in the chain", err)
	}
	if cmdErr.Stderr == "" {
		t.Error("CommandError.Stderr is empty, want whipper's captured stderr")
	}
}

func TestIdentifyRequiresMusicBrainzClient(t *testing.T) {
	client := &Client{Runner: testutil.NewFixtureRunner(t, filepath.Join("testdata", "whipper", "cd-info-match"))}
	if _, err := client.Identify(context.Background(), "/dev/sr0"); err == nil {
		t.Fatal("Identify: want error when Client.MusicBrainz is nil, got nil")
	}
}

func TestIdentifyRequiresDevice(t *testing.T) {
	client := &Client{MusicBrainz: mbTestClient(t, http.StatusOK, matchBody)}
	if _, err := client.Identify(context.Background(), ""); err == nil {
		t.Fatal("Identify: want error for an empty device, got nil")
	}
}

// fakeRunner is a minimal CommandRunner for Rip's tests. FixtureRunner is not
// used here because -O's argument is a t.TempDir() path, unique per test run
// and so impossible to match against a static recorded cmd file the way
// Identify's fixed `cd info -d <device>` invocation can be.
type fakeRunner struct {
	stdout, stderr []byte
	err            error
	gotArgs        []string
}

func (f *fakeRunner) RunCapture(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.gotArgs = append([]string{name}, args...)
	return f.stdout, f.stderr, f.err
}

// exitError mimics testutil.ExitError's shape (an ExitCode() int method)
// without importing it, since production code never constructs one -- only
// tests need a synthetic non-zero exit here.
type exitError struct{ code int }

func (e *exitError) Error() string { return "exit error" }
func (e *exitError) ExitCode() int { return e.code }

func testRelease() musicbrainz.Release {
	return musicbrainz.Release{
		MBReleaseID:      "11111111-1111-1111-1111-111111111111",
		MBReleaseGroupID: "22222222-2222-2222-2222-222222222222",
		Artist:           "Test Artist",
		Album:            "Test Album",
		Year:             2020,
	}
}

// writeLogFixture copies a testdata/whipper-log fixture to the path Rip
// predicts for release under outputRoot.
func writeLogFixture(t *testing.T, outputRoot, fixtureName string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "whipper-log", fixtureName))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", fixtureName, err)
	}
	path := logPath(outputRoot, testRelease())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestRipSuccess(t *testing.T) {
	outputRoot := t.TempDir()
	writeLogFixture(t, outputRoot, "success.log")
	runner := &fakeRunner{}
	client := &Client{Runner: runner}

	result, err := client.Rip(context.Background(), "/dev/sr0", testRelease(), outputRoot)
	if err != nil {
		t.Fatalf("Rip: %v", err)
	}
	if result.AccurateRipSummary != AccurateRipAllAccurate {
		t.Errorf("AccurateRipSummary = %q, want %q", result.AccurateRipSummary, AccurateRipAllAccurate)
	}
	if len(result.Tracks) != 2 {
		t.Fatalf("Tracks = %+v, want 2 entries", result.Tracks)
	}

	wantArgs := []string{
		"whipper", "cd", "rip",
		"-d", "/dev/sr0",
		"-R", testRelease().MBReleaseID,
		"-O", outputRoot,
		"--track-template", trackTemplate,
		"--disc-template", discTemplate,
		"-L", "whipper",
		"--cdr",
		"-k",
	}
	if len(runner.gotArgs) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", runner.gotArgs, wantArgs)
	}
	for i := range wantArgs {
		if runner.gotArgs[i] != wantArgs[i] {
			t.Errorf("args[%d] = %q, want %q (full: %v)", i, runner.gotArgs[i], wantArgs[i], runner.gotArgs)
		}
	}
	// -U/--unknown must never be passed on the confirmed-release path.
	for _, a := range runner.gotArgs {
		if a == "-U" || a == "--unknown" {
			t.Errorf("args contain %q, which must never be passed alongside -R", a)
		}
	}
}

func TestRipPartialSkipExit5(t *testing.T) {
	outputRoot := t.TempDir()
	writeLogFixture(t, outputRoot, "partial-skip.log")
	runner := &fakeRunner{err: &exitError{code: exitPartialSkip}, stderr: []byte("some tracks were skipped")}
	client := &Client{Runner: runner}

	result, err := client.Rip(context.Background(), "/dev/sr0", testRelease(), outputRoot)
	if err != nil {
		t.Fatalf("Rip: want no error for exit 5 (partial skip), got %v", err)
	}
	if len(result.Tracks) != 2 || result.Tracks[1].Status != TrackStatusSkipped {
		t.Errorf("Tracks = %+v, want track 2 skipped", result.Tracks)
	}
}

func TestRipRuntimeErrorExit1(t *testing.T) {
	outputRoot := t.TempDir()
	runner := &fakeRunner{err: &exitError{code: exitRuntimeError}, stderr: []byte("CRCs did not match for track 1")}
	client := &Client{Runner: runner}

	_, err := client.Rip(context.Background(), "/dev/sr0", testRelease(), outputRoot)
	if err == nil {
		t.Fatal("Rip: want error for exit 1 (RuntimeError), got nil")
	}
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("err = %v, want a *CommandError in the chain", err)
	}
}

func TestRipSystemErrorExit255(t *testing.T) {
	outputRoot := t.TempDir()
	runner := &fakeRunner{err: &exitError{code: exitSystemError}, stderr: []byte("no musicbrainz metadata for this release")}
	client := &Client{Runner: runner}

	_, err := client.Rip(context.Background(), "/dev/sr0", testRelease(), outputRoot)
	if err == nil {
		t.Fatal("Rip: want error for exit 255 (SystemError), got nil")
	}
}

func TestRipRequiresConfirmedRelease(t *testing.T) {
	client := &Client{Runner: &fakeRunner{}}
	_, err := client.Rip(context.Background(), "/dev/sr0", musicbrainz.Release{}, t.TempDir())
	if err == nil {
		t.Fatal("Rip: want error when release.MBReleaseID is empty, got nil")
	}
}

func TestParseDiscID(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		want    string
		wantErr bool
	}{
		{
			name:   "present",
			stdout: "CDDB disc id: 3f0aec05\nMusicBrainz disc id abcDEF1234567890123456789-\nDisc duration: 00:21:00.000, 2 audio tracks\n",
			want:   "abcDEF1234567890123456789-",
		},
		{
			name:    "absent",
			stdout:  "CDDB disc id: 3f0aec05\nDisc duration: 00:21:00.000, 2 audio tracks\n",
			wantErr: true,
		},
		{
			name:    "empty",
			stdout:  "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDiscID([]byte(tt.stdout))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDiscID(%q) = %q, want error", tt.stdout, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDiscID(%q): %v", tt.stdout, err)
			}
			if got != tt.want {
				t.Errorf("parseDiscID(%q) = %q, want %q", tt.stdout, got, tt.want)
			}
		})
	}
}

func TestLogPathSanitizesSlashes(t *testing.T) {
	release := musicbrainz.Release{Artist: "AC/DC", Album: "T.N.T."}
	got := logPath("/music", release)
	want := filepath.Join("/music", "AC_DC", "T.N.T.", "AC_DC - T.N.T..log")
	if got != want {
		t.Errorf("logPath = %q, want %q", got, want)
	}
}

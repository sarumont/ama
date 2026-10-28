package web

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sarumont/ama/config"
	"github.com/sarumont/ama/internal/manifest"
)

// testdataMovies/testdataMusic point at the fixture manifest trees in
// testdata/. Each subdirectory of testdataMovies is one manifest, laid out
// exactly the way manifest.Dir would produce it, so pointing
// cfg.Output.Movies at a single subdirectory isolates one fixture for a
// test that needs exactly one "active" (non-complete, non-error) manifest.
const (
	testdataMovies = "testdata/movies"
	testdataMusic  = "testdata/music"
)

// newHandlerTestServer builds a server whose output roots are movies/music
// (either may be "" to leave that root unset, matching an empty
// config.Output field).
func newHandlerTestServer(t *testing.T, movies, music string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Output.Movies = movies
	cfg.Output.Music = music

	srv, err := New(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// post issues a form-encoded POST, the same content type confirm.html's
// plain HTML forms send (no enctype override), so it exercises the
// handlers the same way the browser would.
func post(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// copyFixtureDir copies a testdata fixture directory into a fresh t.TempDir()
// and returns the copy's root. The write endpoints mutate the manifest at
// its on-disk path via manifest.Update, so any test that POSTs to one must
// run against a throwaway copy rather than testdata/ itself — otherwise the
// test would permanently rewrite a checked-in fixture.
func copyFixtureDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copying fixture %s: %v", src, err)
	}
	return dst
}

// readManifest re-reads the manifest at id's path under root (found by
// walking, the same way loadManifests does) so a test can assert on what a
// write endpoint actually persisted, not just what it returned.
func readManifest(t *testing.T, root, id string) *manifest.Manifest {
	t.Helper()
	var found *manifest.Manifest
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".manifest.json") {
			return err
		}
		m, rerr := manifest.Read(p)
		if rerr != nil {
			return rerr
		}
		if m.ID == id {
			found = m
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading manifest %s under %s: %v", id, root, err)
	}
	if found == nil {
		t.Fatalf("no manifest with id %q under %s", id, root)
	}
	return found
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("body does not contain %q\ngot: %s", want, body)
	}
}

func mustNotContain(t *testing.T, body, want string) {
	t.Helper()
	if strings.Contains(body, want) {
		t.Errorf("body unexpectedly contains %q\ngot: %s", want, body)
	}
}

// --- GET / (queue) ---------------------------------------------------------

func TestHandleQueueEmpty(t *testing.T) {
	srv := newHandlerTestServer(t, t.TempDir(), t.TempDir())
	rec := get(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), "No disc in the drive")
	mustNotContain(t, rec.Body.String(), "hx-get")
}

func TestHandleQueueRipping(t *testing.T) {
	srv := newHandlerTestServer(t, testdataMovies+"/Ripping Now (2026)", "")
	rec := get(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "RIPPING_NOW")
	mustContain(t, body, "/dev/sr0")
	mustContain(t, body, "Ripping Now")
	mustContain(t, body, "badge-ripping")
	// Polling must be wired for an in-progress rip.
	mustContain(t, body, `hx-get="/api/status/33333333-3333-3333-3333-333333333333"`)
	mustContain(t, body, "every 3s")
}

func TestHandleQueuePendingConfirmationLinksToConfirm(t *testing.T) {
	srv := newHandlerTestServer(t, testdataMovies+"/Pending Confirm (2026)", "")
	rec := get(t, srv, "/")
	body := rec.Body.String()
	mustContain(t, body, "badge-pending-confirmation")
	mustContain(t, body, `href="/confirm/44444444-4444-4444-4444-444444444444"`)
}

// --- GET /confirm/:id --------------------------------------------------------

func TestHandleConfirmUnknownID404s(t *testing.T) {
	srv := newHandlerTestServer(t, t.TempDir(), t.TempDir())
	rec := get(t, srv, "/confirm/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Not Found")
}

func TestHandleConfirmBluray(t *testing.T) {
	srv := newHandlerTestServer(t, testdataMovies, "")
	rec := get(t, srv, "/confirm/11111111-1111-1111-1111-111111111111")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Disc label + candidates, top candidate first.
	mustContain(t, body, "MARVELS_IRON_MAN_3_BLU_RAY")
	mustContain(t, body, "Iron Man 3")
	mustContain(t, body, "TMDB 68721")
	mustContain(t, body, "Avengers: Age of Ultron")

	// Tracks table: role, role_reason, output_file, and the alternate_cut
	// flag.
	mustContain(t, body, "Iron Man 3 (2013).mkv")
	mustContain(t, body, "longest track")
	mustContain(t, body, "alternate_cut")
	mustContain(t, body, "Needs review")
	mustContain(t, body, "row-flagged")

	// The confirm form itself still posts to #23's endpoint...
	mustContain(t, body, `hx-post="/confirm/11111111-1111-1111-1111-111111111111"`)
	// ...but this fixture is status complete, so #23's locking (see
	// isLocked in handlers.go) renders the role-override and forced-
	// subtitle controls as disabled text instead of hx-post forms.
	mustNotContain(t, body, `hx-post="/api/tracks/11111111-1111-1111-1111-111111111111/0/role"`)
	mustNotContain(t, body, `hx-post="/api/subtitles/11111111-1111-1111-1111-111111111111/7/forced"`)
	mustContain(t, body, "Locked — rip complete")

	// Subtitles table.
	mustContain(t, body, "hdmv_pgs_subtitle")
	mustContain(t, body, "size ratio 0.06")
}

func TestHandleConfirmCD(t *testing.T) {
	srv := newHandlerTestServer(t, "", testdataMusic)
	rec := get(t, srv, "/confirm/55555555-5555-5555-5555-555555555555")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	mustContain(t, body, "Jamestown Revival")
	mustContain(t, body, "The Education of a Wandering Man")
	// CD candidates render MusicBrainz fields, not TMDB ones.
	mustContain(t, body, "release b3f0a4a1-4d05-4f2e-9c39-1f0d4a2f8c11")
	mustContain(t, body, "Utah")
	// CD track schema: number/title, not makemkv_index/role.
	mustContain(t, body, "Where I Need to Be")
	mustContain(t, body, "California (Cast Iron Soul)")
	// No subtitles section for a CD.
	mustNotContain(t, body, "Subtitles</h2>")
}

func TestHandleConfirmPendingSectionsShowPendingNotEmptyTable(t *testing.T) {
	srv := newHandlerTestServer(t, testdataMovies+"/Pending Confirm (2026)", "")
	rec := get(t, srv, "/confirm/44444444-4444-4444-4444-444444444444")
	body := rec.Body.String()
	mustContain(t, body, "Ripping hasn't run yet")
	mustNotContain(t, body, "<table>")
}

// --- GET /history ------------------------------------------------------------

func TestHandleHistoryEmpty(t *testing.T) {
	srv := newHandlerTestServer(t, t.TempDir(), t.TempDir())
	rec := get(t, srv, "/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Nothing has been ripped yet")
}

func TestHandleHistoryListsCompletedAndErroredNewestFirst(t *testing.T) {
	srv := newHandlerTestServer(t, testdataMovies, testdataMusic)
	rec := get(t, srv, "/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Only complete/error manifests appear — the active (ripping,
	// pending_confirmation) fixtures are excluded.
	mustContain(t, body, "Iron Man 3")
	mustContain(t, body, "Bad Disc")
	mustContain(t, body, "Jamestown Revival")
	mustNotContain(t, body, "Ripping Now")
	mustNotContain(t, body, "Pending Confirm")

	// Bad Disc errored before ripping started (ripped_at is zero), so it
	// sorts by detected_at (2026-09-01) ahead of the others.
	badDiscIdx := strings.Index(body, "Bad Disc")
	ironManIdx := strings.Index(body, "Iron Man 3")
	if badDiscIdx == -1 || ironManIdx == -1 || badDiscIdx > ironManIdx {
		t.Errorf("want Bad Disc (newer) before Iron Man 3 (older); body: %s", body)
	}

	// Error visual distinction + Radarr column.
	mustContain(t, body, "row-error")
	mustContain(t, body, "badge-error")
	mustContain(t, body, "added: yes")

	// Links to manifest.json and to /confirm/:id.
	mustContain(t, body, "Iron Man 3 (2013).manifest.json")
	mustContain(t, body, `href="/confirm/11111111-1111-1111-1111-111111111111"`)
}

func TestHandleHistoryFlagsWarningsOnCompleteRip(t *testing.T) {
	// "Warned Movie" is status complete but carries a warning — the case the
	// AC calls out explicitly: "a clean status with warnings is exactly the
	// case worth catching". It must be visually flagged even though its
	// status badge alone would look identical to a clean complete rip.
	srv := newHandlerTestServer(t, testdataMovies+"/Warned Movie (2019)", "")
	rec := get(t, srv, "/history")
	body := rec.Body.String()
	mustContain(t, body, "badge-complete")
	mustContain(t, body, "row-warning")
	mustContain(t, body, "low identification confidence")
}

// --- GET /api/status/:id -----------------------------------------------------

func TestHandleStatusUnknownID(t *testing.T) {
	srv := newHandlerTestServer(t, t.TempDir(), t.TempDir())
	rec := get(t, srv, "/api/status/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	mustContain(t, rec.Body.String(), "Unknown rip")
}

func TestHandleStatusFragment(t *testing.T) {
	srv := newHandlerTestServer(t, testdataMovies+"/Ripping Now (2026)", "")
	rec := get(t, srv, "/api/status/33333333-3333-3333-3333-333333333333")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// A fragment, not a full page.
	mustNotContain(t, body, "<!doctype html>")
	mustContain(t, body, "badge-ripping")
	mustContain(t, body, "every 3s")
}

func TestHandleStatusStopsPollingOnceComplete(t *testing.T) {
	srv := newHandlerTestServer(t, testdataMovies+"/Iron Man 3 (2013)", "")
	rec := get(t, srv, "/api/status/11111111-1111-1111-1111-111111111111")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "badge-complete")
	mustNotContain(t, body, "hx-get")
	mustNotContain(t, body, "hx-trigger")
}

// --- POST /confirm/:id (#23) ------------------------------------------------

func TestHandleConfirmSubmitUnknownID404s(t *testing.T) {
	srv := newHandlerTestServer(t, t.TempDir(), t.TempDir())
	rec := post(t, srv, "/confirm/does-not-exist", url.Values{"tmdb_id": {"1"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleConfirmSubmitBluRayCandidateSelect(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/Pending Confirm (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/confirm/44444444-4444-4444-4444-444444444444", url.Values{"tmdb_id": {"555"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body, "Pending Confirm")
	mustContain(t, body, "badge-ripping")

	m := readManifest(t, movies, "44444444-4444-4444-4444-444444444444")
	if m.Status != manifest.StatusRipping {
		t.Errorf("status = %q, want ripping", m.Status)
	}
	if !m.Identification.Confirmed {
		t.Error("Confirmed = false, want true")
	}
	if m.Identification.ConfirmedBy != "user" {
		t.Errorf("ConfirmedBy = %q, want user", m.Identification.ConfirmedBy)
	}
	if m.Identification.ConfirmedAt == nil {
		t.Error("ConfirmedAt is nil, want set")
	}
	if m.Identification.TMDBID == nil || *m.Identification.TMDBID != 555 {
		t.Errorf("TMDBID = %v, want 555", m.Identification.TMDBID)
	}
	if m.Identification.Title != "Pending Confirm" {
		t.Errorf("Title = %q, want %q", m.Identification.Title, "Pending Confirm")
	}
	if m.Identification.Year != 2026 {
		t.Errorf("Year = %d, want 2026", m.Identification.Year)
	}
}

func TestHandleConfirmSubmitBluRayUnknownCandidate(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/Pending Confirm (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/confirm/44444444-4444-4444-4444-444444444444", url.Values{"tmdb_id": {"999"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}

	m := readManifest(t, movies, "44444444-4444-4444-4444-444444444444")
	if m.Identification.Confirmed {
		t.Error("Confirmed = true, want false (rejected request must not mutate)")
	}
}

func TestHandleConfirmSubmitManualQuery(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/Pending Confirm (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/confirm/44444444-4444-4444-4444-444444444444", url.Values{"query": {"My Manual Title"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	m := readManifest(t, movies, "44444444-4444-4444-4444-444444444444")
	if !m.Identification.Confirmed || m.Identification.ConfirmedBy != "user" {
		t.Errorf("Confirmed/ConfirmedBy = %v/%q, want true/user", m.Identification.Confirmed, m.Identification.ConfirmedBy)
	}
	if m.Identification.Title != "My Manual Title" {
		t.Errorf("Title = %q, want %q", m.Identification.Title, "My Manual Title")
	}
	if m.Identification.TMDBID != nil {
		t.Errorf("TMDBID = %v, want nil (no candidate was looked up)", *m.Identification.TMDBID)
	}
	if m.Status != manifest.StatusRipping {
		t.Errorf("status = %q, want ripping", m.Status)
	}
}

func TestHandleConfirmSubmitNoSelectionOrQuery(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/Pending Confirm (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/confirm/44444444-4444-4444-4444-444444444444", url.Values{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleConfirmSubmitIdempotent(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/Iron Man 3 (2013)")
	srv := newHandlerTestServer(t, movies, "")

	// Iron Man 3 is already confirmed and complete; a repeat confirm with a
	// *different* candidate must be a no-op, not relabel or restart it.
	rec := post(t, srv, "/confirm/11111111-1111-1111-1111-111111111111", url.Values{"tmdb_id": {"99861"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	m := readManifest(t, movies, "11111111-1111-1111-1111-111111111111")
	if m.Status != manifest.StatusComplete {
		t.Errorf("status = %q, want complete (must not restart the rip)", m.Status)
	}
	if m.Identification.TMDBID == nil || *m.Identification.TMDBID != 68721 {
		t.Errorf("TMDBID = %v, want unchanged 68721", m.Identification.TMDBID)
	}
	if m.Identification.Title != "Iron Man 3" {
		t.Errorf("Title = %q, want unchanged %q", m.Identification.Title, "Iron Man 3")
	}
}

func TestHandleConfirmSubmitCDCandidateSelect(t *testing.T) {
	music := copyFixtureDir(t, testdataMusic+"/Pending Artist/Pending CD (2026)")
	srv := newHandlerTestServer(t, "", music)

	rec := post(t, srv, "/confirm/77777777-7777-7777-7777-777777777777", url.Values{
		"mb_release_id": {"aaaa1111-1111-1111-1111-111111111111"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	m := readManifest(t, music, "77777777-7777-7777-7777-777777777777")
	if !m.Identification.Confirmed || m.Identification.ConfirmedBy != "user" {
		t.Errorf("Confirmed/ConfirmedBy = %v/%q, want true/user", m.Identification.Confirmed, m.Identification.ConfirmedBy)
	}
	if m.Identification.MBReleaseID != "aaaa1111-1111-1111-1111-111111111111" {
		t.Errorf("MBReleaseID = %q, want aaaa1111-...", m.Identification.MBReleaseID)
	}
	if m.Identification.MBReleaseGroupID != "bbbb2222-2222-2222-2222-222222222222" {
		t.Errorf("MBReleaseGroupID = %q, want bbbb2222-...", m.Identification.MBReleaseGroupID)
	}
	if m.Identification.Artist != "Pending Artist" || m.Identification.Album != "Pending Album" {
		t.Errorf("Artist/Album = %q/%q, want Pending Artist/Pending Album", m.Identification.Artist, m.Identification.Album)
	}
	if m.Status != manifest.StatusRipping {
		t.Errorf("status = %q, want ripping", m.Status)
	}
}

func TestHandleConfirmSubmitCDManualQuery(t *testing.T) {
	music := copyFixtureDir(t, testdataMusic+"/Pending Artist/Pending CD (2026)")
	srv := newHandlerTestServer(t, "", music)

	rec := post(t, srv, "/confirm/77777777-7777-7777-7777-777777777777", url.Values{"query": {"Some Artist / Some Album"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	m := readManifest(t, music, "77777777-7777-7777-7777-777777777777")
	if m.Identification.Artist != "Some Artist" {
		t.Errorf("Artist = %q, want %q", m.Identification.Artist, "Some Artist")
	}
	if m.Identification.Album != "Some Album" {
		t.Errorf("Album = %q, want %q", m.Identification.Album, "Some Album")
	}
}

// --- POST /api/tracks/:id/:index/role (#23) --------------------------------

func TestHandleTrackRoleHappyPath(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/tracks/88888888-8888-8888-8888-888888888888/1/role", url.Values{"role": {"feature"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body, "set by user")
	mustNotContain(t, body, "row-flagged") // no longer alternate_cut

	m := readManifest(t, movies, "88888888-8888-8888-8888-888888888888")
	if m.Tracks[1].Role != manifest.RoleFeature {
		t.Errorf("Role = %q, want feature", m.Tracks[1].Role)
	}
	if m.Tracks[1].RoleReason != "set by user" {
		t.Errorf("RoleReason = %q, want %q", m.Tracks[1].RoleReason, "set by user")
	}
}

func TestHandleTrackRoleInvalidRole(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/tracks/88888888-8888-8888-8888-888888888888/1/role", url.Values{"role": {"bogus"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleTrackRoleUnknownIndex(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/tracks/88888888-8888-8888-8888-888888888888/99/role", url.Values{"role": {"feature"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleTrackRoleInvalidIndexSyntax(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/tracks/88888888-8888-8888-8888-888888888888/not-a-number/role", url.Values{"role": {"feature"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleTrackRoleUnknownManifestID(t *testing.T) {
	srv := newHandlerTestServer(t, t.TempDir(), t.TempDir())
	rec := post(t, srv, "/api/tracks/does-not-exist/0/role", url.Values{"role": {"feature"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleTrackRoleRejectsCD(t *testing.T) {
	music := copyFixtureDir(t, testdataMusic+"/Jamestown Revival/The Education of a Wandering Man")
	srv := newHandlerTestServer(t, "", music)

	rec := post(t, srv, "/api/tracks/55555555-5555-5555-5555-555555555555/1/role", url.Values{"role": {"feature"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleTrackRoleLockedOnceComplete(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/Iron Man 3 (2013)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/tracks/11111111-1111-1111-1111-111111111111/0/role", url.Values{"role": {"extra"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}

	m := readManifest(t, movies, "11111111-1111-1111-1111-111111111111")
	if m.Tracks[0].Role != manifest.RoleFeature {
		t.Errorf("Role = %q, want unchanged feature (locked)", m.Tracks[0].Role)
	}
}

// --- POST /api/subtitles/:id/:stream_index/forced (#23) --------------------

func TestHandleSubtitleForcedHappyPathSet(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/subtitles/88888888-8888-8888-8888-888888888888/12/forced", url.Values{"forced": {"true"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "set by user")

	m := readManifest(t, movies, "88888888-8888-8888-8888-888888888888")
	var sub *manifest.Subtitle
	for i := range m.Subtitles {
		if m.Subtitles[i].StreamIndex == 12 {
			sub = &m.Subtitles[i]
		}
	}
	if sub == nil || !sub.ForcedCandidate {
		t.Fatalf("stream 12 ForcedCandidate not set: %+v", sub)
	}
	if sub.ForcedCandidateReason == nil || *sub.ForcedCandidateReason != "set by user" {
		t.Errorf("ForcedCandidateReason = %v, want %q", sub.ForcedCandidateReason, "set by user")
	}
}

func TestHandleSubtitleForcedHappyPathClear(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/subtitles/88888888-8888-8888-8888-888888888888/7/forced", url.Values{"forced": {"false"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "cleared by user")

	m := readManifest(t, movies, "88888888-8888-8888-8888-888888888888")
	var sub *manifest.Subtitle
	for i := range m.Subtitles {
		if m.Subtitles[i].StreamIndex == 7 {
			sub = &m.Subtitles[i]
		}
	}
	if sub == nil || sub.ForcedCandidate {
		t.Fatalf("stream 7 ForcedCandidate not cleared: %+v", sub)
	}
	if sub.ForcedCandidateReason == nil || *sub.ForcedCandidateReason != "cleared by user" {
		t.Errorf("ForcedCandidateReason = %v, want %q", sub.ForcedCandidateReason, "cleared by user")
	}
}

func TestHandleSubtitleForcedInvalidValue(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/subtitles/88888888-8888-8888-8888-888888888888/7/forced", url.Values{"forced": {"maybe"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSubtitleForcedUnknownStreamIndex(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/subtitles/88888888-8888-8888-8888-888888888888/999/forced", url.Values{"forced": {"true"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSubtitleForcedInvalidIndexSyntax(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/In Progress (2026)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/subtitles/88888888-8888-8888-8888-888888888888/not-a-number/forced", url.Values{"forced": {"true"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSubtitleForcedUnknownManifestID(t *testing.T) {
	srv := newHandlerTestServer(t, t.TempDir(), t.TempDir())
	rec := post(t, srv, "/api/subtitles/does-not-exist/7/forced", url.Values{"forced": {"true"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleSubtitleForcedRejectsCD(t *testing.T) {
	music := copyFixtureDir(t, testdataMusic+"/Jamestown Revival/The Education of a Wandering Man")
	srv := newHandlerTestServer(t, "", music)

	rec := post(t, srv, "/api/subtitles/55555555-5555-5555-5555-555555555555/7/forced", url.Values{"forced": {"true"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSubtitleForcedLockedOnceComplete(t *testing.T) {
	movies := copyFixtureDir(t, testdataMovies+"/Iron Man 3 (2013)")
	srv := newHandlerTestServer(t, movies, "")

	rec := post(t, srv, "/api/subtitles/11111111-1111-1111-1111-111111111111/7/forced", url.Values{"forced": {"false"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}

	m := readManifest(t, movies, "11111111-1111-1111-1111-111111111111")
	var sub *manifest.Subtitle
	for i := range m.Subtitles {
		if m.Subtitles[i].StreamIndex == 7 {
			sub = &m.Subtitles[i]
		}
	}
	if sub == nil || !sub.ForcedCandidate {
		t.Fatalf("stream 7 ForcedCandidate changed despite lock: %+v", sub)
	}
}

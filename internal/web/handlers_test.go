package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sarumont/ama/config"
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

	// Role override + forced subtitle controls point at #23's documented
	// (not-yet-implemented) endpoints.
	mustContain(t, body, `hx-post="/api/tracks/11111111-1111-1111-1111-111111111111/0/role"`)
	mustContain(t, body, `hx-post="/api/subtitles/11111111-1111-1111-1111-111111111111/7/forced"`)
	mustContain(t, body, `hx-post="/confirm/11111111-1111-1111-1111-111111111111"`)

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

package musicbrainz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New("ama-test/0.0 ( https://example.invalid )", srv.Client(), srv.URL)
}

func TestByDiscIDReturnsReleases(t *testing.T) {
	var got *http.Request
	body := fixture(t, "discid_match.json")
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	releases, err := client.ByDiscID(context.Background(), "T3STmatchD1scIDXXXXXXXXXXXX-")
	if err != nil {
		t.Fatalf("ByDiscID: %v", err)
	}

	want := []Release{
		{
			MBReleaseID:      "22222222-2222-2222-2222-222222222222",
			MBReleaseGroupID: "33333333-3333-3333-3333-333333333333",
			Artist:           "Jamestown Revival",
			Album:            "The Education of a Wandering Man",
			Year:             2014,
			Tracks: []Track{
				{Number: 1, Title: "Where I Need to Be", DurationSeconds: 207},
				{Number: 2, Title: "California (Cast Iron Soul)", DurationSeconds: 231},
			},
		},
		{
			MBReleaseID:      "44444444-4444-4444-4444-444444444444",
			MBReleaseGroupID: "33333333-3333-3333-3333-333333333333",
			Artist:           "Jamestown Revival & Friends",
			Album:            "The Education of a Wandering Man (Deluxe)",
			Year:             2015,
			Tracks: []Track{
				{Number: 1, Title: "Where I Need to Be", DurationSeconds: 207},
			},
		},
	}
	if len(releases) != len(want) {
		t.Fatalf("ByDiscID returned %d releases, want %d: %+v", len(releases), len(want), releases)
	}
	for i := range want {
		if !equalRelease(releases[i], want[i]) {
			t.Errorf("releases[%d] = %+v, want %+v", i, releases[i], want[i])
		}
	}

	if path := "/discid/T3STmatchD1scIDXXXXXXXXXXXX-"; got.URL.Path != path {
		t.Errorf("path = %q, want %q", got.URL.Path, path)
	}
	if inc := got.URL.Query().Get("inc"); inc != "recordings+artist-credits+release-groups" {
		t.Errorf("inc param = %q, want %q", inc, "recordings+artist-credits+release-groups")
	}
	if ua := got.Header.Get("User-Agent"); ua == "" {
		t.Error("User-Agent header was not sent")
	}
}

func TestByDiscIDNotFoundIsNotAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": "Not Found"}`))
	})

	releases, err := client.ByDiscID(context.Background(), "N0MATCHd1scIDYYYYYYYYYYYY-")
	if err != nil {
		t.Fatalf("ByDiscID: %v", err)
	}
	if len(releases) != 0 {
		t.Errorf("ByDiscID returned %d releases for a 404, want 0", len(releases))
	}
}

func TestByDiscIDRateLimited(t *testing.T) {
	body := fixture(t, "rate_limited.json")
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body)
	})

	_, err := client.ByDiscID(context.Background(), "anything")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRateLimited)", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError in the chain", err)
	}
	if apiErr.RetryAfter.Seconds() != 2 {
		t.Errorf("RetryAfter = %v, want 2s", apiErr.RetryAfter)
	}
}

func TestByDiscIDRequiresDiscID(t *testing.T) {
	client := New("", nil, "")
	if _, err := client.ByDiscID(context.Background(), ""); err == nil {
		t.Fatal("ByDiscID: want error for an empty disc id, got nil")
	}
}

func equalRelease(a, b Release) bool {
	if a.MBReleaseID != b.MBReleaseID || a.MBReleaseGroupID != b.MBReleaseGroupID ||
		a.Artist != b.Artist || a.Album != b.Album || a.Year != b.Year {
		return false
	}
	if len(a.Tracks) != len(b.Tracks) {
		return false
	}
	for i := range a.Tracks {
		if a.Tracks[i] != b.Tracks[i] {
			return false
		}
	}
	return true
}

package identify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fixture reads a recorded TMDB response from testdata.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// newTestClient starts a server running handler and returns a Client pointed at
// it, along with the server so tests can inspect recorded requests.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New("test-token", srv.Client(), srv.URL)
}

// serveFixture replies with status and the named fixture body.
func serveFixture(t *testing.T, status int, name string) http.HandlerFunc {
	body := fixture(t, name)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	}
}

func TestSearchReturnsCandidates(t *testing.T) {
	var got *http.Request
	body := fixture(t, "search_movie.json")
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	candidates, err := client.Search(context.Background(), "Iron Man 3", 2013)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	want := []Candidate{
		{
			TMDBID:        68721,
			Title:         "Iron Man 3",
			OriginalTitle: "Iron Man Three",
			Year:          2013,
			Overview:      "When Tony Stark's world is torn apart by a formidable terrorist called the Mandarin, he starts an odyssey of rebuilding and retribution.",
			PosterPath:    "/qhPtAc1TKbMPqNvcdXSOn9Bn7hZ.jpg",
			Popularity:    51.286,
		},
		{
			TMDBID:        194588,
			Title:         "Iron Man 3: A Marvel Super Special",
			OriginalTitle: "Iron Man 3: A Marvel Super Special",
			Year:          0,
			Popularity:    1.24,
		},
		{
			TMDBID:        24428,
			Title:         "The Avengers",
			OriginalTitle: "The Avengers",
			Year:          2012,
			Overview:      "When an unexpected enemy emerges and threatens global safety and security, Nick Fury finds himself in need of a team.",
			PosterPath:    "/RYMX2wcKCBAr24UyPD7xwmjaTn.jpg",
			Popularity:    89.771,
		},
	}
	if !reflect.DeepEqual(candidates, want) {
		t.Errorf("candidates = %+v, want %+v", candidates, want)
	}

	if got.URL.Path != "/search/movie" {
		t.Errorf("path = %q, want /search/movie", got.URL.Path)
	}
	if q := got.URL.Query().Get("query"); q != "Iron Man 3" {
		t.Errorf("query param = %q, want %q", q, "Iron Man 3")
	}
	if y := got.URL.Query().Get("year"); y != "2013" {
		t.Errorf("year param = %q, want 2013", y)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer test-token")
	}
}

func TestSearchOmitsZeroYear(t *testing.T) {
	var got *http.Request
	body := fixture(t, "search_movie_empty.json")
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write(body)
	})

	if _, err := client.Search(context.Background(), "Solaris", 0); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, ok := got.URL.Query()["year"]; ok {
		t.Errorf("year param present for zero year: %q", got.URL.RawQuery)
	}
}

func TestSearchEmptyResults(t *testing.T) {
	client := newTestClient(t, serveFixture(t, http.StatusOK, "search_movie_empty.json"))

	candidates, err := client.Search(context.Background(), "no such disc", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("candidates = %+v, want none", candidates)
	}
}

func TestSearchUnauthorized(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		client := newTestClient(t, serveFixture(t, status, "unauthorized.json"))

		_, err := client.Search(context.Background(), "Iron Man 3", 0)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("status %d: err = %v, want ErrUnauthorized", status, err)
		}
		if errors.Is(err, ErrRateLimited) {
			t.Errorf("status %d: auth failure also matched ErrRateLimited", status)
		}

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("status %d: err = %T, want *APIError", status, err)
		}
		if apiErr.StatusCode != status {
			t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, status)
		}
		if apiErr.TMDBCode != 7 {
			t.Errorf("TMDBCode = %d, want 7", apiErr.TMDBCode)
		}
		if !strings.Contains(err.Error(), "Invalid API key") {
			t.Errorf("error %q does not include TMDB's status message", err)
		}
		if strings.Contains(err.Error(), "test-token") {
			t.Errorf("error %q leaks the credential", err)
		}
	}
}

func TestSearchRateLimited(t *testing.T) {
	body := fixture(t, "rate_limited.json")
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(body)
	})

	_, err := client.Search(context.Background(), "Iron Man 3", 0)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Errorf("rate limit also matched ErrUnauthorized")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T, want *APIError", err)
	}
	if apiErr.RetryAfter != 3*time.Second {
		t.Errorf("RetryAfter = %v, want 3s", apiErr.RetryAfter)
	}
}

func TestSearchMalformedJSON(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results": [{"id": `))
	})

	candidates, err := client.Search(context.Background(), "Iron Man 3", 0)
	if err == nil {
		t.Fatalf("Search succeeded on malformed JSON, got %+v", candidates)
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Errorf("err = %v, want a decode error", err)
	}
}

func TestSearchNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // nothing is listening now

	client := New("test-token", srv.Client(), addr)
	if _, err := client.Search(context.Background(), "Iron Man 3", 0); err == nil {
		t.Fatal("Search succeeded against a closed server")
	}
}

func TestSearchHonorsContextCancellation(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Search(ctx, "Iron Man 3", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestIMDBID(t *testing.T) {
	var got *http.Request
	body := fixture(t, "external_ids.json")
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write(body)
	})

	id, err := client.IMDBID(context.Background(), 68721)
	if err != nil {
		t.Fatalf("IMDBID: %v", err)
	}
	if id != "tt1300854" {
		t.Errorf("imdb id = %q, want tt1300854", id)
	}
	if got.URL.Path != "/movie/68721/external_ids" {
		t.Errorf("path = %q, want /movie/68721/external_ids", got.URL.Path)
	}
}

func TestNewDefaults(t *testing.T) {
	client := New("test-token", nil, "")
	if client.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", client.baseURL, DefaultBaseURL)
	}
	if client.http == nil || client.http.Timeout == 0 {
		t.Errorf("default http client has no timeout: %+v", client.http)
	}
}

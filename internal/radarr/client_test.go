package radarr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sarumont/ama/config"
)

const testAPIKey = "test-api-key"

// recorded captures what the fake service received, so tests can assert on the
// request AMA actually sends.
type recorded struct {
	method string
	path   string
	query  string
	apiKey string
	body   map[string]any
}

// fakeArr is an httptest server standing in for Radarr or Sonarr. Handlers are
// keyed by request path; any path without a handler fails the test.
type fakeArr struct {
	t        *testing.T
	server   *httptest.Server
	handlers map[string]http.HandlerFunc
	requests []recorded
}

func newFakeArr(t *testing.T, handlers map[string]http.HandlerFunc) *fakeArr {
	t.Helper()
	f := &fakeArr{t: t, handlers: handlers}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeArr) serve(w http.ResponseWriter, r *http.Request) {
	rec := recorded{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.RawQuery,
		apiKey: r.Header.Get("X-Api-Key"),
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&rec.body); err != nil && err.Error() != "EOF" {
			f.t.Errorf("decoding request body for %s: %v", r.URL.Path, err)
		}
	}
	f.requests = append(f.requests, rec)

	handler, ok := f.handlers[r.URL.Path]
	if !ok {
		f.t.Errorf("unexpected request to %s", r.URL.Path)
		http.Error(w, "unexpected path", http.StatusNotFound)
		return
	}
	handler(w, r)
}

func (f *fakeArr) config() config.Arr {
	return config.Arr{Enabled: true, URL: f.server.URL, APIKey: testAPIKey}
}

func (f *fakeArr) client() *http.Client {
	return f.server.Client()
}

// request returns the single recorded request for path, failing if there was
// not exactly one.
func (f *fakeArr) request(path string) recorded {
	f.t.Helper()
	var found []recorded
	for _, r := range f.requests {
		if r.path == path {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("want exactly 1 request to %s, got %d", path, len(found))
	}
	return found[0]
}

func (f *fakeArr) requestCount(path string) int {
	n := 0
	for _, r := range f.requests {
		if r.path == path {
			n++
		}
	}
	return n
}

// writeJSON responds 200 with the given raw JSON body.
func writeJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

// writeStatus responds with the given status and raw JSON body.
func writeStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const qualityProfiles = `[{"id":4,"name":"HD-1080p"},{"id":2,"name":"Any"},{"id":7,"name":"Ultra-HD"}]`

func TestRadarrAddMovie(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{
		"/api/v3/qualityprofile": writeJSON(qualityProfiles),
		"/api/v3/movie/lookup":   writeJSON(`[{"title":"Iron Man 3","year":2013,"tmdbId":68721,"titleSlug":"iron-man-3-68721"}]`),
		"/api/v3/movie":          writeStatus(http.StatusCreated, `{"id":41,"title":"Iron Man 3"}`),
	})

	client := NewRadarr(fake.config(), fake.client())
	got, err := client.AddMovie(t.Context(), 68721, AddOptions{RootFolderPath: "/media/library/movies"})
	if err != nil {
		t.Fatalf("AddMovie: %v", err)
	}

	want := AddResult{ID: 41, Added: true}
	if got != want {
		t.Errorf("AddMovie result = %+v, want %+v", got, want)
	}

	lookup := fake.request("/api/v3/movie/lookup")
	if lookup.query != "term=tmdb%3A68721" {
		t.Errorf("lookup query = %q, want term=tmdb:68721", lookup.query)
	}
	if lookup.apiKey != testAPIKey {
		t.Errorf("lookup X-Api-Key = %q, want %q", lookup.apiKey, testAPIKey)
	}

	post := fake.request("/api/v3/movie")
	if post.method != http.MethodPost {
		t.Errorf("add method = %s, want POST", post.method)
	}
	if post.apiKey != testAPIKey {
		t.Errorf("add X-Api-Key = %q, want %q", post.apiKey, testAPIKey)
	}
	// Fields carried over from the lookup must survive into the add.
	if post.body["title"] != "Iron Man 3" {
		t.Errorf("add body title = %v, want Iron Man 3", post.body["title"])
	}
	if post.body["titleSlug"] != "iron-man-3-68721" {
		t.Errorf("add body titleSlug = %v, want iron-man-3-68721", post.body["titleSlug"])
	}
	if post.body["rootFolderPath"] != "/media/library/movies" {
		t.Errorf("add body rootFolderPath = %v", post.body["rootFolderPath"])
	}
	// Lowest-numbered profile from the instance, not a hardcoded 1.
	if post.body["qualityProfileId"] != float64(2) {
		t.Errorf("add body qualityProfileId = %v, want 2", post.body["qualityProfileId"])
	}
	if post.body["monitored"] != true {
		t.Errorf("add body monitored = %v, want true", post.body["monitored"])
	}
	addOptions, ok := post.body["addOptions"].(map[string]any)
	if !ok {
		t.Fatalf("add body addOptions = %v, want an object", post.body["addOptions"])
	}
	if addOptions["searchForMovie"] != false {
		t.Errorf("add body addOptions.searchForMovie = %v, want false", addOptions["searchForMovie"])
	}
}

func TestSonarrAddSeries(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{
		"/api/v3/qualityprofile": writeJSON(qualityProfiles),
		"/api/v3/series/lookup":  writeJSON(`[{"title":"Breaking Bad","tvdbId":81189,"titleSlug":"breaking-bad"}]`),
		"/api/v3/series":         writeStatus(http.StatusCreated, `{"id":9,"title":"Breaking Bad"}`),
	})

	client := NewSonarr(fake.config(), fake.client())
	got, err := client.AddSeries(t.Context(), 81189, AddOptions{RootFolderPath: "/media/library/tv"})
	if err != nil {
		t.Fatalf("AddSeries: %v", err)
	}

	want := AddResult{ID: 9, Added: true}
	if got != want {
		t.Errorf("AddSeries result = %+v, want %+v", got, want)
	}

	lookup := fake.request("/api/v3/series/lookup")
	if lookup.query != "term=tvdb%3A81189" {
		t.Errorf("lookup query = %q, want term=tvdb:81189", lookup.query)
	}

	post := fake.request("/api/v3/series")
	if post.body["title"] != "Breaking Bad" {
		t.Errorf("add body title = %v, want Breaking Bad", post.body["title"])
	}
	if post.body["rootFolderPath"] != "/media/library/tv" {
		t.Errorf("add body rootFolderPath = %v", post.body["rootFolderPath"])
	}
	if post.body["qualityProfileId"] != float64(2) {
		t.Errorf("add body qualityProfileId = %v, want 2", post.body["qualityProfileId"])
	}
	if post.body["seasonFolder"] != true {
		t.Errorf("add body seasonFolder = %v, want true", post.body["seasonFolder"])
	}
	if post.body["languageProfileId"] != float64(1) {
		t.Errorf("add body languageProfileId = %v, want 1", post.body["languageProfileId"])
	}
	addOptions, ok := post.body["addOptions"].(map[string]any)
	if !ok {
		t.Fatalf("add body addOptions = %v, want an object", post.body["addOptions"])
	}
	if addOptions["searchForMissingEpisodes"] != false {
		t.Errorf("add body addOptions.searchForMissingEpisodes = %v, want false", addOptions["searchForMissingEpisodes"])
	}
}

func TestAddExplicitQualityProfileSkipsLookup(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{
		"/api/v3/movie/lookup": writeJSON(`[{"title":"Arrival","tmdbId":329865}]`),
		"/api/v3/movie":        writeStatus(http.StatusCreated, `{"id":3}`),
	})

	client := NewRadarr(fake.config(), fake.client())
	if _, err := client.AddMovie(t.Context(), 329865, AddOptions{
		RootFolderPath:   "/movies",
		QualityProfileID: 6,
	}); err != nil {
		t.Fatalf("AddMovie: %v", err)
	}

	if n := fake.requestCount("/api/v3/qualityprofile"); n != 0 {
		t.Errorf("quality profile requests = %d, want 0", n)
	}
	if got := fake.request("/api/v3/movie").body["qualityProfileId"]; got != float64(6) {
		t.Errorf("qualityProfileId = %v, want 6", got)
	}
}

func TestAddAlreadyExists(t *testing.T) {
	tests := []struct {
		name         string
		lookupPath   string
		resourcePath string
		status       int
		body         string
		add          func(*testing.T, *fakeArr) (AddResult, error)
	}{
		{
			name:         "radarr validation failure array",
			lookupPath:   "/api/v3/movie/lookup",
			resourcePath: "/api/v3/movie",
			status:       http.StatusBadRequest,
			body:         `[{"propertyName":"TmdbId","errorMessage":"This movie has already been added","severity":"error"}]`,
			add: func(t *testing.T, f *fakeArr) (AddResult, error) {
				return NewRadarr(f.config(), f.client()).
					AddMovie(t.Context(), 68721, AddOptions{RootFolderPath: "/movies", QualityProfileID: 1})
			},
		},
		{
			name:         "sonarr validation failure array",
			lookupPath:   "/api/v3/series/lookup",
			resourcePath: "/api/v3/series",
			status:       http.StatusBadRequest,
			body:         `[{"propertyName":"TvdbId","errorMessage":"This series has already been added","severity":"error"}]`,
			add: func(t *testing.T, f *fakeArr) (AddResult, error) {
				return NewSonarr(f.config(), f.client()).
					AddSeries(t.Context(), 81189, AddOptions{RootFolderPath: "/tv", QualityProfileID: 1})
			},
		},
		{
			name:         "conflict with unwrapped message",
			lookupPath:   "/api/v3/movie/lookup",
			resourcePath: "/api/v3/movie",
			status:       http.StatusConflict,
			body:         `{"message":"Movie already exists"}`,
			add: func(t *testing.T, f *fakeArr) (AddResult, error) {
				return NewRadarr(f.config(), f.client()).
					AddMovie(t.Context(), 68721, AddOptions{RootFolderPath: "/movies", QualityProfileID: 1})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeArr(t, map[string]http.HandlerFunc{
				tc.lookupPath:   writeJSON(`[{"title":"Already Here"}]`),
				tc.resourcePath: writeStatus(tc.status, tc.body),
			})

			got, err := tc.add(t, fake)
			if err != nil {
				t.Fatalf("add: want nil error for an existing item, got %v", err)
			}
			want := AddResult{AlreadyExists: true}
			if got != want {
				t.Errorf("result = %+v, want %+v", got, want)
			}
		})
	}
}

// A 400 that is not about a duplicate must still surface as an error.
func TestAddOtherBadRequestIsAnError(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{
		"/api/v3/movie/lookup": writeJSON(`[{"title":"Broken"}]`),
		"/api/v3/movie": writeStatus(http.StatusBadRequest,
			`[{"propertyName":"RootFolderPath","errorMessage":"Folder does not exist","severity":"error"}]`),
	})

	client := NewRadarr(fake.config(), fake.client())
	_, err := client.AddMovie(t.Context(), 68721, AddOptions{RootFolderPath: "/nope", QualityProfileID: 1})
	if err == nil {
		t.Fatal("AddMovie: want an error, got nil")
	}

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Error(), "Folder does not exist") {
		t.Errorf("error message %q does not include the service response", apiErr.Error())
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Error("a 400 must not report as an auth failure")
	}
}

func TestAuthFailure(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeArr(t, map[string]http.HandlerFunc{
				"/api/v3/qualityprofile": writeStatus(tc.status, `{"error":"Unauthorized"}`),
				"/api/v3/command":        writeStatus(tc.status, `{"error":"Unauthorized"}`),
			})

			client := NewRadarr(config.Arr{URL: fake.server.URL, APIKey: "wrong"}, fake.client())

			if _, err := client.AddMovie(t.Context(), 68721, AddOptions{RootFolderPath: "/movies"}); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("AddMovie error = %v, want ErrUnauthorized", err)
			}
			if err := client.TriggerImportScan(t.Context(), "/movies/Iron Man 3 (2013)"); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("TriggerImportScan error = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestTriggerImportScan(t *testing.T) {
	tests := []struct {
		name    string
		command string
		trigger func(*testing.T, *fakeArr) error
	}{
		{
			name:    "radarr",
			command: "DownloadedMoviesScan",
			trigger: func(t *testing.T, f *fakeArr) error {
				return NewRadarr(f.config(), f.client()).
					TriggerImportScan(t.Context(), "/media/library/movies/Iron Man 3 (2013)")
			},
		},
		{
			name:    "sonarr",
			command: "DownloadedEpisodesScan",
			trigger: func(t *testing.T, f *fakeArr) error {
				return NewSonarr(f.config(), f.client()).
					TriggerImportScan(t.Context(), "/media/library/movies/Iron Man 3 (2013)")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeArr(t, map[string]http.HandlerFunc{
				"/api/v3/command": writeStatus(http.StatusCreated, `{"id":12,"status":"queued"}`),
			})

			if err := tc.trigger(t, fake); err != nil {
				t.Fatalf("TriggerImportScan: %v", err)
			}

			req := fake.request("/api/v3/command")
			if req.method != http.MethodPost {
				t.Errorf("method = %s, want POST", req.method)
			}
			if req.apiKey != testAPIKey {
				t.Errorf("X-Api-Key = %q, want %q", req.apiKey, testAPIKey)
			}
			if req.body["name"] != tc.command {
				t.Errorf("command name = %v, want %v", req.body["name"], tc.command)
			}
			if req.body["path"] != "/media/library/movies/Iron Man 3 (2013)" {
				t.Errorf("command path = %v", req.body["path"])
			}
		})
	}
}

func TestTriggerImportScanRequiresPath(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{})

	if err := NewRadarr(fake.config(), fake.client()).TriggerImportScan(t.Context(), ""); err == nil {
		t.Fatal("want an error for an empty path, got nil")
	}
}

func TestUnreachableServer(t *testing.T) {
	// Start a server only to take a live port, then close it so connections to
	// that address are refused.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cfg := config.Arr{Enabled: true, URL: dead.URL, APIKey: testAPIKey}
	httpClient := dead.Client()
	dead.Close()

	radarr := NewRadarr(cfg, httpClient)
	_, err := radarr.AddMovie(t.Context(), 68721, AddOptions{RootFolderPath: "/movies", QualityProfileID: 1})
	if err == nil {
		t.Fatal("AddMovie: want an error against a closed server, got nil")
	}
	if !strings.Contains(err.Error(), "radarr unreachable at "+dead.URL) {
		t.Errorf("AddMovie error = %q, want it to name the unreachable service and url", err)
	}

	sonarr := NewSonarr(cfg, httpClient)
	if err := sonarr.TriggerImportScan(t.Context(), "/tv"); err == nil {
		t.Fatal("TriggerImportScan: want an error against a closed server, got nil")
	} else if !strings.Contains(err.Error(), "sonarr unreachable at "+dead.URL) {
		t.Errorf("TriggerImportScan error = %q, want it to name the unreachable service and url", err)
	}
}

func TestCancelledContext(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{
		"/api/v3/command": writeStatus(http.StatusCreated, `{"id":1}`),
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := NewRadarr(fake.config(), fake.client()).TriggerImportScan(ctx, "/movies")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestAddRequiresRootFolderPath(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{})

	if _, err := NewRadarr(fake.config(), fake.client()).AddMovie(t.Context(), 68721, AddOptions{}); err == nil {
		t.Error("AddMovie: want an error without a root folder path, got nil")
	}
	if _, err := NewSonarr(fake.config(), fake.client()).AddSeries(t.Context(), 81189, AddOptions{}); err == nil {
		t.Error("AddSeries: want an error without a root folder path, got nil")
	}
}

func TestAddRejectsNonPositiveID(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{})

	if _, err := NewRadarr(fake.config(), fake.client()).AddMovie(t.Context(), 0, AddOptions{RootFolderPath: "/movies"}); err == nil {
		t.Error("AddMovie: want an error for tmdb id 0, got nil")
	}
	if _, err := NewSonarr(fake.config(), fake.client()).AddSeries(t.Context(), -1, AddOptions{RootFolderPath: "/tv"}); err == nil {
		t.Error("AddSeries: want an error for a negative tvdb id, got nil")
	}
}

func TestAddNoLookupMatch(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{
		"/api/v3/movie/lookup": writeJSON(`[]`),
	})

	client := NewRadarr(fake.config(), fake.client())
	_, err := client.AddMovie(t.Context(), 68721, AddOptions{RootFolderPath: "/movies", QualityProfileID: 1})
	if err == nil {
		t.Fatal("want an error when the lookup finds nothing, got nil")
	}
	if !strings.Contains(err.Error(), "tmdb id 68721") {
		t.Errorf("error = %q, want it to name the tmdb id", err)
	}
}

func TestNoQualityProfilesConfigured(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{
		"/api/v3/qualityprofile": writeJSON(`[]`),
	})

	client := NewRadarr(fake.config(), fake.client())
	_, err := client.AddMovie(t.Context(), 68721, AddOptions{RootFolderPath: "/movies"})
	if err == nil {
		t.Fatal("want an error when the instance has no quality profiles, got nil")
	}
	if !strings.Contains(err.Error(), "QualityProfileID") {
		t.Errorf("error = %q, want it to point at AddOptions.QualityProfileID", err)
	}
}

func TestTrailingSlashInURL(t *testing.T) {
	fake := newFakeArr(t, map[string]http.HandlerFunc{
		"/api/v3/command": writeStatus(http.StatusCreated, `{"id":1}`),
	})

	cfg := fake.config()
	cfg.URL += "/"

	if err := NewRadarr(cfg, fake.client()).TriggerImportScan(t.Context(), "/movies"); err != nil {
		t.Fatalf("TriggerImportScan: %v", err)
	}
	fake.request("/api/v3/command")
}

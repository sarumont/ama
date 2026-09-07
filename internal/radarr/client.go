// Package radarr provides minimal clients for the Radarr and Sonarr v3 REST
// APIs. Both are Servarr-suite applications with near-identical request shapes,
// so a single package covers them: Radarr adds a movie by TMDB ID, Sonarr adds
// a series by TVDB ID, and both can be asked to scan a path and import what
// they find there.
//
// Authentication is the Servarr X-Api-Key header, not bearer auth.
//
// The clients do not consult config.Arr.Enabled. A caller decides whether an
// integration is switched on before constructing a client; the client's only
// job is to talk to the service and wrap failures clearly.
//
// Nothing here imports internal/manifest. AddMovie and AddSeries return an
// AddResult describing what happened, and the caller maps that onto the
// manifest's radarr/sonarr blocks.
package radarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sarumont/ama/config"
)

// defaultTimeout applies when a caller does not inject its own *http.Client.
const defaultTimeout = 30 * time.Second

// maxErrorBody caps how much of an error response body is kept in an *Error.
const maxErrorBody = 512

// ErrUnauthorized reports that the service rejected the API key. It is
// reachable with errors.Is on the *Error returned for a 401 or 403.
var ErrUnauthorized = errors.New("radarr: authentication rejected, check api_key")

// Error is an unexpected HTTP response from Radarr or Sonarr.
type Error struct {
	// App is "radarr" or "sonarr".
	App string
	// Op names the operation that failed, e.g. "add movie".
	Op         string
	StatusCode int
	// Body is the response body, truncated.
	Body string
}

func (e *Error) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: %s: unexpected status %d", e.App, e.Op, e.StatusCode)
	}
	return fmt.Sprintf("%s: %s: unexpected status %d: %s", e.App, e.Op, e.StatusCode, e.Body)
}

// Unwrap exposes ErrUnauthorized for the status codes that mean a bad API key.
func (e *Error) Unwrap() error {
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	return nil
}

// AddOptions are the per-call settings shared by AddMovie and AddSeries.
type AddOptions struct {
	// RootFolderPath is the library root the service should manage the item
	// under. Required: neither service accepts an add without one, and there is
	// no safe default to guess.
	RootFolderPath string
	// QualityProfileID selects the quality profile. Zero means "resolve the
	// lowest-numbered profile the service has", which avoids hardcoding an ID
	// that only happens to be right on one instance.
	QualityProfileID int
}

// AddResult describes the outcome of an add.
type AddResult struct {
	// ID is the service-side movie or series ID. Zero when unknown, which is
	// the case for an item that already existed.
	ID int
	// Added is true when this call created the item.
	Added bool
	// AlreadyExists is true when the service rejected the add because it
	// already tracks the item. That is not an error for AMA's purposes: the
	// goal is only that the service knows about the title.
	AlreadyExists bool
}

// Radarr is a client for a Radarr v3 instance.
type Radarr struct {
	arr
}

// Sonarr is a client for a Sonarr v3 instance.
type Sonarr struct {
	arr
}

// NewRadarr builds a Radarr client from the radarr section of the config. A nil
// httpClient gets a default client with a 30s timeout; tests inject the client
// from httptest.NewServer.
func NewRadarr(cfg config.Arr, httpClient *http.Client) *Radarr {
	return &Radarr{newArr("radarr", cfg, httpClient)}
}

// NewSonarr builds a Sonarr client from the sonarr section of the config.
func NewSonarr(cfg config.Arr, httpClient *http.Client) *Sonarr {
	return &Sonarr{newArr("sonarr", cfg, httpClient)}
}

// AddMovie adds the movie with the given TMDB ID to Radarr.
//
// The movie is added monitored and without triggering a search: AMA has already
// produced the file, so Radarr only needs to know the title exists and then
// import what is on disk.
//
// A movie Radarr already tracks yields AddResult{AlreadyExists: true} and a nil
// error.
func (c *Radarr) AddMovie(ctx context.Context, tmdbID int, opts AddOptions) (AddResult, error) {
	if tmdbID <= 0 {
		return AddResult{}, fmt.Errorf("radarr: add movie: tmdb id must be positive, got %d", tmdbID)
	}
	return c.add(ctx, addSpec{
		op:           "add movie",
		lookupPath:   "/api/v3/movie/lookup",
		term:         "tmdb:" + strconv.Itoa(tmdbID),
		resourcePath: "/api/v3/movie",
		subject:      fmt.Sprintf("tmdb id %d", tmdbID),
		fields: map[string]any{
			"minimumAvailability": "released",
			"addOptions":          map[string]any{"searchForMovie": false},
		},
	}, opts)
}

// AddSeries adds the series with the given TVDB ID to Sonarr. Sonarr's v3 API
// is TVDB-keyed, so callers holding a TMDB ID must translate first.
//
// As with AddMovie the series is added monitored and without a search, and an
// already-tracked series yields AddResult{AlreadyExists: true} and a nil error.
func (c *Sonarr) AddSeries(ctx context.Context, tvdbID int, opts AddOptions) (AddResult, error) {
	if tvdbID <= 0 {
		return AddResult{}, fmt.Errorf("sonarr: add series: tvdb id must be positive, got %d", tvdbID)
	}
	return c.add(ctx, addSpec{
		op:           "add series",
		lookupPath:   "/api/v3/series/lookup",
		term:         "tvdb:" + strconv.Itoa(tvdbID),
		resourcePath: "/api/v3/series",
		subject:      fmt.Sprintf("tvdb id %d", tvdbID),
		fields: map[string]any{
			"seasonFolder": true,
			// Sonarr v3.0 requires a language profile; v4 dropped the concept
			// and ignores the field. Sending 1 satisfies both.
			"languageProfileId": 1,
			"addOptions":        map[string]any{"searchForMissingEpisodes": false},
		},
	}, opts)
}

// TriggerImportScan asks Radarr to scan path and import the media it finds.
func (c *Radarr) TriggerImportScan(ctx context.Context, path string) error {
	return c.command(ctx, "trigger import scan", "DownloadedMoviesScan", path)
}

// TriggerImportScan asks Sonarr to scan path and import the media it finds.
func (c *Sonarr) TriggerImportScan(ctx context.Context, path string) error {
	return c.command(ctx, "trigger import scan", "DownloadedEpisodesScan", path)
}

// arr is the transport and the logic Radarr and Sonarr share.
type arr struct {
	app     string
	baseURL string
	apiKey  string
	http    *http.Client
}

func newArr(app string, cfg config.Arr, httpClient *http.Client) arr {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return arr{
		app:     app,
		baseURL: strings.TrimRight(cfg.URL, "/"),
		apiKey:  cfg.APIKey,
		http:    httpClient,
	}
}

// addSpec is the per-service half of an add: the endpoints, the lookup term,
// and the fields that only one of the two services understands.
type addSpec struct {
	op           string
	lookupPath   string
	term         string
	resourcePath string
	subject      string
	fields       map[string]any
}

// add performs the lookup-then-post flow both services need.
//
// Posting a bare {tmdbId, ...} body is unreliable — the services expect a full
// resource and fail in obscure ways without one — so the canonical approach is
// to fetch the resource from the service's own lookup endpoint and post it back
// with the add fields filled in. Keeping the looked-up object as a map means
// every field it carries survives the round trip without modelling the whole
// (large, version-dependent) resource here.
func (a arr) add(ctx context.Context, spec addSpec, opts AddOptions) (AddResult, error) {
	if opts.RootFolderPath == "" {
		return AddResult{}, fmt.Errorf("%s: %s: root folder path is required", a.app, spec.op)
	}

	profileID := opts.QualityProfileID
	if profileID == 0 {
		var err error
		if profileID, err = a.defaultQualityProfileID(ctx, spec.op); err != nil {
			return AddResult{}, err
		}
	}

	var found []map[string]any
	query := url.Values{"term": []string{spec.term}}
	if err := a.getJSON(ctx, spec.op, spec.lookupPath, query, &found); err != nil {
		return AddResult{}, err
	}
	if len(found) == 0 {
		return AddResult{}, fmt.Errorf("%s: %s: no match for %s", a.app, spec.op, spec.subject)
	}

	resource := found[0]
	resource["rootFolderPath"] = opts.RootFolderPath
	resource["qualityProfileId"] = profileID
	resource["monitored"] = true
	for k, v := range spec.fields {
		resource[k] = v
	}

	status, body, err := a.do(ctx, spec.op, http.MethodPost, spec.resourcePath, nil, resource)
	if err != nil {
		return AddResult{}, err
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		var added struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(body, &added); err != nil {
			return AddResult{}, fmt.Errorf("%s: %s: decoding response: %w", a.app, spec.op, err)
		}
		return AddResult{ID: added.ID, Added: true}, nil
	case alreadyExists(status, body):
		return AddResult{AlreadyExists: true}, nil
	default:
		return AddResult{}, a.httpError(spec.op, status, body)
	}
}

// command posts to the shared /api/v3/command endpoint.
//
// importMode is deliberately left unset so the service applies its own default.
// AMA writes its rips into the library root the service already manages, and
// forcing "Move" or "Copy" from here would override the operator's configured
// behaviour for files that are, from the service's point of view, already home.
func (a arr) command(ctx context.Context, op, name, path string) error {
	if path == "" {
		return fmt.Errorf("%s: %s: path is required", a.app, op)
	}
	payload := map[string]any{"name": name, "path": path}

	status, body, err := a.do(ctx, op, http.MethodPost, "/api/v3/command", nil, payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusAccepted {
		return a.httpError(op, status, body)
	}
	return nil
}

// defaultQualityProfileID returns the lowest-numbered profile on the instance.
// Profile IDs are per-instance, so there is no correct constant to hardcode;
// picking deterministically from the service's own list at least fails loudly
// when an instance has no profiles at all.
func (a arr) defaultQualityProfileID(ctx context.Context, op string) (int, error) {
	var profiles []struct {
		ID int `json:"id"`
	}
	if err := a.getJSON(ctx, op, "/api/v3/qualityprofile", nil, &profiles); err != nil {
		return 0, err
	}
	best := 0
	for _, p := range profiles {
		if p.ID > 0 && (best == 0 || p.ID < best) {
			best = p.ID
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("%s: %s: no quality profiles configured, set AddOptions.QualityProfileID", a.app, op)
	}
	return best, nil
}

func (a arr) getJSON(ctx context.Context, op, path string, query url.Values, out any) error {
	status, body, err := a.do(ctx, op, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return a.httpError(op, status, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: %s: decoding %s response: %w", a.app, op, path, err)
	}
	return nil
}

// do issues one request. It returns a non-nil error only for failures below the
// HTTP response — an unreachable service, a timeout, a malformed URL — so
// callers can decide what a given status means.
func (a arr) do(ctx context.Context, op, method, path string, query url.Values, payload any) (int, []byte, error) {
	if a.baseURL == "" {
		return 0, nil, fmt.Errorf("%s: %s: url is not configured", a.app, op)
	}

	var reqBody io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: %s: encoding request: %w", a.app, op, err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	endpoint := a.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %s: building request: %w", a.app, op, err)
	}
	req.Header.Set("X-Api-Key", a.apiKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %s: %s unreachable at %s: %w", a.app, op, a.app, a.baseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %s: reading response: %w", a.app, op, err)
	}
	return resp.StatusCode, body, nil
}

func (a arr) httpError(op string, status int, body []byte) error {
	return &Error{App: a.app, Op: op, StatusCode: status, Body: truncate(body)}
}

// alreadyExists reports whether a rejected add was rejected because the service
// already tracks the item. Both services answer with a 400 (occasionally a 409)
// carrying a validation-failure array whose errorMessage says so.
func alreadyExists(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusConflict {
		return false
	}

	var failures []struct {
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.Unmarshal(body, &failures); err == nil {
		for _, f := range failures {
			if mentionsExisting(f.ErrorMessage) {
				return true
			}
		}
		return false
	}
	// Not the documented array shape; fall back to the raw body so a
	// differently-worded wrapper still gets recognised.
	return mentionsExisting(string(body))
}

func mentionsExisting(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "already been added") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "already added")
}

func truncate(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > maxErrorBody {
		return s[:maxErrorBody] + "..."
	}
	return s
}

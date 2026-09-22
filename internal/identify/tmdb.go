// Package identify turns a disc label or BDMV title into movie candidates.
//
// tmdb.go is the TMDB HTTP client: it performs the searches and returns the raw
// candidate data. Ranking those candidates against the disc label lives in
// fuzzy.go, so nothing here scores or sorts results beyond the order TMDB
// itself returns them in.
package identify

import (
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
)

// DefaultBaseURL is the TMDB v3 API root.
const DefaultBaseURL = "https://api.themoviedb.org/3"

// defaultTimeout bounds a single request when the caller does not supply its own
// http.Client.
const defaultTimeout = 10 * time.Second

// maxErrorBody caps how much of an error response body is read before giving up
// on parsing TMDB's status envelope.
const maxErrorBody = 64 << 10

// Errors callers can match with errors.Is to decide how to react to a failed
// call. ErrRateLimited is the retryable one; ErrUnauthorized means the
// configured credential is wrong and retrying will not help.
var (
	// ErrUnauthorized reports a 401 or 403 from TMDB — almost always a bad or
	// missing tmdb.api_key.
	ErrUnauthorized = errors.New("tmdb: unauthorized")
	// ErrRateLimited reports a 429 from TMDB. The call is worth retrying after
	// a delay; see APIError.RetryAfter for the delay TMDB asked for.
	ErrRateLimited = errors.New("tmdb: rate limited")
)

// APIError is a non-2xx response from TMDB. It carries both the HTTP status and
// TMDB's own status_code/status_message envelope when the body contained one.
type APIError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// TMDBCode is TMDB's application-level status_code, or 0 if absent.
	TMDBCode int
	// Message is TMDB's status_message, or empty if absent.
	Message string
	// RetryAfter is the Retry-After header parsed as a duration, or 0 if the
	// response did not set one.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.TMDBCode != 0 {
		return fmt.Sprintf("tmdb: http %d (status_code %d): %s", e.StatusCode, e.TMDBCode, msg)
	}
	return fmt.Sprintf("tmdb: http %d: %s", e.StatusCode, msg)
}

// Is lets errors.Is match the sentinels above, so callers can distinguish an
// auth failure from a transient rate limit without type-asserting.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	}
	return false
}

// Candidate is one movie TMDB returned for a search. Fields map directly to the
// TMDB response; AMA's own match score is added later by the ranking step.
type Candidate struct {
	// TMDBID is the movie's TMDB id, written to the manifest as tmdb_id.
	TMDBID int
	// Title is the localized title.
	Title string
	// OriginalTitle is the title in the original language, which often matches
	// a disc label better than the localized one.
	OriginalTitle string
	// Year is the year component of release_date, or 0 when TMDB has no usable
	// release date.
	Year int
	// Overview is TMDB's synopsis, shown in the confirmation UI.
	Overview string
	// PosterPath is the poster's path relative to TMDB's image base (for
	// example "/abc.jpg"), or empty when there is no poster.
	PosterPath string
	// Popularity is TMDB's popularity metric, used as a tiebreaker when two
	// candidates match the disc label equally well.
	Popularity float64
}

// Client searches TMDB. The zero value is not usable; construct one with New.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New returns a Client authenticating with apiKey, which is sent as a bearer
// token and so works for both v3 API keys and v4 read access tokens.
//
// A nil httpClient gets a client with a default per-request timeout. baseURL
// defaults to DefaultBaseURL; tests point it at an httptest server.
func New(apiKey string, httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &Client{baseURL: baseURL, apiKey: apiKey, http: httpClient}
}

// maxSearchPages bounds how many pages of /search/movie results Search will
// fetch. TMDB returns up to 20 results per page; a generic disc label can
// push the right movie past page 1, but fetching every page for a query with
// thousands of results would be wasteful, so the fetch is capped rather than
// exhaustive.
const maxSearchPages = 5

// searchResponse is the subset of /search/movie AMA reads.
type searchResponse struct {
	TotalPages int `json:"total_pages"`
	Results    []struct {
		ID            int     `json:"id"`
		Title         string  `json:"title"`
		OriginalTitle string  `json:"original_title"`
		ReleaseDate   string  `json:"release_date"`
		Overview      string  `json:"overview"`
		PosterPath    string  `json:"poster_path"`
		Popularity    float64 `json:"popularity"`
	} `json:"results"`
}

// Search queries TMDB for movies matching title. A non-zero year narrows the
// search; zero omits the filter entirely.
//
// TMDB's year filter excludes any movie whose release year does not match
// exactly, but the year disc metadata reports (a reissue date, a festival
// date, the packaging date) is frequently not the film's TMDB release year.
// So a year-filtered search that comes back empty is retried once without
// the year filter, rather than reporting a false "no such movie".
//
// Results are collected across up to maxSearchPages pages of TMDB's paginated
// response, so a match past the first 20 results can still be found.
//
// No match is not an error: the result is an empty slice.
func (c *Client) Search(ctx context.Context, title string, year int) ([]Candidate, error) {
	candidates, err := c.search(ctx, title, year)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 && year != 0 {
		return c.search(ctx, title, 0)
	}
	return candidates, nil
}

// search performs one TMDB search across up to maxSearchPages pages, without
// the year-filter fallback Search layers on top.
func (c *Client) search(ctx context.Context, title string, year int) ([]Candidate, error) {
	params := url.Values{}
	params.Set("query", title)
	if year != 0 {
		params.Set("year", strconv.Itoa(year))
	}

	var candidates []Candidate
	for page := 1; page <= maxSearchPages; page++ {
		params.Set("page", strconv.Itoa(page))

		var body searchResponse
		if err := c.get(ctx, "/search/movie", params, &body); err != nil {
			return nil, err
		}

		for _, r := range body.Results {
			candidates = append(candidates, Candidate{
				TMDBID:        r.ID,
				Title:         r.Title,
				OriginalTitle: r.OriginalTitle,
				Year:          releaseYear(r.ReleaseDate),
				Overview:      r.Overview,
				PosterPath:    r.PosterPath,
				Popularity:    r.Popularity,
			})
		}

		if body.TotalPages <= page {
			break
		}
	}
	return candidates, nil
}

// externalIDsResponse is the subset of /movie/{id}/external_ids AMA reads.
type externalIDsResponse struct {
	IMDBID string `json:"imdb_id"`
}

// IMDBID looks up the IMDb id for a confirmed TMDB movie, for the manifest's
// identification.imdb_id field. An empty string means TMDB knows the movie but
// has no IMDb id for it.
func (c *Client) IMDBID(ctx context.Context, tmdbID int) (string, error) {
	var body externalIDsResponse
	path := "/movie/" + strconv.Itoa(tmdbID) + "/external_ids"
	if err := c.get(ctx, path, nil, &body); err != nil {
		return "", err
	}
	return body.IMDBID, nil
}

// get performs an authenticated GET and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	endpoint := c.baseURL + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("tmdb: building request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// The URL is not wrapped in: it would put the query — and any future
		// credential-bearing parameter — into logs.
		return fmt.Errorf("tmdb: requesting %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("tmdb: decoding %s response: %w", path, err)
	}
	return nil
}

// apiError builds an APIError from a non-2xx response, best-effort parsing
// TMDB's status envelope out of the body.
func apiError(resp *http.Response) error {
	err := &APIError{
		StatusCode: resp.StatusCode,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if readErr == nil {
		var envelope struct {
			StatusCode    int    `json:"status_code"`
			StatusMessage string `json:"status_message"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			err.TMDBCode = envelope.StatusCode
			err.Message = envelope.StatusMessage
		}
	}
	return err
}

// retryAfter parses the Retry-After header, which arrives as either
// delay-seconds ("120") or an HTTP-date. TMDB itself only ever sends
// delay-seconds, but the response passing through this client is whatever is
// in front of TMDB — Cloudflare and other edges emit the HTTP-date form on
// their own 429/503 pages — so both forms are handled.
func retryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// releaseYear extracts the year from a TMDB release_date ("YYYY-MM-DD").
// Missing or malformed dates yield 0 rather than an error: TMDB regularly
// returns an empty release_date for unreleased or poorly catalogued titles.
func releaseYear(date string) int {
	if len(date) < 4 {
		return 0
	}
	year, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return year
}

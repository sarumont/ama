// Package musicbrainz is a minimal client for the MusicBrainz web service's
// disc ID lookup. It exists so internal/cd can resolve a CD's TOC-derived
// disc ID into candidate releases (artist, album, year, track list) itself,
// in Go, rather than scraping whipper's own "Matching releases:" stdout text
// -- see internal/cd's package doc for why that split was chosen (issue #9's
// spike).
//
// Modeled on internal/identify/tmdb.go: its own domain types, its own
// sentinel error for the one retryable failure mode, context-based
// cancellation, and nothing beyond the single endpoint AMA actually needs.
package musicbrainz

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

// DefaultBaseURL is the MusicBrainz web service v2 root.
const DefaultBaseURL = "https://musicbrainz.org/ws/2"

// defaultTimeout bounds a single request when the caller does not supply its
// own http.Client.
const defaultTimeout = 10 * time.Second

// maxErrorBody caps how much of an error response body is read before giving
// up on parsing MusicBrainz's error envelope.
const maxErrorBody = 64 << 10

// ErrRateLimited reports a 503 from MusicBrainz -- its response for exceeding
// the service's courtesy rate limit (documented as 1 request/second for
// unauthenticated clients). The call is worth retrying after a delay; see
// APIError.RetryAfter for any delay the service asked for.
var ErrRateLimited = errors.New("musicbrainz: rate limited")

// APIError is a non-2xx response from MusicBrainz. A 404 (no releases
// attached to the queried disc ID) is not represented as an APIError; see
// Client.ByDiscID.
type APIError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Message is MusicBrainz's own "error" field, or empty if the body did
	// not contain one.
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
	return fmt.Sprintf("musicbrainz: http %d: %s", e.StatusCode, msg)
}

// Is lets errors.Is match ErrRateLimited without a type assertion.
func (e *APIError) Is(target error) bool {
	return target == ErrRateLimited && e.StatusCode == http.StatusServiceUnavailable
}

// Track is one track of the medium (disc) that matched the queried disc ID.
type Track struct {
	// Number is the track's position on the disc, as reported by
	// MusicBrainz. 0 means MusicBrainz reported a non-numeric track number
	// (rare outside vinyl-style sides, which whipper does not rip).
	Number int
	Title  string
	// DurationSeconds is MusicBrainz's track length, rounded down from
	// milliseconds. 0 means MusicBrainz has no length for this track.
	DurationSeconds int
}

// Release is one MusicBrainz release matching a disc ID -- a candidate for
// web/confirm.html to present, and (once confirmed) what internal/cd.Rip
// pins the actual rip to. Field names mirror docs/MANIFEST.md's CD
// identification block directly (MBReleaseID -> mb_release_id, and so on).
type Release struct {
	MBReleaseID      string
	MBReleaseGroupID string
	Artist           string
	Album            string
	// Year is the release date's year component, or 0 when MusicBrainz has
	// no usable date for this release.
	Year int
	// Tracks is the track list of whichever medium on this release matched
	// the queried disc ID.
	Tracks []Track
}

// Client looks up releases by disc ID against the MusicBrainz web service.
// The zero value is not usable; construct one with New.
type Client struct {
	baseURL   string
	userAgent string
	http      *http.Client
}

// New returns a Client. MusicBrainz's API usage policy requires every client
// to send a descriptive User-Agent identifying the application, a version,
// and a contact URL (e.g. "ama/0.1.0 ( https://github.com/sarumont/ama )");
// requests missing one risk being rate-limited more aggressively. An empty
// userAgent is allowed (the header is simply omitted) but not recommended.
//
// A nil httpClient gets a client with a default per-request timeout. baseURL
// defaults to DefaultBaseURL; tests point it at an httptest server.
func New(userAgent string, httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &Client{baseURL: baseURL, userAgent: userAgent, http: httpClient}
}

// discIDResponse is the subset of GET /discid/{id} AMA reads.
type discIDResponse struct {
	Releases []releaseJSON `json:"releases"`
}

type releaseJSON struct {
	ID           string             `json:"id"`
	Title        string             `json:"title"`
	Date         string             `json:"date"`
	ReleaseGroup releaseGroupJSON   `json:"release-group"`
	ArtistCredit []artistCreditJSON `json:"artist-credit"`
	Media        []mediumJSON       `json:"media"`
}

type releaseGroupJSON struct {
	ID string `json:"id"`
}

type artistCreditJSON struct {
	Name string `json:"name"`
	// JoinPhrase is the text MusicBrainz inserts between this credit and the
	// next (" & ", " feat. ", ""...), already appropriate to append verbatim.
	JoinPhrase string `json:"joinphrase"`
}

type mediumJSON struct {
	Tracks []trackJSON `json:"tracks"`
}

type trackJSON struct {
	Number string `json:"number"`
	Title  string `json:"title"`
	// Length is the track duration in milliseconds.
	Length int `json:"length"`
}

// toRelease converts the wire shape into AMA's own domain type. Only the
// first medium's track list is used: querying by disc ID scopes the response
// to the medium that TOC actually matches, so a release response ever
// carries more than one medium is not expected in practice.
func (r releaseJSON) toRelease() Release {
	rel := Release{
		MBReleaseID:      r.ID,
		MBReleaseGroupID: r.ReleaseGroup.ID,
		Artist:           joinArtistCredit(r.ArtistCredit),
		Album:            r.Title,
		Year:             parseYear(r.Date),
	}
	if len(r.Media) > 0 {
		rel.Tracks = make([]Track, 0, len(r.Media[0].Tracks))
		for _, t := range r.Media[0].Tracks {
			number, _ := strconv.Atoi(t.Number) // best-effort; 0 for non-numeric
			rel.Tracks = append(rel.Tracks, Track{
				Number:          number,
				Title:           t.Title,
				DurationSeconds: t.Length / 1000,
			})
		}
	}
	return rel
}

// joinArtistCredit renders a release's artist-credit array as a single
// display string, honouring each entry's joinphrase rather than assuming a
// plain ", "-joined list.
func joinArtistCredit(credits []artistCreditJSON) string {
	var b strings.Builder
	for _, c := range credits {
		b.WriteString(c.Name)
		b.WriteString(c.JoinPhrase)
	}
	return b.String()
}

// ByDiscID looks up every release MusicBrainz has attached to discID -- the
// disc ID whipper's TOC read computed (see internal/cd.Client.Identify).
//
// No match is not an error: MusicBrainz answers 404 for a disc ID it has
// never seen (an obscure or small-label pressing, exactly the case
// docs/CLAUDE's CD flow needs to handle), and ByDiscID reports that as a nil,
// nil-error slice so callers can present "no candidates" the same way
// internal/identify does for an empty TMDB search.
func (c *Client) ByDiscID(ctx context.Context, discID string) ([]Release, error) {
	if discID == "" {
		return nil, errors.New("musicbrainz: disc id is required")
	}

	params := url.Values{}
	params.Set("inc", "recordings+artist-credits+release-groups")
	params.Set("fmt", "json")
	endpoint := c.baseURL + "/discid/" + url.PathEscape(discID) + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("musicbrainz: building request for disc %s: %w", discID, err)
	}
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("musicbrainz: requesting disc %s: %w", discID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(resp)
	}

	var body discIDResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("musicbrainz: decoding disc %s response: %w", discID, err)
	}

	releases := make([]Release, 0, len(body.Releases))
	for _, r := range body.Releases {
		releases = append(releases, r.toRelease())
	}
	return releases, nil
}

// apiError builds an APIError from a non-2xx response, best-effort parsing
// MusicBrainz's {"error": "..."} envelope out of the body.
func apiError(resp *http.Response) error {
	err := &APIError{
		StatusCode: resp.StatusCode,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if readErr == nil {
		var envelope struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			err.Message = envelope.Error
		}
	}
	return err
}

// retryAfter parses the Retry-After header, which arrives as either
// delay-seconds ("120") or an HTTP-date.
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

// parseYear extracts the year from a MusicBrainz release date, which is
// "YYYY", "YYYY-MM", or "YYYY-MM-DD" -- MusicBrainz allows partial dates.
// Missing or malformed dates yield 0 rather than an error.
func parseYear(date string) int {
	if len(date) < 4 {
		return 0
	}
	year, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return year
}

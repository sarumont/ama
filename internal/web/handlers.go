package web

// This file implements the three read-only views (queue, confirm, history)
// and the /api/status/:id HTMX polling fragment. None of these handlers ever
// mutate a manifest — see docs/CLAUDE.md, "Web UI is read-mostly": the only
// mutating actions (confirm identification, override a track's role, set a
// forced subtitle) are wired into confirm.html as hx-post targets but their
// handlers are issue #23's scope, not this one's.
//
// # Data access is a placeholder
//
// There is no daemon yet (#13 is future work), so there is no in-memory
// pipeline/manifest store for these handlers to read from. Until #13 lands,
// the filesystem is the only source of truth available here: loadManifests
// walks the configured output roots and reads every *.manifest.json it
// finds, on every request. That is the "current disc" heuristic in
// handleQueue too — the one manifest that is not yet complete or errored is
// treated as "active", since docs/CLAUDE.md guarantees at most one rip is
// ever in flight ("the drive is the queue"). #13's daemon wiring will very
// likely replace this whole file's data access with real in-memory state
// that the rip pipeline updates directly; when it does, re-walking and
// re-parsing every manifest on every request should go away in favor of
// that. This is deliberately not built out further than today's need.

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sarumont/ama/internal/manifest"
)

// historyLimit caps how many past rips history.html shows, newest first.
// There is no pagination UI for v1 — a large library still renders a usable
// page with a note that older rips are cut off, rather than an unbounded
// table.
const historyLimit = 50

// manifestEntry pairs a parsed manifest with the path it was read from, so
// history.html can link back to the manifest.json file on disk.
type manifestEntry struct {
	Manifest *manifest.Manifest
	Path     string
}

// loadManifests discovers every manifest.json under the configured output
// roots. A root that does not exist yet (nothing has been ripped there) is
// treated as empty rather than an error. A manifest that fails to parse is
// logged and skipped rather than failing the whole page — one corrupt file
// should not take down the queue/history views for every other rip.
func (s *Server) loadManifests() ([]manifestEntry, error) {
	var out []manifestEntry
	for _, root := range []string{s.cfg.Output.Movies, s.cfg.Output.Music} {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return fs.SkipDir
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".manifest.json") {
				return nil
			}
			m, rerr := manifest.Read(p)
			if rerr != nil {
				s.log.Warn("skipping unreadable manifest", "path", p, "err", rerr)
				return nil
			}
			out = append(out, manifestEntry{Manifest: m, Path: p})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", root, err)
		}
	}
	return out, nil
}

// findManifest returns the entry whose manifest has the given id, and
// whether one was found.
func findManifest(entries []manifestEntry, id string) (manifestEntry, bool) {
	for _, e := range entries {
		if e.Manifest.ID == id {
			return e, true
		}
	}
	return manifestEntry{}, false
}

// --- Queue ("/") ---------------------------------------------------------

// queueViewData is the "/" view model.
type queueViewData struct {
	CurrentPath string
	// Current is nil for the empty state: no disc in the drive.
	Current *statusView
}

// statusView is the template-ready view of the currently active manifest,
// shared between the inline render in queue.html and the standalone
// /api/status/:id fragment (handleStatus), which is why every field here is
// already display-safe (manifest.Manifest's pointer fields resolved, times
// and sizes formatted) rather than the raw manifest types.
type statusView struct {
	ID         string
	Status     string
	DiscLabel  string
	Device     string
	DiscType   string
	Title      string // BD title, or "Artist — Album" for CD; "" if not yet identified
	Year       int
	TrackCount int
	OCRDone    int
	OCRTotal   int
	Warnings   []string
	Errors     []string
}

func buildStatusView(m *manifest.Manifest) statusView {
	v := statusView{
		ID:         m.ID,
		Status:     string(m.Status),
		DiscLabel:  derefStringOr(m.Disc.Label, "Unknown"),
		Device:     m.Disc.Device,
		DiscType:   string(m.Disc.Type),
		Year:       m.Identification.Year,
		TrackCount: len(m.Tracks),
		Warnings:   m.Warnings,
		Errors:     m.Errors,
	}
	if m.Disc.Type == manifest.DiscTypeCD {
		v.Title = joinNonEmpty(" — ", m.Identification.Artist, m.Identification.Album)
	} else {
		v.Title = m.Identification.Title
	}
	for _, sub := range m.Subtitles {
		v.OCRTotal++
		if sub.Converted {
			v.OCRDone++
		}
	}
	return v
}

// handleQueue renders the "/" view: the one disc currently in the drive, if
// any. "Current" is whichever manifest is not yet complete or errored; per
// docs/CLAUDE.md there is never more than one ("no background queue"), but if
// data on disk ever violates that (e.g. a stale manifest left by a crash),
// the most recently detected one wins rather than an arbitrary one.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	entries, err := s.loadManifests()
	if err != nil {
		s.log.Error("loading manifests", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Error", "Could not read manifests.")
		return
	}

	var active *manifest.Manifest
	for _, e := range entries {
		m := e.Manifest
		if m.Status == manifest.StatusComplete || m.Status == manifest.StatusError {
			continue
		}
		if active == nil || m.DetectedAt.After(active.DetectedAt) {
			active = m
		}
	}

	data := queueViewData{CurrentPath: r.URL.Path}
	if active != nil {
		v := buildStatusView(active)
		data.Current = &v
	}
	s.render(w, http.StatusOK, "queue.html", data)
}

// handleStatus returns the HTMX progress fragment for one rip
// (GET /api/status/:id). It renders queue.html's "status-fragment" template
// directly rather than a full page — HTMX swaps this straight into the DOM.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := s.loadManifests()
	if err != nil {
		s.log.Error("loading manifests", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	entry, ok := findManifest(entries, id)
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<div id="status-panel">Unknown rip.</div>`))
		return
	}

	view := buildStatusView(entry.Manifest)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages["queue.html"].ExecuteTemplate(w, "status-fragment", view); err != nil {
		s.log.Error("rendering template", "template", "status-fragment", "err", err)
	}
}

// --- Confirm ("/confirm/:id") ---------------------------------------------

// confirmViewData is the "/confirm/:id" view model. Manifest is the full
// manifest for fields with no pointer types to worry about (Disc.Type,
// Disc.Device, Identification's non-pointer fields, Output, Candidates,
// Warnings/Errors); the remaining fields resolve the handful of pointer
// fields the template needs into display-safe strings, or wrap Tracks and
// Subtitles the same way (see trackRow/subtitleRow).
type confirmViewData struct {
	CurrentPath string
	Manifest    *manifest.Manifest
	DiscLabel   string
	Confidence  string // formatted percentage, "" if Identification.Confidence is unset
	ConfirmedAt string // formatted timestamp, "" if unset
	// Locked is true once the manifest is in a terminal state (complete or
	// error) — see trackRow.Locked/subtitleRow.Locked for why.
	Locked    bool
	Tracks    []trackRow
	Subtitles []subtitleRow
}

// trackRow is confirm.html's per-track view. manifest.Track carries pointer
// fields (MakeMKVIndex, SizeBytes, AccurateRip) so that a legitimate zero
// value isn't dropped by omitempty; those are awkward and unsafe to
// dereference from html/template directly (a nil *string or *int prints as
// its address, not its value, or its zero value), so they are resolved to
// display strings here instead.
type trackRow struct {
	manifest.Track
	IndexDisplay       string // MakeMKVIndex (BD); "—" if unset
	SizeDisplay        string // human-readable SizeBytes (BD); "—" if unset
	DurationDisplay    string // human-readable DurationSeconds
	AccurateRipDisplay string // "Yes"/"No"/"—" (CD)
	Flagged            bool   // Role == alternate_cut, needs user review
	// ManifestID and Locked travel on the row itself (rather than being
	// read from the page's outer scope in confirm.html) so the "track-row"
	// named template renders identically whether it's part of the full
	// page or rendered standalone as a handleTrackRole response — see
	// confirm.html.
	ManifestID string
	// Locked is true once the manifest is complete or error: #23's chosen
	// answer to "what happens to overrides after handoff to Radarr" is to
	// make them read-only from then on (see handlers.go's handleTrackRole
	// doc comment), and confirm.html renders the override form as disabled
	// text instead when this is set.
	Locked bool
}

// subtitleRow is confirm.html's per-subtitle view; see trackRow.
type subtitleRow struct {
	manifest.Subtitle
	SizeDisplay         string
	ForcedReasonDisplay string
	ManifestID          string
	Locked              bool
}

func trackRows(tracks []manifest.Track, manifestID string, locked bool) []trackRow {
	rows := make([]trackRow, len(tracks))
	for i, t := range tracks {
		row := trackRow{
			Track:           t,
			DurationDisplay: humanDuration(t.DurationSeconds),
			Flagged:         t.Role == manifest.RoleAlternateCut,
			IndexDisplay:    "—",
			SizeDisplay:     "—",
			ManifestID:      manifestID,
			Locked:          locked,
		}
		if t.MakeMKVIndex != nil {
			row.IndexDisplay = strconv.Itoa(*t.MakeMKVIndex)
		}
		if t.SizeBytes != nil {
			row.SizeDisplay = humanSize(*t.SizeBytes)
		}
		switch {
		case t.AccurateRip == nil:
			row.AccurateRipDisplay = "—"
		case *t.AccurateRip:
			row.AccurateRipDisplay = "Yes"
		default:
			row.AccurateRipDisplay = "No"
		}
		rows[i] = row
	}
	return rows
}

func subtitleRows(subs []manifest.Subtitle, manifestID string, locked bool) []subtitleRow {
	rows := make([]subtitleRow, len(subs))
	for i, sub := range subs {
		rows[i] = subtitleRow{
			Subtitle:            sub,
			SizeDisplay:         humanSize(sub.SizeBytes),
			ForcedReasonDisplay: derefStringOr(sub.ForcedCandidateReason, ""),
			ManifestID:          manifestID,
			Locked:              locked,
		}
	}
	return rows
}

// handleConfirm renders "/confirm/:id" for the manifest with that id, 404ing
// with a rendered error page (not a panic or a blank 500) when it is
// unknown.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := s.loadManifests()
	if err != nil {
		s.log.Error("loading manifests", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Error", "Could not read manifests.")
		return
	}

	entry, ok := findManifest(entries, id)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not Found", fmt.Sprintf("No manifest with id %q.", id))
		return
	}

	s.render(w, http.StatusOK, "confirm.html", s.buildConfirmViewData(r, entry))
}

// buildConfirmViewData builds "/confirm/:id"'s view model for entry. It is
// shared by handleConfirm (GET) and handleConfirmSubmit (POST, #23), which
// re-renders the same page as its HTMX response so the candidate-select and
// manual-search forms' hx-target="body" swap picks up every field a confirm
// can change, not just identification.
func (s *Server) buildConfirmViewData(r *http.Request, entry manifestEntry) confirmViewData {
	m := entry.Manifest
	locked := isLocked(m)
	return confirmViewData{
		CurrentPath: r.URL.Path,
		Manifest:    m,
		DiscLabel:   derefStringOr(m.Disc.Label, "Unknown"),
		Confidence:  formatConfidence(m.Identification.Confidence),
		ConfirmedAt: formatTime(m.Identification.ConfirmedAt),
		Locked:      locked,
		Tracks:      trackRows(m.Tracks, m.ID, locked),
		Subtitles:   subtitleRows(m.Subtitles, m.ID, locked),
	}
}

// isLocked reports whether m is in a terminal state (complete or error) —
// see handleTrackRole's doc comment for why #23 makes overrides read-only
// from that point on.
func isLocked(m *manifest.Manifest) bool {
	return m.Status == manifest.StatusComplete || m.Status == manifest.StatusError
}

// --- User actions (#23) ----------------------------------------------------
//
// docs/CLAUDE.md's "Web UI is read-mostly" names exactly three user
// actions, implemented by the three handlers below: confirm a disc's
// identification, override a track's role, and manually set a forced
// subtitle. All three write through manifest.Update (the atomic
// read-modify-write in writer.go, one lock per manifest path) and only ever
// set fields — never delete or clear one another step already populated —
// per the manifest's append-only convention.
//
// # Locking after handoff
//
// The ticket flagged an open question: what happens when a user tries to
// override a track's role or a subtitle's forced flag on a rip that is
// already status complete (handed to Radarr) or error? Three options were on
// the table: (a) lock all overrides from that point on — read-only; (b)
// allow the manifest edit but leave the on-disk *.mkv/*.processed.mkv files
// stale relative to it; (c) allow the edit and re-run the affected
// downstream steps (re-mux, re-trigger the Radarr scan) — which has no
// precedent anywhere in this codebase. This implements (a): isLocked's
// callers reject with 409 rather than silently producing a manifest that
// disagrees with what's on disk (b) or inventing an inverse/rerun pipeline
// this project has never needed before (c). confirm.html renders the
// override controls as disabled text once Locked is set. This is a default,
// not a settled design decision — see the PR description.
//
// Confirming identification itself (handleConfirmSubmit) is not subject to
// this lock: it can only ever apply once, since it is what moves a
// manifest off pending_confirmation in the first place, so by the time a
// manifest reaches complete/error it is already confirmed and
// handleConfirmSubmit's own idempotency check (below) makes a repeat POST a
// no-op regardless of status.

// errTrackNotFound and errSubtitleNotFound are returned by the mutators
// passed to manifest.Update in handleTrackRole/handleSubtitleForced to
// distinguish "no such index" from a generic write failure, so the HTTP
// handler can answer 404 instead of 500. errManifestLocked is the same for
// the isLocked rejection (409) — see "Locking after handoff" above. All
// three are checked with errors.Is because manifest.Update wraps whatever
// its callback returns with fmt.Errorf("...: %w", err).
var (
	errTrackNotFound    = errors.New("no track with that index")
	errSubtitleNotFound = errors.New("no subtitle stream with that index")
	errManifestLocked   = errors.New("manifest is complete or errored; overrides are locked")
)

// validRoles is the set handleTrackRole accepts, per docs/MANIFEST.md's
// "Role Values" and the AC — role_skip is a title-selection outcome (a
// track never ripped at all), not something a user assigns after the fact,
// so it is deliberately not in this set.
var validRoles = map[string]bool{
	string(manifest.RoleFeature):      true,
	string(manifest.RoleAlternateCut): true,
	string(manifest.RoleCommentary):   true,
	string(manifest.RoleExtra):        true,
}

// roleReasonUserSet and the forcedReasonUser* constants are the
// role_reason/forced_candidate_reason values handleTrackRole and
// handleSubtitleForced record, marking the field as user-set rather than
// produced by title selection's heuristics or the PGS size-ratio heuristic.
const (
	roleReasonUserSet   = "set by user"
	forcedReasonUserSet = "set by user"
	forcedReasonCleared = "cleared by user"
	confirmedByUser     = "user"
)

// handleConfirmSubmit handles "POST /confirm/:id" — the one hard gate in
// the pipeline. It accepts either a ranked candidate the user selected
// (tmdb_id for a Blu-ray, mb_release_id for a CD — the hidden fields
// confirm.html's candidate-select form posts) or a manually typed search
// result (the "query" field confirm.html's manual-search form posts), and
// on success writes identification.tmdb_id/mb_release_id, title (or
// artist/album for a CD), year, confirmed: true, confirmed_at and
// confirmed_by: "user" — unblocking the rip by moving status from
// pending_confirmation to ripping.
//
// # The manual-search "query" field
//
// There is no TMDB/MusicBrainz search wired into this package (that would
// be new scope — nothing here calls out to either API), so "query" is not
// a live search: it is taken as the identification directly, exactly as
// confirm.html's placeholder text ("Title, or artist / album") describes.
// For a Blu-ray, the whole string becomes Identification.Title. For a CD,
// splitArtistAlbumQuery splits it on "/" into artist and album, or — with
// no "/" — treats the whole string as the album. No tmdb_id/mb_release_id
// is set from a manual query, since none was looked up; only the candidate-
// select path populates those. This is a judgment call flagged in the PR
// description, not something docs/MANIFEST.md or the issue spells out.
//
// # Idempotency
//
// A manifest that is already confirmed makes this a no-op — the AC
// requires this so a duplicate submit (e.g. a slow request retried by the
// browser) can't restart or relabel an already-confirmed rip. The check is
// repeated inside the manifest.Update callback (not just before it) so a
// concurrent confirm can't race past it between the outer check and the
// write.
func (s *Server) handleConfirmSubmit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := s.loadManifests()
	if err != nil {
		s.log.Error("loading manifests", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	entry, ok := findManifest(entries, id)
	if !ok {
		http.Error(w, fmt.Sprintf("no manifest with id %q", id), http.StatusNotFound)
		return
	}
	m := entry.Manifest

	if m.Identification.Confirmed {
		s.render(w, http.StatusOK, "confirm.html", s.buildConfirmViewData(r, entry))
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form body", http.StatusBadRequest)
		return
	}

	var mutate func(*manifest.Manifest)
	if m.Disc.Type == manifest.DiscTypeCD {
		mutate, err = buildCDConfirmMutation(r, m)
	} else {
		mutate, err = buildBluRayConfirmMutation(r, m)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	err = manifest.Update(entry.Path, func(mm *manifest.Manifest) error {
		if mm.Identification.Confirmed {
			return nil // became confirmed concurrently; no-op
		}
		mutate(mm)
		mm.Identification.Confirmed = true
		now := time.Now().UTC()
		mm.Identification.ConfirmedAt = &now
		mm.Identification.ConfirmedBy = confirmedByUser
		if mm.Status == manifest.StatusPendingConfirmation {
			mm.Status = manifest.StatusRipping
		}
		return nil
	})
	if err != nil {
		s.log.Error("updating manifest", "path", entry.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	updated, err := manifest.Read(entry.Path)
	if err != nil {
		s.log.Error("reloading manifest", "path", entry.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "confirm.html", s.buildConfirmViewData(r, manifestEntry{Manifest: updated, Path: entry.Path}))
}

// buildBluRayConfirmMutation validates a Blu-ray confirm POST's form values
// against m's own candidate list and returns a mutator applying the result,
// or an error describing what was wrong with the request (for a 400). It
// does not itself touch m; the returned mutator is applied later, inside
// manifest.Update, to the freshly-read manifest.
func buildBluRayConfirmMutation(r *http.Request, m *manifest.Manifest) (func(*manifest.Manifest), error) {
	if tmdbStr := strings.TrimSpace(r.FormValue("tmdb_id")); tmdbStr != "" {
		tmdbID, err := strconv.Atoi(tmdbStr)
		if err != nil {
			return nil, fmt.Errorf("invalid tmdb_id %q", tmdbStr)
		}
		var chosen *manifest.Candidate
		for i := range m.Identification.Candidates {
			if m.Identification.Candidates[i].TMDBID == tmdbID {
				chosen = &m.Identification.Candidates[i]
				break
			}
		}
		if chosen == nil {
			return nil, fmt.Errorf("tmdb_id %d is not among this disc's candidates", tmdbID)
		}
		c := *chosen
		return func(mm *manifest.Manifest) {
			tmdbID := c.TMDBID
			mm.Identification.TMDBID = &tmdbID
			mm.Identification.Title = c.Title
			mm.Identification.Year = c.Year
		}, nil
	}
	if query := strings.TrimSpace(r.FormValue("query")); query != "" {
		return func(mm *manifest.Manifest) {
			mm.Identification.Title = query
		}, nil
	}
	return nil, errors.New("must select a candidate or enter a search query")
}

// buildCDConfirmMutation is buildBluRayConfirmMutation for a CD manifest —
// see its doc comment and handleConfirmSubmit's for the manual-query
// behavior.
func buildCDConfirmMutation(r *http.Request, m *manifest.Manifest) (func(*manifest.Manifest), error) {
	if mbID := strings.TrimSpace(r.FormValue("mb_release_id")); mbID != "" {
		var chosen *manifest.Candidate
		for i := range m.Identification.Candidates {
			if m.Identification.Candidates[i].MBReleaseID == mbID {
				chosen = &m.Identification.Candidates[i]
				break
			}
		}
		if chosen == nil {
			return nil, fmt.Errorf("mb_release_id %q is not among this disc's candidates", mbID)
		}
		c := *chosen
		return func(mm *manifest.Manifest) {
			mm.Identification.MBReleaseID = c.MBReleaseID
			mm.Identification.MBReleaseGroupID = c.MBReleaseGroupID
			mm.Identification.Artist = c.Artist
			mm.Identification.Album = c.Album
			mm.Identification.Year = c.Year
		}, nil
	}
	if query := strings.TrimSpace(r.FormValue("query")); query != "" {
		artist, album := splitArtistAlbumQuery(query)
		return func(mm *manifest.Manifest) {
			if artist != "" {
				mm.Identification.Artist = artist
			}
			mm.Identification.Album = album
		}, nil
	}
	return nil, errors.New("must select a candidate or enter a search query")
}

// splitArtistAlbumQuery splits a manual-search query on "/" into artist and
// album, matching confirm.html's placeholder text ("Title, or artist /
// album"). With no "/" the whole string is taken as the album alone —
// there's no reliable way to guess artist vs. album from one bare string,
// and album is what manifest.Dir/name key the CD's output path on.
func splitArtistAlbumQuery(query string) (artist, album string) {
	if i := strings.Index(query, "/"); i >= 0 {
		return strings.TrimSpace(query[:i]), strings.TrimSpace(query[i+1:])
	}
	return "", query
}

// handleTrackRole handles "POST /api/tracks/:id/:index/role" —
// confirm.html's per-track role-override form, which selects one of the
// four Role values a user can assign (RoleSkip is excluded; see
// validRoles) and posts it as the "role" field. :index is the track's
// makemkv_index (confirm.html's IndexDisplay), not its position in the
// Tracks slice. On success it writes Track.Role and marks Track.RoleReason
// as user-set, then returns just that row's "track-row" fragment — the
// hx-swap="outerHTML" response confirm.html's hx-target="closest tr" swaps
// in — rather than the whole page the way handleConfirmSubmit does; a
// role change only ever affects its own row.
//
// See "Locking after handoff" above for why this 409s once isLocked(m).
func (s *Server) handleTrackRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := s.loadManifests()
	if err != nil {
		s.log.Error("loading manifests", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	entry, ok := findManifest(entries, id)
	if !ok {
		http.Error(w, fmt.Sprintf("no manifest with id %q", id), http.StatusNotFound)
		return
	}
	m := entry.Manifest

	if m.Disc.Type != manifest.DiscTypeBluRay {
		http.Error(w, "role override only applies to Blu-ray tracks", http.StatusBadRequest)
		return
	}

	indexStr := r.PathValue("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid track index %q", indexStr), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form body", http.StatusBadRequest)
		return
	}
	role := r.FormValue("role")
	if !validRoles[role] {
		http.Error(w, fmt.Sprintf("invalid role %q: must be one of feature, alternate_cut, commentary, extra", role), http.StatusBadRequest)
		return
	}

	var result trackRow
	err = manifest.Update(entry.Path, func(mm *manifest.Manifest) error {
		if isLocked(mm) {
			return errManifestLocked
		}
		for i := range mm.Tracks {
			if mm.Tracks[i].MakeMKVIndex != nil && *mm.Tracks[i].MakeMKVIndex == index {
				mm.Tracks[i].Role = manifest.Role(role)
				mm.Tracks[i].RoleReason = roleReasonUserSet
				result = trackRows([]manifest.Track{mm.Tracks[i]}, mm.ID, false)[0]
				return nil
			}
		}
		return errTrackNotFound
	})
	switch {
	case errors.Is(err, errManifestLocked):
		http.Error(w, "cannot change role: this rip is already complete or errored", http.StatusConflict)
		return
	case errors.Is(err, errTrackNotFound):
		http.Error(w, fmt.Sprintf("no track with index %d", index), http.StatusNotFound)
		return
	case err != nil:
		s.log.Error("updating manifest", "path", entry.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages["confirm.html"].ExecuteTemplate(w, "track-row", result); err != nil {
		s.log.Error("rendering template", "template", "track-row", "err", err)
	}
}

// handleSubtitleForced handles "POST /api/subtitles/:id/:stream_index/forced"
// — confirm.html's per-subtitle forced-flag toggle, which posts "forced" as
// the literal string "true" or "false" (confirm.html always sends the
// opposite of the subtitle's current ForcedCandidate value, i.e. it's a
// toggle button, not a form the user fills in). :stream_index is
// Subtitle.StreamIndex. On success it sets ForcedCandidate and records a
// user-set ForcedCandidateReason (a distinct reason for setting vs.
// clearing, so it's clear from the manifest alone which happened), then
// returns just that row's "subtitle-row" fragment — see handleTrackRole's
// doc comment, which this mirrors.
//
// See "Locking after handoff" above for why this 409s once isLocked(m).
func (s *Server) handleSubtitleForced(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := s.loadManifests()
	if err != nil {
		s.log.Error("loading manifests", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	entry, ok := findManifest(entries, id)
	if !ok {
		http.Error(w, fmt.Sprintf("no manifest with id %q", id), http.StatusNotFound)
		return
	}
	m := entry.Manifest

	if m.Disc.Type != manifest.DiscTypeBluRay {
		http.Error(w, "forced-subtitle override only applies to Blu-ray subtitles", http.StatusBadRequest)
		return
	}

	streamIndexStr := r.PathValue("stream_index")
	streamIndex, err := strconv.Atoi(streamIndexStr)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid stream index %q", streamIndexStr), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form body", http.StatusBadRequest)
		return
	}
	var forced bool
	switch r.FormValue("forced") {
	case "true":
		forced = true
	case "false":
		forced = false
	default:
		http.Error(w, fmt.Sprintf("invalid forced value %q: must be true or false", r.FormValue("forced")), http.StatusBadRequest)
		return
	}

	var result subtitleRow
	err = manifest.Update(entry.Path, func(mm *manifest.Manifest) error {
		if isLocked(mm) {
			return errManifestLocked
		}
		for i := range mm.Subtitles {
			if mm.Subtitles[i].StreamIndex == streamIndex {
				reason := forcedReasonCleared
				if forced {
					reason = forcedReasonUserSet
				}
				mm.Subtitles[i].ForcedCandidate = forced
				mm.Subtitles[i].ForcedCandidateReason = &reason
				result = subtitleRows([]manifest.Subtitle{mm.Subtitles[i]}, mm.ID, false)[0]
				return nil
			}
		}
		return errSubtitleNotFound
	})
	switch {
	case errors.Is(err, errManifestLocked):
		http.Error(w, "cannot change forced subtitle: this rip is already complete or errored", http.StatusConflict)
		return
	case errors.Is(err, errSubtitleNotFound):
		http.Error(w, fmt.Sprintf("no subtitle stream with index %d", streamIndex), http.StatusNotFound)
		return
	case err != nil:
		s.log.Error("updating manifest", "path", entry.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages["confirm.html"].ExecuteTemplate(w, "subtitle-row", result); err != nil {
		s.log.Error("rendering template", "template", "subtitle-row", "err", err)
	}
}

// --- History ("/history") -------------------------------------------------

// historyViewData is the "/history" view model.
type historyViewData struct {
	CurrentPath string
	Rows        []historyRow
	Truncated   bool
	Limit       int
}

// historyRow is one row of the history table — a completed or errored
// manifest, with its pointer fields resolved the same way trackRow and
// subtitleRow do.
type historyRow struct {
	ID                    string
	DiscType              string
	Title                 string // "Title (Year)" for BD, "Artist — Album (Year)" for CD
	RippedAtDisplay       string
	Status                string
	Warnings              []string
	Errors                []string
	OutputPath            string
	MainFeature           string
	ProcessedFeature      string
	ManifestPath          string
	HasRadarr             bool
	RadarrAdded           bool
	RadarrImportTriggered bool
}

func historyRows(entries []manifestEntry) []historyRow {
	rows := make([]historyRow, len(entries))
	for i, e := range entries {
		m := e.Manifest
		row := historyRow{
			ID:               m.ID,
			DiscType:         string(m.Disc.Type),
			Status:           string(m.Status),
			RippedAtDisplay:  formatTimeValue(m.RippedAt),
			Warnings:         m.Warnings,
			Errors:           m.Errors,
			OutputPath:       m.Output.Path,
			MainFeature:      m.Output.MainFeature,
			ProcessedFeature: m.Output.ProcessedFeature,
			ManifestPath:     e.Path,
		}
		if m.Disc.Type == manifest.DiscTypeCD {
			row.Title = joinNonEmpty(" — ", m.Identification.Artist, m.Identification.Album)
		} else {
			row.Title = m.Identification.Title
			if m.Radarr != nil {
				row.HasRadarr = true
				row.RadarrAdded = m.Radarr.Added
				row.RadarrImportTriggered = m.Radarr.ImportTriggered
			}
		}
		if m.Identification.Year > 0 {
			row.Title = fmt.Sprintf("%s (%d)", row.Title, m.Identification.Year)
		}
		rows[i] = row
	}
	return rows
}

// historyTime is the timestamp history.html sorts by. RippedAt is usually
// what "newest first" means, but a rip that errored before ripping ever
// started (e.g. identification failed) leaves RippedAt zero, so DetectedAt
// is the fallback — an errored disc should still show up near the top of
// the list it belongs in, not sort to the very end as if it were ancient.
func historyTime(m *manifest.Manifest) time.Time {
	if !m.RippedAt.IsZero() {
		return m.RippedAt
	}
	return m.DetectedAt
}

// handleHistory renders "/history": every completed or errored manifest,
// newest first, capped at historyLimit.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	entries, err := s.loadManifests()
	if err != nil {
		s.log.Error("loading manifests", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Error", "Could not read manifests.")
		return
	}

	var finished []manifestEntry
	for _, e := range entries {
		switch e.Manifest.Status {
		case manifest.StatusComplete, manifest.StatusError:
			finished = append(finished, e)
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		return historyTime(finished[i].Manifest).After(historyTime(finished[j].Manifest))
	})

	truncated := len(finished) > historyLimit
	if truncated {
		finished = finished[:historyLimit]
	}

	data := historyViewData{
		CurrentPath: r.URL.Path,
		Rows:        historyRows(finished),
		Truncated:   truncated,
		Limit:       historyLimit,
	}
	s.render(w, http.StatusOK, "history.html", data)
}

// --- display formatting helpers -------------------------------------------

func derefStringOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func formatConfidence(p *float64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%.0f%%", *p*100)
}

func formatTime(p *time.Time) string {
	if p == nil || p.IsZero() {
		return ""
	}
	return p.Format(time.RFC3339)
}

func formatTimeValue(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format(time.RFC3339)
}

// humanSize renders b as a binary-prefixed size (KiB/MiB/...), matching the
// convention set by docs/MANIFEST.md's size_bytes fields.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// humanDuration renders a duration_seconds value the way a person reads it
// ("1h47m0s") rather than as raw seconds.
func humanDuration(seconds int) string {
	return (time.Duration(seconds) * time.Second).String()
}

// joinNonEmpty joins non-empty parts with sep, so "Artist — Album" degrades
// to just "Album" (or "") when identification is incomplete instead of
// showing a stray separator.
func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

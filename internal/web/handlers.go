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
	Tracks      []trackRow
	Subtitles   []subtitleRow
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
}

// subtitleRow is confirm.html's per-subtitle view; see trackRow.
type subtitleRow struct {
	manifest.Subtitle
	SizeDisplay         string
	ForcedReasonDisplay string
}

func trackRows(tracks []manifest.Track) []trackRow {
	rows := make([]trackRow, len(tracks))
	for i, t := range tracks {
		row := trackRow{
			Track:           t,
			DurationDisplay: humanDuration(t.DurationSeconds),
			Flagged:         t.Role == manifest.RoleAlternateCut,
			IndexDisplay:    "—",
			SizeDisplay:     "—",
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

func subtitleRows(subs []manifest.Subtitle) []subtitleRow {
	rows := make([]subtitleRow, len(subs))
	for i, sub := range subs {
		rows[i] = subtitleRow{
			Subtitle:            sub,
			SizeDisplay:         humanSize(sub.SizeBytes),
			ForcedReasonDisplay: derefStringOr(sub.ForcedCandidateReason, ""),
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

	m := entry.Manifest
	data := confirmViewData{
		CurrentPath: r.URL.Path,
		Manifest:    m,
		DiscLabel:   derefStringOr(m.Disc.Label, "Unknown"),
		Confidence:  formatConfidence(m.Identification.Confidence),
		ConfirmedAt: formatTime(m.Identification.ConfirmedAt),
		Tracks:      trackRows(m.Tracks),
		Subtitles:   subtitleRows(m.Subtitles),
	}
	s.render(w, http.StatusOK, "confirm.html", data)
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

// titles.go implements title selection: turning the raw list of MakeMKV titles
// into a role per title (which is the main feature, which are alternate cuts,
// which are commentary or extras, and which are playlist padding to skip).
//
// This is the most failure-prone part of the pipeline, so Classify is a pure
// function of its arguments: no config reads, no file system, no subprocesses.
// Everything it needs is passed in, which makes the rules exhaustively
// table-testable.
//
// The roles are bluray's own domain type deliberately, not manifest's. Mapping
// a Role onto the manifest's role string, and onto an output file name, belongs
// to the layer above.
package bluray

import (
	"sort"
	"strconv"
)

// Role is what a title turned out to be. The values are the ones the manifest
// serializes (see docs/MANIFEST.md "Role Values"), plus RoleSkip, which is
// internal: a skipped title is written to no file and gets no manifest entry.
type Role string

const (
	// RoleFeature is the main feature: the longest title on the disc.
	RoleFeature Role = "feature"
	// RoleAlternateCut is a title within 10% of the feature's duration. It is
	// most often the same film in a different cut, but it is also how a disc
	// with several near-identical playlists of the feature shows up, so these
	// need a human to confirm them.
	RoleAlternateCut Role = "alternate_cut"
	// RoleCommentary is a title with an audio stream flagged as commentary.
	RoleCommentary Role = "commentary"
	// RoleExtra is a title too short to be the feature but long enough to be
	// worth keeping: a featurette, deleted scene, trailer.
	RoleExtra Role = "extra"
	// RoleSkip is a title below the minimum duration: menu loops, transitions
	// and other playlist padding. Nothing is written for it.
	RoleSkip Role = "skip"
)

// ProducesOutput reports whether a title with this role is written to disk.
func (r Role) ProducesOutput() bool { return r != RoleSkip }

// Subdirectory is the folder, relative to the movie's own folder, that a title
// with this role is written into. An empty string means the movie folder
// itself. Commentary tracks live in extras/ alongside the extras, per
// docs/ARCHITECTURE.md.
func (r Role) Subdirectory() string {
	switch r {
	case RoleExtra, RoleCommentary:
		return "extras"
	default:
		return ""
	}
}

// Classification is one title plus the role it was assigned and why.
type Classification struct {
	// Track is the title as MakeMKV reported it.
	Track Track
	// Role is the assigned role.
	Role Role
	// Reason is a short human-readable explanation of the assignment, surfaced
	// in the web UI and stored as the manifest's role_reason.
	Reason string
	// Edition is the edition name for an alternate cut, used for Plex/Jellyfin
	// "{edition-...}" file naming. Empty for every other role.
	Edition string
	// NeedsReview marks a classification the user has to confirm before the rip
	// counts as complete. Only alternate cuts need it: everything else is
	// either unambiguous or already destined for manual sorting in extras/.
	NeedsReview bool
}

// alternateCutNumerator and alternateCutDenominator express the 90% alternate
// cut threshold as an exact ratio. The comparison is done in integers on
// purpose: float64(d) >= float64(feature)*0.90 misclassifies a title sitting
// exactly on the boundary, because 0.90 is not representable in binary.
const (
	alternateCutNumerator   = 9
	alternateCutDenominator = 10
)

// Classify assigns a Role to every title, applying the rules from
// docs/CLAUDE.md in this exact order. The order is normative:
//
//	sort by duration descending
//	feature = tracks[0]
//	for each remaining track:
//	  if duration >= feature.duration * 0.90 -> alternate_cut
//	  else if any audio track has the commentary flag -> commentary
//	  else if duration < minTrackDuration -> skip
//	  else -> extra
//
// Because the alternate cut check comes first, a commentary-flagged title
// within 10% of the feature is an alternate_cut, not a commentary. Because the
// commentary check comes before the minimum duration check, a
// commentary-flagged title shorter than minTrackDuration is a commentary, not
// a skip.
//
// minTrackDuration is makemkv.min_track_duration in seconds. Zero or negative
// means no minimum, so nothing is skipped for being short.
//
// The result is ordered by descending duration, i.e. the feature is always
// first. tracks is not modified. An empty (or nil) input yields an empty
// result and no feature is invented.
func Classify(tracks []Track, minTrackDuration int) []Classification {
	out := make([]Classification, 0, len(tracks))
	if len(tracks) == 0 {
		return out
	}

	sorted := make([]Track, len(tracks))
	copy(sorted, tracks)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].DurationSeconds != sorted[j].DurationSeconds {
			return sorted[i].DurationSeconds > sorted[j].DurationSeconds
		}
		// Tiebreak, undocumented in the spec and decided here: equal durations
		// order by ascending MakeMKV index, so the lowest-indexed of a set of
		// identical titles becomes the feature and the classification is
		// reproducible run to run. Discs routinely expose the feature as
		// several byte-identical playlists, and the lowest index is the one
		// MakeMKV lists first.
		return sorted[i].Index < sorted[j].Index
	})

	feature := sorted[0]
	out = append(out, Classification{
		Track:  feature,
		Role:   RoleFeature,
		Reason: "longest track",
	})

	alternateCuts := 0
	for _, track := range sorted[1:] {
		c := Classification{Track: track}
		switch {
		case isAlternateCut(track.DurationSeconds, feature.DurationSeconds):
			alternateCuts++
			c.Role = RoleAlternateCut
			c.Reason = "duration within 10% of feature"
			c.Edition = editionName(alternateCuts)
			c.NeedsReview = true
		case hasCommentaryAudio(track):
			c.Role = RoleCommentary
			c.Reason = "audio track flagged as commentary"
		case track.DurationSeconds < minTrackDuration:
			c.Role = RoleSkip
			c.Reason = "duration below minimum track duration"
		default:
			c.Role = RoleExtra
			c.Reason = "duration below feature threshold"
		}
		out = append(out, c)
	}
	return out
}

// isAlternateCut reports whether duration is within 10% of the feature's.
func isAlternateCut(duration, featureDuration int) bool {
	return duration*alternateCutDenominator >= featureDuration*alternateCutNumerator
}

// hasCommentaryAudio reports whether any audio stream of the title is flagged
// as commentary. Track.HasCommentaryAudio is makemkv.go's summary of the same
// thing, but the streams are the source of truth and a Track built by hand (in
// a test, or by a future caller) may carry only the streams.
func hasCommentaryAudio(t Track) bool {
	if t.HasCommentaryAudio {
		return true
	}
	for _, audio := range t.AudioTracks {
		if audio.HasCommentaryFlag {
			return true
		}
	}
	return false
}

// editionName names the nth alternate cut. The disc says nothing about what
// distinguishes the cuts, so they are numbered in duration order and the user
// renames them when confirming.
func editionName(n int) string {
	return "Alternate Cut " + strconv.Itoa(n)
}

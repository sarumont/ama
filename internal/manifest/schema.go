// Package manifest defines the JSON manifest AMA writes alongside every rip.
// The manifest is the authoritative record of a rip and the contract for
// downstream tooling (subtitle OCR, Radarr/Sonarr integration, extras sorting).
// The schema is documented in docs/MANIFEST.md; this package is the Go
// translation of it and nothing more — writing manifests to disk lives in
// writer.go.
//
// # Blu-ray and CD variants
//
// A manifest describes either a Blu-ray rip or a CD rip, and the two shapes
// differ: a CD has no subtitles, no Radarr/Sonarr blocks, identifies against
// MusicBrainz instead of TMDB, and its tracks carry a number and a title rather
// than a MakeMKV index and a role.
//
// Rather than two parallel manifest types (which would force every consumer —
// the writer, the web handlers, the *arr clients — to branch on a Go type),
// there is one Manifest with Disc.Type as the discriminator. Fields belonging
// to only one variant are tagged omitempty and, where they can legitimately
// hold a zero value, are pointers, so a CD manifest never carries empty Blu-ray
// sections and vice versa. Fields that docs/MANIFEST.md shows explicitly as
// null (Disc.Label, Track.Edition, Subtitle.ForcedCandidateReason,
// Subtitle.PairedWithStreamIndex, Subtitle.ConversionError, the *arr
// timestamps) are pointers without omitempty, so they marshal to null rather
// than disappearing.
//
// Track is the one place where those two rules collide: edition must be emitted
// as null on a Blu-ray track but must not appear at all on a CD track. Track
// therefore carries the union of both field sets and has a MarshalJSON that
// emits the documented shape for its variant. See Track.MarshalJSON.
package manifest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Version is stamped into every manifest as ama_version. It is a variable so a
// release build can override it with -ldflags.
var Version = "0.1.0"

// Status is the lifecycle state of a rip.
type Status string

// The statuses defined in docs/MANIFEST.md.
const (
	// StatusPendingConfirmation means AMA is waiting for the user to confirm
	// the disc identification.
	StatusPendingConfirmation Status = "pending_confirmation"
	// StatusRipping means MakeMKV or whipper is running.
	StatusRipping Status = "ripping"
	// StatusAnalyzing means subtitle analysis is running.
	StatusAnalyzing Status = "analyzing"
	// StatusPendingOCR means PGS to SRT conversion is in progress.
	StatusPendingOCR Status = "pending_ocr"
	// StatusComplete means every step finished successfully.
	StatusComplete Status = "complete"
	// StatusError means one or more steps failed; see Manifest.Errors.
	StatusError Status = "error"
)

// DiscType distinguishes the two manifest variants.
type DiscType string

// The supported disc types.
const (
	DiscTypeBluRay DiscType = "bluray"
	DiscTypeCD     DiscType = "cd"
)

// Role is how a ripped Blu-ray title was classified by title selection.
type Role string

// The roles defined in docs/MANIFEST.md, plus RoleSkip from the title selection
// pseudocode in docs/CLAUDE.md.
const (
	// RoleFeature is the main feature — the longest title.
	RoleFeature Role = "feature"
	// RoleAlternateCut is within 10% of the feature duration and is flagged
	// for user review.
	RoleAlternateCut Role = "alternate_cut"
	// RoleCommentary is a title whose audio was identified as commentary.
	RoleCommentary Role = "commentary"
	// RoleExtra is below the feature threshold and is placed in extras/.
	RoleExtra Role = "extra"
	// RoleSkip is below makemkv.min_track_duration and is not ripped.
	RoleSkip Role = "skip"
)

// Manifest is the complete record of one rip.
type Manifest struct {
	ID         string    `json:"id"`
	AMAVersion string    `json:"ama_version"`
	RippedAt   time.Time `json:"ripped_at"`
	Status     Status    `json:"status"`

	Disc           Disc           `json:"disc"`
	Identification Identification `json:"identification"`
	Tracks         []Track        `json:"tracks"`

	// Subtitles is Blu-ray only.
	Subtitles []Subtitle `json:"subtitles,omitempty"`

	Output Output `json:"output"`

	// Radarr and Sonarr are Blu-ray only. A Blu-ray rip populates whichever
	// one matches the content type and leaves the other in its zero state;
	// a CD rip omits both.
	Radarr *Arr `json:"radarr,omitempty"`
	Sonarr *Arr `json:"sonarr,omitempty"`

	Warnings []string `json:"warnings"`
	Errors   []string `json:"errors"`
}

// New returns a manifest for a rip that is about to start: a fresh UUID, the
// current time, the package version, and status pending_confirmation.
func New(discType DiscType, device string) *Manifest {
	return &Manifest{
		ID:         newUUID(),
		AMAVersion: Version,
		RippedAt:   time.Now().UTC(),
		Status:     StatusPendingConfirmation,
		Disc: Disc{
			Type:   discType,
			Device: device,
		},
		Warnings: []string{},
		Errors:   []string{},
	}
}

// MarshalJSON emits warnings and errors as [] rather than null when they are
// unset, so consumers can index them without a nil check.
func (m Manifest) MarshalJSON() ([]byte, error) {
	type alias Manifest
	out := alias(m)
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	if out.Errors == nil {
		out.Errors = []string{}
	}
	return json.Marshal(out)
}

// Disc describes the physical disc.
type Disc struct {
	Type DiscType `json:"type"`
	// Label is the volume label. CDs have none, so it marshals to null.
	Label  *string `json:"label"`
	Device string  `json:"device"`
}

// Identification records how the disc was identified and against what.
// The tmdb_id/imdb_id/title/confidence/candidates fields are Blu-ray only; the
// mb_*/artist/album fields are CD only.
type Identification struct {
	// Method is the identification path, e.g. "bdmv+tmdb" or "musicbrainz".
	Method string `json:"method"`

	// Blu-ray identification.
	TMDBID     *int        `json:"tmdb_id,omitempty"`
	IMDbID     string      `json:"imdb_id,omitempty"`
	Title      string      `json:"title,omitempty"`
	Confidence *float64    `json:"confidence,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`

	// CD identification.
	MBReleaseID      string `json:"mb_release_id,omitempty"`
	MBReleaseGroupID string `json:"mb_release_group_id,omitempty"`
	Artist           string `json:"artist,omitempty"`
	Album            string `json:"album,omitempty"`

	// Year is the release year of the movie or album, omitted while unknown.
	Year int `json:"year,omitempty"`

	Confirmed   bool       `json:"confirmed"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	// ConfirmedBy is "user" or "auto"; absent on CD manifests.
	ConfirmedBy string `json:"confirmed_by,omitempty"`
}

// Candidate is one ranked TMDB match for a disc.
type Candidate struct {
	TMDBID int     `json:"tmdb_id"`
	Title  string  `json:"title"`
	Year   int     `json:"year"`
	Score  float64 `json:"score"`
}

// Track is one ripped Blu-ray title or one ripped CD audio track. It holds the
// union of both field sets; see MarshalJSON for how the variant is chosen.
type Track struct {
	// Blu-ray fields. MakeMKVIndex, SizeBytes, AudioTrackCount and
	// ChapterCount are pointers because zero is a meaningful value for them
	// (the feature is routinely index 0) and omitempty would drop it.
	MakeMKVIndex    *int    `json:"makemkv_index,omitempty"`
	SizeBytes       *int64  `json:"size_bytes,omitempty"`
	AudioTrackCount *int    `json:"audio_track_count,omitempty"`
	ChapterCount    *int    `json:"chapter_count,omitempty"`
	Role            Role    `json:"role,omitempty"`
	RoleReason      string  `json:"role_reason,omitempty"`
	Edition         *string `json:"edition,omitempty"`

	// CD fields.
	Number      *int   `json:"number,omitempty"`
	Title       string `json:"title,omitempty"`
	AccurateRip *bool  `json:"accurate_rip,omitempty"`

	// Shared fields.
	DurationSeconds int    `json:"duration_seconds"`
	OutputFile      string `json:"output_file"`
}

// MarshalJSON writes the Blu-ray track shape or the CD track shape depending on
// the variant: a track with Number set is a CD track, anything else is a
// Blu-ray title. Emitting the exact documented shape is why this exists —
// omitempty alone cannot both keep "edition": null on a Blu-ray track and drop
// the key entirely from a CD track.
func (t Track) MarshalJSON() ([]byte, error) {
	if t.Number != nil {
		return json.Marshal(cdTrack{
			Number:          *t.Number,
			Title:           t.Title,
			DurationSeconds: t.DurationSeconds,
			AccurateRip:     t.AccurateRip,
			OutputFile:      t.OutputFile,
		})
	}
	return json.Marshal(bdTrack{
		MakeMKVIndex:    t.MakeMKVIndex,
		DurationSeconds: t.DurationSeconds,
		SizeBytes:       t.SizeBytes,
		AudioTrackCount: t.AudioTrackCount,
		ChapterCount:    t.ChapterCount,
		Role:            t.Role,
		RoleReason:      t.RoleReason,
		OutputFile:      t.OutputFile,
		Edition:         t.Edition,
	})
}

// bdTrack is the marshaling shape of a Blu-ray title.
type bdTrack struct {
	MakeMKVIndex    *int    `json:"makemkv_index"`
	DurationSeconds int     `json:"duration_seconds"`
	SizeBytes       *int64  `json:"size_bytes"`
	AudioTrackCount *int    `json:"audio_track_count"`
	ChapterCount    *int    `json:"chapter_count"`
	Role            Role    `json:"role"`
	RoleReason      string  `json:"role_reason"`
	OutputFile      string  `json:"output_file"`
	Edition         *string `json:"edition"`
}

// cdTrack is the marshaling shape of a CD audio track.
type cdTrack struct {
	Number          int    `json:"number"`
	Title           string `json:"title"`
	DurationSeconds int    `json:"duration_seconds"`
	AccurateRip     *bool  `json:"accurate_rip"`
	OutputFile      string `json:"output_file"`
}

// Subtitle is one subtitle stream found in a ripped Blu-ray title.
type Subtitle struct {
	StreamIndex         int    `json:"stream_index"`
	Codec               string `json:"codec"`
	Language            string `json:"language"`
	SizeBytes           int64  `json:"size_bytes"`
	ForcedFlagInSource  bool   `json:"forced_flag_in_source"`
	DefaultFlagInSource bool   `json:"default_flag_in_source"`
	HearingImpaired     bool   `json:"hearing_impaired"`
	// ForcedCandidate is set by the PGS size-ratio heuristic.
	ForcedCandidate bool `json:"forced_candidate"`
	// ForcedCandidateReason explains the heuristic's decision; null when the
	// stream was not flagged.
	ForcedCandidateReason *string `json:"forced_candidate_reason"`
	// PairedWithStreamIndex is the stream this one was compared against;
	// null when it had no pair.
	PairedWithStreamIndex *int `json:"paired_with_stream_index"`
	NeedsOCR              bool `json:"needs_ocr"`
	Converted             bool `json:"converted"`
	// ConversionError is null unless OCR failed for this stream.
	ConversionError *string `json:"conversion_error"`
}

// Output records where the rip landed. MainFeature and ProcessedFeature are
// Blu-ray only; a CD manifest carries just the album directory.
type Output struct {
	Path             string `json:"path"`
	MainFeature      string `json:"main_feature,omitempty"`
	ProcessedFeature string `json:"processed_feature,omitempty"`
}

// Arr records what AMA did with a Radarr or Sonarr instance. Both services have
// the same shape, so the radarr and sonarr blocks share this type.
type Arr struct {
	Added             bool       `json:"added"`
	AddedAt           *time.Time `json:"added_at"`
	ImportTriggered   bool       `json:"import_triggered"`
	ImportTriggeredAt *time.Time `json:"import_triggered_at"`
}

// newUUID returns a random (version 4) UUID. Generating it here keeps the
// package dependency-free; it is the only UUID this project needs.
func newUUID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error as of Go 1.24.
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

package cd

import (
	"errors"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// TrackStatus is whipper's per-track "Status" field in the YAML rip log.
type TrackStatus string

// The track statuses documented in the issue #9 spike.
const (
	TrackStatusCopyOK      TrackStatus = "Copy OK"
	TrackStatusCRCMismatch TrackStatus = "Error, CRC mismatch"
	TrackStatusSkipped     TrackStatus = "Track not ripped (skipped)"
)

// AccurateRipSummary is whipper's album-level "AccurateRip summary" line.
type AccurateRipSummary string

// The four AccurateRip summary values whipper/result/logger.go emits (see
// the issue #9 spike, section 4). AccurateRipSomeUnverifiedPrefix is not a
// complete value: state 2's summary carries a dynamic "N/M got no match"
// suffix whipper fills in per rip, so match it with strings.HasPrefix rather
// than equality.
const (
	// AccurateRipAllAccurate: every track matched AccurateRip (state 1).
	AccurateRipAllAccurate AccurateRipSummary = "All tracks accurately ripped"
	// AccurateRipNoneVerifiable: the disc IS in the AccurateRip database but
	// no track's checksum matched -- state 3, possibly a different pressing
	// than the one(s) on record.
	AccurateRipNoneVerifiable AccurateRipSummary = "No tracks could be verified as accurate (you may have a different pressing from the one(s) in the database)"
	// AccurateRipNotInDatabase: the disc has no AccurateRip entry at all --
	// state 4. Not a failure, just unverifiable; see the package doc.
	AccurateRipNotInDatabase AccurateRipSummary = "None of the tracks are present in the AccurateRip database"
	// AccurateRipSomeUnverifiedPrefix: state 2, a partial match.
	AccurateRipSomeUnverifiedPrefix = "Some tracks could not be verified as accurate"
)

// HealthStatus is whipper's album-level "Health status" line.
type HealthStatus string

// The three Health status values the issue #9 spike documents.
const (
	HealthOK      HealthStatus = "No errors occurred"
	HealthSkipped HealthStatus = "Some tracks were not ripped (skipped)"
	HealthErrors  HealthStatus = "There were errors"
)

// ARResult is one AccurateRip algorithm version's (v1 or v2) verification
// result for a track, straight from the YAML log's "AccurateRip v1"/
// "AccurateRip v2" keys. It is nil on RippedTrack for a track whipper
// skipped entirely.
type ARResult struct {
	Result     string `yaml:"Result"`
	Confidence int    `yaml:"Confidence"`
	LocalCRC   string `yaml:"Local CRC"`
	RemoteCRC  string `yaml:"Remote CRC"`
}

// RippedTrack is one track's entry in whipper's YAML rip log.
type RippedTrack struct {
	// Number is the track number -- the YAML log's integer key.
	Number int
	// Filename is the FLAC file whipper wrote, as an absolute path. Empty
	// for a skipped track.
	Filename  string
	PeakLevel float64
	// ExtractionQuality is whipper's own string, e.g. "100.00 %"; kept as
	// reported rather than parsed to a float, since the manifest schema has
	// no numeric field for it and a raw string is one less thing that can be
	// misparsed.
	ExtractionQuality string
	TestCRC           string
	CopyCRC           string
	// AccurateRipV1 and AccurateRipV2 are nil for a track whipper skipped.
	AccurateRipV1 *ARResult
	AccurateRipV2 *ARResult
	Status        TrackStatus
}

// Verified reports whether this track's checksum matched AccurateRip's
// database, under either algorithm version -- the input to the manifest's
// per-track accurate_rip bool. False does not by itself mean the rip
// failed: the disc may simply be absent from the database entirely, which
// RipResult.AccurateRipSummary distinguishes from a genuine mismatch.
func (t RippedTrack) Verified() bool {
	for _, ar := range []*ARResult{t.AccurateRipV1, t.AccurateRipV2} {
		if ar != nil && strings.HasPrefix(ar.Result, "Found, exact match") {
			return true
		}
	}
	return false
}

// RipResult is Rip's outcome, parsed from whipper's YAML rip log. It reports
// what whipper did; deciding what the manifest or the web UI does with an
// incomplete AccurateRip verification is out of scope for this package (see
// the package doc and issue #9's spike, section 4).
type RipResult struct {
	// Tracks is in ascending track-number order.
	Tracks             []RippedTrack
	AccurateRipSummary AccurateRipSummary
	HealthStatus       HealthStatus
}

// whipperLog is the YAML shape of whipper's rip log, a ruamel.yaml
// round-trip dump (see the issue #9 spike for the full example this is
// modeled on). Keys and nesting are whipper's own, not AMA's; extra
// top-level keys the real log carries -- notably a trailing "SHA-256 hash"
// line -- are simply not declared here and are ignored by yaml.Unmarshal.
type whipperLog struct {
	Tracks map[int]struct {
		Filename          string    `yaml:"Filename"`
		PeakLevel         float64   `yaml:"Peak level"`
		ExtractionQuality string    `yaml:"Extraction quality"`
		TestCRC           string    `yaml:"Test CRC"`
		CopyCRC           string    `yaml:"Copy CRC"`
		AccurateRipV1     *ARResult `yaml:"AccurateRip v1"`
		AccurateRipV2     *ARResult `yaml:"AccurateRip v2"`
		Status            string    `yaml:"Status"`
	} `yaml:"Tracks"`
	ConclusiveStatusReport struct {
		AccurateRipSummary string `yaml:"AccurateRip summary"`
		HealthStatus       string `yaml:"Health status"`
	} `yaml:"Conclusive status report"`
}

// parseLog unmarshals whipper's YAML rip log into a RipResult.
func parseLog(data []byte) (*RipResult, error) {
	var raw whipperLog
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if len(raw.Tracks) == 0 {
		return nil, errors.New("no Tracks section")
	}

	numbers := make([]int, 0, len(raw.Tracks))
	for n := range raw.Tracks {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)

	result := &RipResult{
		Tracks:             make([]RippedTrack, 0, len(numbers)),
		AccurateRipSummary: AccurateRipSummary(raw.ConclusiveStatusReport.AccurateRipSummary),
		HealthStatus:       HealthStatus(raw.ConclusiveStatusReport.HealthStatus),
	}
	for _, n := range numbers {
		t := raw.Tracks[n]
		result.Tracks = append(result.Tracks, RippedTrack{
			Number:            n,
			Filename:          t.Filename,
			PeakLevel:         t.PeakLevel,
			ExtractionQuality: t.ExtractionQuality,
			TestCRC:           t.TestCRC,
			CopyCRC:           t.CopyCRC,
			AccurateRipV1:     t.AccurateRipV1,
			AccurateRipV2:     t.AccurateRipV2,
			Status:            TrackStatus(t.Status),
		})
	}
	return result, nil
}

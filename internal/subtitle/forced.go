package subtitle

import "fmt"

// DefaultForcedRatioThreshold matches config's subtitle.forced_ratio_threshold
// default and is used when DetectForcedCandidates is given a non-positive
// threshold.
const DefaultForcedRatioThreshold = 0.25

// LanguageEnglish is the only language forced detection auto-flags. Discs in
// other languages are reported by Analyze but never annotated here.
const LanguageEnglish = "eng"

// DetectForcedCandidates infers which English PGS stream is the forced-subtitle
// track from the size ratio between tracks, since Blu-rays rarely set the
// forced disposition themselves. It annotates streams in place and returns the
// same slice so it can be chained between Analyze and WriteSidecar:
//
//	streams, err := subtitle.Analyze(ctx, path)
//	streams = subtitle.DetectForcedCandidates(streams, cfg.Subtitle.ForcedRatioThreshold)
//	err = subtitle.WriteSidecar(path, streams)
//
// The three fields it owns (ForcedCandidate, ForcedCandidateReason,
// PairedWithStreamIndex) are cleared first, so calling it twice — or calling it
// on an already-annotated sidecar — yields the same result as calling it once.
//
// A ratioThreshold of zero or less falls back to DefaultForcedRatioThreshold.
//
// Pairing rules:
//
//   - Only streams with codec CodecPGS and language "eng" take part. Everything
//     else is left untouched: reported, never auto-flagged.
//
//   - Fewer than two eng PGS streams means there is no pair to compare, so
//     nothing is flagged.
//
//   - The largest eng PGS stream is taken as the full track and every other eng
//     PGS stream is measured against it (ties broken by the lower stream index).
//     The docs describe the common two-track disc, where this reduces to exactly
//     the documented smaller-vs-larger comparison; with three or more tracks the
//     largest is still the only sensible reference, because a forced track is
//     small relative to the *full* track, not relative to its neighbours.
//
//   - Of the streams whose ratio is below the threshold, only the single
//     smallest ratio is flagged (ties broken by the lower stream index). A file
//     has at most one forced track, so at most one candidate is produced.
//
//   - Both streams of the matched pair get PairedWithStreamIndex pointing at
//     each other, matching the docs/MANIFEST.md example.
//
//   - A stream whose size is 0 — the value Analyze records when it could not
//     determine a size — is never flagged and is never used as the reference,
//     which also keeps the ratio from dividing by zero.
//
//   - If some *other* eng PGS stream already carries ForcedFlagInSource, the
//     source has already named its forced track and nothing is flagged, so the
//     heuristic cannot contradict it. A source flag on the stream the heuristic
//     picked agrees with it, and is flagged normally.
func DetectForcedCandidates(streams []SubtitleStream, ratioThreshold float64) []SubtitleStream {
	if ratioThreshold <= 0 {
		ratioThreshold = DefaultForcedRatioThreshold
	}

	var eng []int
	for i := range streams {
		streams[i].ForcedCandidate = false
		streams[i].ForcedCandidateReason = nil
		streams[i].PairedWithStreamIndex = nil

		if streams[i].Codec == CodecPGS && streams[i].Language == LanguageEnglish {
			eng = append(eng, i)
		}
	}
	if len(eng) < 2 {
		return streams
	}

	// The full track: the largest stream of known size.
	full := -1
	for _, i := range eng {
		if streams[i].SizeBytes <= 0 {
			continue
		}
		if full == -1 || better(streams[i], streams[full]) {
			full = i
		}
	}
	if full == -1 {
		return streams
	}

	// The forced track: the smallest ratio against the full track, if any is
	// below the threshold.
	forced, forcedRatio := -1, 0.0
	for _, i := range eng {
		if i == full || streams[i].SizeBytes <= 0 {
			continue
		}
		ratio := float64(streams[i].SizeBytes) / float64(streams[full].SizeBytes)
		if ratio >= ratioThreshold {
			continue
		}
		if forced == -1 || ratio < forcedRatio ||
			(ratio == forcedRatio && streams[i].StreamIndex < streams[forced].StreamIndex) {
			forced, forcedRatio = i, ratio
		}
	}
	if forced == -1 {
		return streams
	}

	// Defer to the source when it flags a different track as forced.
	for _, i := range eng {
		if i != forced && streams[i].ForcedFlagInSource {
			return streams
		}
	}

	// Copied out of the slice so the stored pointers do not alias the live
	// StreamIndex fields.
	fullIndex, forcedIndex := streams[full].StreamIndex, streams[forced].StreamIndex
	reason := fmt.Sprintf("size ratio %.2f vs stream %d (%d vs %d bytes)",
		forcedRatio, fullIndex, streams[forced].SizeBytes, streams[full].SizeBytes)

	streams[forced].ForcedCandidate = true
	streams[forced].ForcedCandidateReason = &reason
	streams[forced].PairedWithStreamIndex = &fullIndex
	streams[full].PairedWithStreamIndex = &forcedIndex

	return streams
}

// better reports whether a is a better full-track reference than b: the larger
// stream, or the lower stream index when the sizes tie.
func better(a, b SubtitleStream) bool {
	if a.SizeBytes != b.SizeBytes {
		return a.SizeBytes > b.SizeBytes
	}
	return a.StreamIndex < b.StreamIndex
}

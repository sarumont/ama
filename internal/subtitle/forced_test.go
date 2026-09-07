package subtitle

import (
	"encoding/json"
	"testing"
)

// pgs builds an English PGS stream, the only kind forced detection acts on.
func pgs(index int, size int64) SubtitleStream {
	return SubtitleStream{
		StreamIndex: index,
		Codec:       CodecPGS,
		Language:    LanguageEnglish,
		SizeBytes:   size,
		NeedsOCR:    true,
	}
}

// want describes the expected annotation of one stream, keyed by stream index.
type want struct {
	candidate bool
	reason    string // "" means the reason must be nil
	pairedIdx *int   // nil means PairedWithStreamIndex must be nil
}

func idx(i int) *int { return &i }

func TestDetectForcedCandidates(t *testing.T) {
	tests := []struct {
		name      string
		streams   []SubtitleStream
		threshold float64
		want      map[int]want
	}{
		{
			// The documented two-track disc: a small forced track alongside
			// the full one.
			name: "eng pgs pair below threshold, smaller first",
			streams: []SubtitleStream{
				pgs(7, 2_100_000),
				pgs(12, 33_000_000),
			},
			threshold: 0.25,
			want: map[int]want{
				7:  {candidate: true, reason: "size ratio 0.06 vs stream 12 (2100000 vs 33000000 bytes)", pairedIdx: idx(12)},
				12: {pairedIdx: idx(7)},
			},
		},
		{
			// Input order must not matter: the same pair listed largest first
			// produces the identical annotation.
			name: "eng pgs pair below threshold, larger first",
			streams: []SubtitleStream{
				pgs(12, 33_000_000),
				pgs(7, 2_100_000),
			},
			threshold: 0.25,
			want: map[int]want{
				7:  {candidate: true, reason: "size ratio 0.06 vs stream 12 (2100000 vs 33000000 bytes)", pairedIdx: idx(12)},
				12: {pairedIdx: idx(7)},
			},
		},
		{
			name: "ratio just below threshold is flagged",
			streams: []SubtitleStream{
				pgs(3, 2_499_999),
				pgs(4, 10_000_000),
			},
			threshold: 0.25,
			want: map[int]want{
				3: {candidate: true, reason: "size ratio 0.25 vs stream 4 (2499999 vs 10000000 bytes)", pairedIdx: idx(4)},
				4: {pairedIdx: idx(3)},
			},
		},
		{
			// The comparison is strict: a ratio exactly at the threshold is
			// two full tracks of similar size, not a forced track.
			name: "ratio exactly at threshold is not flagged",
			streams: []SubtitleStream{
				pgs(3, 2_500_000),
				pgs(4, 10_000_000),
			},
			threshold: 0.25,
			want:      map[int]want{3: {}, 4: {}},
		},
		{
			name: "ratio above threshold is not flagged",
			streams: []SubtitleStream{
				pgs(3, 8_000_000),
				pgs(4, 10_000_000),
			},
			threshold: 0.25,
			want:      map[int]want{3: {}, 4: {}},
		},
		{
			// A non-default configured threshold moves the line: this ratio of
			// 0.40 is above the default but below the configured 0.5.
			name: "non-default threshold flags a ratio the default would not",
			streams: []SubtitleStream{
				pgs(3, 4_000_000),
				pgs(4, 10_000_000),
			},
			threshold: 0.5,
			want: map[int]want{
				3: {candidate: true, reason: "size ratio 0.40 vs stream 4 (4000000 vs 10000000 bytes)", pairedIdx: idx(4)},
				4: {pairedIdx: idx(3)},
			},
		},
		{
			name: "non-positive threshold falls back to the default",
			streams: []SubtitleStream{
				pgs(3, 1_000_000),
				pgs(4, 10_000_000),
			},
			threshold: 0,
			want: map[int]want{
				3: {candidate: true, reason: "size ratio 0.10 vs stream 4 (1000000 vs 10000000 bytes)", pairedIdx: idx(4)},
				4: {pairedIdx: idx(3)},
			},
		},
		{
			// Other languages are reported by Analyze but never auto-flagged,
			// even when their ratio would qualify.
			name: "non-english pgs pair is never flagged",
			streams: []SubtitleStream{
				{StreamIndex: 5, Codec: CodecPGS, Language: "fra", SizeBytes: 2_100_000, NeedsOCR: true},
				{StreamIndex: 6, Codec: CodecPGS, Language: "fra", SizeBytes: 33_000_000, NeedsOCR: true},
			},
			threshold: 0.25,
			want:      map[int]want{5: {}, 6: {}},
		},
		{
			name: "mixed languages: only the eng pair is considered",
			streams: []SubtitleStream{
				{StreamIndex: 2, Codec: CodecPGS, Language: "spa", SizeBytes: 1_000_000, NeedsOCR: true},
				pgs(7, 2_100_000),
				{StreamIndex: 9, Codec: CodecPGS, Language: "spa", SizeBytes: 30_000_000, NeedsOCR: true},
				pgs(12, 33_000_000),
			},
			threshold: 0.25,
			want: map[int]want{
				2:  {},
				7:  {candidate: true, reason: "size ratio 0.06 vs stream 12 (2100000 vs 33000000 bytes)", pairedIdx: idx(12)},
				9:  {},
				12: {pairedIdx: idx(7)},
			},
		},
		{
			// Text subtitles are not part of the size heuristic, which only
			// holds for bitmap tracks.
			name: "non-pgs eng streams are ignored",
			streams: []SubtitleStream{
				{StreamIndex: 3, Codec: "subrip", Language: LanguageEnglish, SizeBytes: 40_000},
				{StreamIndex: 4, Codec: "subrip", Language: LanguageEnglish, SizeBytes: 900_000},
			},
			threshold: 0.25,
			want:      map[int]want{3: {}, 4: {}},
		},
		{
			name:      "single eng pgs track has no pair",
			streams:   []SubtitleStream{pgs(7, 2_100_000)},
			threshold: 0.25,
			want:      map[int]want{7: {}},
		},
		{
			name:      "no eng pgs tracks",
			streams:   []SubtitleStream{{StreamIndex: 3, Codec: "subrip", Language: "deu", SizeBytes: 40_000}},
			threshold: 0.25,
			want:      map[int]want{3: {}},
		},
		{
			// Three tracks: every candidate is measured against the largest,
			// and only the smallest qualifying ratio becomes the candidate.
			name: "three eng pgs tracks flag only the smallest qualifying ratio",
			streams: []SubtitleStream{
				pgs(7, 2_100_000),
				pgs(9, 900_000),
				pgs(12, 33_000_000),
			},
			threshold: 0.25,
			want: map[int]want{
				7:  {},
				9:  {candidate: true, reason: "size ratio 0.03 vs stream 12 (900000 vs 33000000 bytes)", pairedIdx: idx(12)},
				12: {pairedIdx: idx(9)},
			},
		},
		{
			// Three tracks where two are full-size commentary/main tracks:
			// neither is close enough to the largest to qualify.
			name: "three eng pgs tracks with no qualifying ratio",
			streams: []SubtitleStream{
				pgs(7, 30_000_000),
				pgs(9, 31_000_000),
				pgs(12, 33_000_000),
			},
			threshold: 0.25,
			want:      map[int]want{7: {}, 9: {}, 12: {}},
		},
		{
			// Analyze records 0 when it cannot size a stream; that is unknown,
			// not tiny, so it must not divide by zero or produce a flag.
			name: "larger stream of unknown size yields no flag",
			streams: []SubtitleStream{
				pgs(7, 2_100_000),
				pgs(12, 0),
			},
			threshold: 0.25,
			want:      map[int]want{7: {}, 12: {}},
		},
		{
			name: "smaller stream of unknown size yields no flag",
			streams: []SubtitleStream{
				pgs(7, 0),
				pgs(12, 33_000_000),
			},
			threshold: 0.25,
			want:      map[int]want{7: {}, 12: {}},
		},
		{
			// The source already names its forced track, so the heuristic must
			// not add a conflicting second candidate elsewhere.
			name: "source forced flag on another track suppresses the candidate",
			streams: []SubtitleStream{
				pgs(7, 2_100_000),
				func() SubtitleStream { s := pgs(12, 33_000_000); s.ForcedFlagInSource = true; return s }(),
			},
			threshold: 0.25,
			want:      map[int]want{7: {}, 12: {}},
		},
		{
			// A source flag on the stream the heuristic picked agrees with it.
			name: "source forced flag on the picked track still flags it",
			streams: []SubtitleStream{
				func() SubtitleStream { s := pgs(7, 2_100_000); s.ForcedFlagInSource = true; return s }(),
				pgs(12, 33_000_000),
			},
			threshold: 0.25,
			want: map[int]want{
				7:  {candidate: true, reason: "size ratio 0.06 vs stream 12 (2100000 vs 33000000 bytes)", pairedIdx: idx(12)},
				12: {pairedIdx: idx(7)},
			},
		},
		{
			name:      "empty input",
			streams:   nil,
			threshold: 0.25,
			want:      map[int]want{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectForcedCandidates(tt.streams, tt.threshold)

			if len(got) != len(tt.want) {
				t.Fatalf("got %d streams, want %d", len(got), len(tt.want))
			}
			for _, s := range got {
				w, ok := tt.want[s.StreamIndex]
				if !ok {
					t.Fatalf("unexpected stream index %d in result", s.StreamIndex)
				}
				checkStream(t, s, w)
			}
		})
	}
}

func checkStream(t *testing.T, s SubtitleStream, w want) {
	t.Helper()

	if s.ForcedCandidate != w.candidate {
		t.Errorf("stream %d: ForcedCandidate = %t, want %t", s.StreamIndex, s.ForcedCandidate, w.candidate)
	}

	switch {
	case w.reason == "" && s.ForcedCandidateReason != nil:
		t.Errorf("stream %d: ForcedCandidateReason = %q, want nil", s.StreamIndex, *s.ForcedCandidateReason)
	case w.reason != "" && s.ForcedCandidateReason == nil:
		t.Errorf("stream %d: ForcedCandidateReason = nil, want %q", s.StreamIndex, w.reason)
	case w.reason != "" && *s.ForcedCandidateReason != w.reason:
		t.Errorf("stream %d: ForcedCandidateReason = %q, want %q", s.StreamIndex, *s.ForcedCandidateReason, w.reason)
	}

	switch {
	case w.pairedIdx == nil && s.PairedWithStreamIndex != nil:
		t.Errorf("stream %d: PairedWithStreamIndex = %d, want nil", s.StreamIndex, *s.PairedWithStreamIndex)
	case w.pairedIdx != nil && s.PairedWithStreamIndex == nil:
		t.Errorf("stream %d: PairedWithStreamIndex = nil, want %d", s.StreamIndex, *w.pairedIdx)
	case w.pairedIdx != nil && *s.PairedWithStreamIndex != *w.pairedIdx:
		t.Errorf("stream %d: PairedWithStreamIndex = %d, want %d",
			s.StreamIndex, *s.PairedWithStreamIndex, *w.pairedIdx)
	}
}

// The three fields are owned by DetectForcedCandidates, so re-running it over
// an already-annotated list must not accumulate stale pairings.
func TestDetectForcedCandidatesIsIdempotent(t *testing.T) {
	streams := []SubtitleStream{pgs(7, 2_100_000), pgs(12, 33_000_000)}

	once := DetectForcedCandidates(streams, 0.25)
	first := append([]SubtitleStream(nil), once...)
	twice := DetectForcedCandidates(once, 0.25)

	for i := range twice {
		checkStream(t, twice[i], want{
			candidate: first[i].ForcedCandidate,
			reason:    derefString(first[i].ForcedCandidateReason),
			pairedIdx: first[i].PairedWithStreamIndex,
		})
	}
}

// A stream annotated on an earlier run whose sizes no longer qualify must be
// cleared, not left carrying the previous verdict.
func TestDetectForcedCandidatesClearsStaleAnnotation(t *testing.T) {
	reason := "stale"
	streams := []SubtitleStream{
		{
			StreamIndex: 7, Codec: CodecPGS, Language: LanguageEnglish, SizeBytes: 9_000_000,
			ForcedCandidate: true, ForcedCandidateReason: &reason, PairedWithStreamIndex: idx(12),
		},
		{
			StreamIndex: 12, Codec: CodecPGS, Language: LanguageEnglish, SizeBytes: 10_000_000,
			PairedWithStreamIndex: idx(7),
		},
	}

	for _, s := range DetectForcedCandidates(streams, 0.25) {
		checkStream(t, s, want{})
	}
}

// DetectForcedCandidates annotates in place and returns the same slice, so a
// caller may use either the return value or the original variable.
func TestDetectForcedCandidatesAnnotatesInPlace(t *testing.T) {
	streams := []SubtitleStream{pgs(7, 2_100_000), pgs(12, 33_000_000)}

	DetectForcedCandidates(streams, 0.25)

	if !streams[0].ForcedCandidate {
		t.Errorf("stream 7 was not annotated in place")
	}
}

// The sidecar must serialize the new fields under the names and null shape
// docs/MANIFEST.md specifies.
func TestForcedFieldsJSONShape(t *testing.T) {
	streams := DetectForcedCandidates([]SubtitleStream{pgs(7, 2_100_000), pgs(12, 33_000_000)}, 0.25)

	data, err := json.Marshal(streams[1])
	if err != nil {
		t.Fatalf("marshaling stream: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshaling stream: %v", err)
	}

	if v, ok := got["forced_candidate"]; !ok || v != false {
		t.Errorf("forced_candidate = %v (present %t), want false", v, ok)
	}
	if v, ok := got["forced_candidate_reason"]; !ok || v != nil {
		t.Errorf("forced_candidate_reason = %v (present %t), want null", v, ok)
	}
	if v, ok := got["paired_with_stream_index"]; !ok || v != float64(7) {
		t.Errorf("paired_with_stream_index = %v (present %t), want 7", v, ok)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

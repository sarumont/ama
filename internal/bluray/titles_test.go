package bluray

import (
	"reflect"
	"testing"
)

// track builds a Track with no commentary audio.
func track(index, duration int) Track {
	return Track{Index: index, DurationSeconds: duration}
}

// commentaryTrack builds a Track whose second audio stream is flagged as
// commentary, the way makemkv.go reports a real one.
func commentaryTrack(index, duration int) Track {
	t := track(index, duration)
	t.AudioTracks = []AudioTrack{
		{Index: 0, LanguageCode: "eng", Name: "Surround 5.1"},
		{Index: 1, LanguageCode: "eng", Name: "Director's Commentary", Flags: streamFlagDirectorsComments, HasCommentaryFlag: true},
	}
	t.AudioTrackCount = len(t.AudioTracks)
	t.HasCommentaryAudio = true
	return t
}

// want is the part of a Classification a case asserts on, in result order.
type want struct {
	index       int
	role        Role
	edition     string
	needsReview bool
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name             string
		tracks           []Track
		minTrackDuration int
		want             []want
	}{
		{
			name:             "empty track list invents no feature",
			tracks:           nil,
			minTrackDuration: 60,
			want:             nil,
		},
		{
			name:             "single track is trivially the feature",
			tracks:           []Track{track(0, 7647)},
			minTrackDuration: 60,
			want:             []want{{index: 0, role: RoleFeature}},
		},
		{
			name:             "single track shorter than the minimum is still the feature",
			tracks:           []Track{track(3, 12)},
			minTrackDuration: 60,
			want:             []want{{index: 3, role: RoleFeature}},
		},
		{
			name:             "longest track wins regardless of input order",
			tracks:           []Track{track(0, 659), track(1, 7647), track(2, 1200)},
			minTrackDuration: 60,
			want: []want{
				{index: 1, role: RoleFeature},
				{index: 2, role: RoleExtra},
				{index: 0, role: RoleExtra},
			},
		},
		{
			name:             "feature and one extra",
			tracks:           []Track{track(0, 7647), track(2, 659)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 2, role: RoleExtra},
			},
		},
		{
			name:             "duration exactly at the 90% boundary is an alternate cut",
			tracks:           []Track{track(0, 1000), track(1, 900)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleAlternateCut, edition: "Alternate Cut 1", needsReview: true},
			},
		},
		{
			name:             "duration one second below the 90% boundary is an extra",
			tracks:           []Track{track(0, 1000), track(1, 899)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleExtra},
			},
		},
		{
			name:             "multiple alternate cuts are numbered in duration order",
			tracks:           []Track{track(0, 7647), track(1, 7000), track(2, 7300), track(3, 659)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 2, role: RoleAlternateCut, edition: "Alternate Cut 1", needsReview: true},
				{index: 1, role: RoleAlternateCut, edition: "Alternate Cut 2", needsReview: true},
				{index: 3, role: RoleExtra},
			},
		},
		{
			name:             "multiple extras",
			tracks:           []Track{track(0, 7647), track(1, 900), track(2, 659), track(3, 120)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleExtra},
				{index: 2, role: RoleExtra},
				{index: 3, role: RoleExtra},
			},
		},
		{
			name:             "duration exactly at min_track_duration is an extra not a skip",
			tracks:           []Track{track(0, 7647), track(1, 60)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleExtra},
			},
		},
		{
			name:             "duration one second below min_track_duration is skipped",
			tracks:           []Track{track(0, 7647), track(1, 59)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleSkip},
			},
		},
		{
			name:             "playlist padding below the minimum is all skipped",
			tracks:           []Track{track(0, 7647), track(1, 42), track(2, 12), track(3, 5), track(4, 1)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleSkip},
				{index: 2, role: RoleSkip},
				{index: 3, role: RoleSkip},
				{index: 4, role: RoleSkip},
			},
		},
		{
			name:             "zero min_track_duration skips nothing",
			tracks:           []Track{track(0, 7647), track(1, 12), track(2, 1)},
			minTrackDuration: 0,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleExtra},
				{index: 2, role: RoleExtra},
			},
		},

		// The two cases that prove the rule ORDER from docs/CLAUDE.md.
		{
			name:             "rule order: commentary within 10% of the feature is an alternate cut",
			tracks:           []Track{track(0, 1000), commentaryTrack(1, 950)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleAlternateCut, edition: "Alternate Cut 1", needsReview: true},
			},
		},
		{
			name:             "rule order: commentary below min_track_duration is a commentary not a skip",
			tracks:           []Track{track(0, 7647), commentaryTrack(1, 30)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleCommentary},
			},
		},
		{
			name:             "commentary above the minimum and below 90% is a commentary",
			tracks:           []Track{track(0, 7647), commentaryTrack(1, 3000)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleCommentary},
			},
		},
		{
			name: "commentary detected from the audio streams alone",
			tracks: []Track{
				track(0, 7647),
				{
					Index:           1,
					DurationSeconds: 30,
					AudioTracks: []AudioTrack{
						{Index: 0, LanguageCode: "eng", Name: "Commentary", HasCommentaryFlag: true},
					},
					// HasCommentaryAudio deliberately left false.
				},
			},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleCommentary},
			},
		},
		{
			name: "commentary detected from the track summary alone",
			tracks: []Track{
				track(0, 7647),
				{Index: 1, DurationSeconds: 30, HasCommentaryAudio: true},
			},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleCommentary},
			},
		},
		{
			name:             "audio streams without the commentary flag do not make a commentary",
			tracks:           []Track{track(0, 7647), {Index: 1, DurationSeconds: 30, AudioTracks: []AudioTrack{{Index: 0, LanguageCode: "eng", Name: "Surround 5.1"}}}},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleSkip},
			},
		},

		// Tiebreak: equal durations order by ascending MakeMKV index, so the
		// lowest-indexed of the tied titles becomes the feature.
		{
			name:             "equal durations: lowest index becomes the feature",
			tracks:           []Track{track(5, 7647), track(2, 7647)},
			minTrackDuration: 60,
			want: []want{
				{index: 2, role: RoleFeature},
				{index: 5, role: RoleAlternateCut, edition: "Alternate Cut 1", needsReview: true},
			},
		},
		{
			name:             "equal durations: input order does not change the feature",
			tracks:           []Track{track(2, 7647), track(5, 7647)},
			minTrackDuration: 60,
			want: []want{
				{index: 2, role: RoleFeature},
				{index: 5, role: RoleAlternateCut, edition: "Alternate Cut 1", needsReview: true},
			},
		},
		{
			name:             "equal durations lower down keep ascending index order",
			tracks:           []Track{track(3, 659), track(0, 7647), track(1, 659)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleExtra},
				{index: 3, role: RoleExtra},
			},
		},
		{
			name:             "disc exposing the feature as several identical playlists",
			tracks:           []Track{track(0, 7647), track(1, 7647), track(2, 7647), track(3, 300)},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleAlternateCut, edition: "Alternate Cut 1", needsReview: true},
				{index: 2, role: RoleAlternateCut, edition: "Alternate Cut 2", needsReview: true},
				{index: 3, role: RoleExtra},
			},
		},
		{
			name: "full disc: feature, alternate cut, commentary, extras and padding",
			tracks: []Track{
				track(0, 7647),
				commentaryTrack(1, 7640),
				commentaryTrack(2, 5400),
				track(3, 659),
				track(4, 30),
				track(5, 7000),
			},
			minTrackDuration: 60,
			want: []want{
				{index: 0, role: RoleFeature},
				{index: 1, role: RoleAlternateCut, edition: "Alternate Cut 1", needsReview: true},
				{index: 5, role: RoleAlternateCut, edition: "Alternate Cut 2", needsReview: true},
				{index: 2, role: RoleCommentary},
				{index: 3, role: RoleExtra},
				{index: 4, role: RoleSkip},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.tracks, tt.minTrackDuration)
			if got == nil {
				t.Fatal("Classify returned a nil slice, want empty")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d classifications, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				c := got[i]
				if c.Track.Index != w.index {
					t.Errorf("result[%d]: track index = %d, want %d", i, c.Track.Index, w.index)
				}
				if c.Role != w.role {
					t.Errorf("result[%d] (track %d): role = %q, want %q", i, c.Track.Index, c.Role, w.role)
				}
				if c.Edition != w.edition {
					t.Errorf("result[%d] (track %d): edition = %q, want %q", i, c.Track.Index, c.Edition, w.edition)
				}
				if c.NeedsReview != w.needsReview {
					t.Errorf("result[%d] (track %d): needs review = %t, want %t", i, c.Track.Index, c.NeedsReview, w.needsReview)
				}
				if c.Reason == "" {
					t.Errorf("result[%d] (track %d): reason is empty", i, c.Track.Index)
				}
			}
		})
	}
}

// TestClassifyReasons pins the role_reason strings, which are written to the
// manifest and read by humans.
func TestClassifyReasons(t *testing.T) {
	got := Classify([]Track{
		track(0, 1000),
		track(1, 950),
		commentaryTrack(2, 400),
		track(3, 200),
		track(4, 30),
	}, 60)

	want := []string{
		"longest track",
		"duration within 10% of feature",
		"audio track flagged as commentary",
		"duration below feature threshold",
		"duration below minimum track duration",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d classifications, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Reason != w {
			t.Errorf("result[%d] (role %q): reason = %q, want %q", i, got[i].Role, got[i].Reason, w)
		}
	}
}

// TestClassifyDoesNotModifyInput guards the purity of Classify: the caller's
// slice must come back in the order it went in, untouched.
func TestClassifyDoesNotModifyInput(t *testing.T) {
	tracks := []Track{track(0, 659), track(1, 7647), commentaryTrack(2, 3000), track(3, 30)}
	before := make([]Track, len(tracks))
	copy(before, tracks)

	Classify(tracks, 60)

	if !reflect.DeepEqual(tracks, before) {
		t.Errorf("Classify modified its input:\n got %+v\nwant %+v", tracks, before)
	}
}

// TestClassifyIsDeterministic checks that the same disc classifies identically
// no matter what order makemkvcon happened to report the titles in.
func TestClassifyIsDeterministic(t *testing.T) {
	orders := [][]Track{
		{track(0, 7647), track(1, 7647), track(2, 659), track(3, 30)},
		{track(3, 30), track(2, 659), track(1, 7647), track(0, 7647)},
		{track(1, 7647), track(3, 30), track(0, 7647), track(2, 659)},
		{track(2, 659), track(0, 7647), track(3, 30), track(1, 7647)},
	}

	first := Classify(orders[0], 60)
	for i, order := range orders[1:] {
		if got := Classify(order, 60); !reflect.DeepEqual(got, first) {
			t.Errorf("input order %d classified differently:\n got %+v\nwant %+v", i+1, got, first)
		}
	}
}

func TestRoleProducesOutput(t *testing.T) {
	tests := []struct {
		role Role
		want bool
	}{
		{RoleFeature, true},
		{RoleAlternateCut, true},
		{RoleCommentary, true},
		{RoleExtra, true},
		{RoleSkip, false},
	}
	for _, tt := range tests {
		if got := tt.role.ProducesOutput(); got != tt.want {
			t.Errorf("Role(%q).ProducesOutput() = %t, want %t", tt.role, got, tt.want)
		}
	}
}

func TestRoleSubdirectory(t *testing.T) {
	tests := []struct {
		role Role
		want string
	}{
		{RoleFeature, ""},
		{RoleAlternateCut, ""},
		{RoleCommentary, "extras"},
		{RoleExtra, "extras"},
		{RoleSkip, ""},
	}
	for _, tt := range tests {
		if got := tt.role.Subdirectory(); got != tt.want {
			t.Errorf("Role(%q).Subdirectory() = %q, want %q", tt.role, got, tt.want)
		}
	}
}

// TestRoleValues pins the serialized role strings to docs/MANIFEST.md.
func TestRoleValues(t *testing.T) {
	tests := []struct {
		role Role
		want string
	}{
		{RoleFeature, "feature"},
		{RoleAlternateCut, "alternate_cut"},
		{RoleCommentary, "commentary"},
		{RoleExtra, "extra"},
		{RoleSkip, "skip"},
	}
	for _, tt := range tests {
		if string(tt.role) != tt.want {
			t.Errorf("role = %q, want %q", tt.role, tt.want)
		}
	}
}

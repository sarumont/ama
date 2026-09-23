package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sarumont/ama/internal/bluray"
	"github.com/sarumont/ama/internal/identify"
	"github.com/sarumont/ama/internal/manifest"
	"github.com/sarumont/ama/internal/musicbrainz"
	"github.com/sarumont/ama/internal/subtitle"
)

func TestOutputFileName(t *testing.T) {
	tests := []struct {
		name  string
		index int
		cls   bluray.Classification
		want  string
	}{
		{
			name: "feature",
			cls:  bluray.Classification{Role: bluray.RoleFeature},
			want: "Iron Man 3 (2013).mkv",
		},
		{
			name: "alternate cut carries the edition name",
			cls:  bluray.Classification{Role: bluray.RoleAlternateCut, Edition: "Alternate Cut 1"},
			want: "Iron Man 3 (2013) {edition-Alternate Cut 1}.mkv",
		},
		{
			name:  "extra goes under extras/ named by makemkv index",
			index: 2,
			cls:   bluray.Classification{Role: bluray.RoleExtra},
			want:  filepath.Join("extras", "Iron Man 3 (2013)_t02.mkv"),
		},
		{
			name:  "commentary also goes under extras/",
			index: 11,
			cls:   bluray.Classification{Role: bluray.RoleCommentary},
			want:  filepath.Join("extras", "Iron Man 3 (2013)_t11.mkv"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outputFileName("Iron Man 3 (2013)", tt.index, tt.cls); got != tt.want {
				t.Errorf("outputFileName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRelativeToRoot(t *testing.T) {
	tests := []struct {
		name string
		root string
		path string
		want string
	}{
		{
			name: "path under root",
			root: "/media/library/music",
			path: "/media/library/music/Artist/Album/01 - Track.flac",
			want: filepath.Join("Artist", "Album", "01 - Track.flac"),
		},
		{
			name: "path not under root falls back to base name",
			root: "/media/library/music",
			path: "/tmp/somewhere/else/01 - Track.flac",
			want: "01 - Track.flac",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relativeToRoot(tt.root, tt.path); got != tt.want {
				t.Errorf("relativeToRoot(%q, %q) = %q, want %q", tt.root, tt.path, got, tt.want)
			}
		})
	}
}

func TestMoveFile(t *testing.T) {
	t.Run("same filesystem rename", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src.mkv")
		dst := filepath.Join(dir, "dst.mkv")
		if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
			t.Fatalf("writing src: %v", err)
		}
		if err := moveFile(src, dst); err != nil {
			t.Fatalf("moveFile: %v", err)
		}
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Errorf("source still exists after move: %v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != "data" {
			t.Errorf("dst content = %q, %v, want %q, nil", got, err, "data")
		}
	})

	t.Run("missing source is an error", func(t *testing.T) {
		dir := t.TempDir()
		if err := moveFile(filepath.Join(dir, "absent"), filepath.Join(dir, "dst")); err == nil {
			t.Error("moveFile with a missing source: want an error, got nil")
		}
	})
}

func TestFindRelease(t *testing.T) {
	releases := []musicbrainz.Release{
		{MBReleaseID: "aaa", Artist: "Artist A"},
		{MBReleaseID: "bbb", Artist: "Artist B"},
	}

	if got, ok := findRelease(releases, "bbb"); !ok || got.Artist != "Artist B" {
		t.Errorf("findRelease(bbb) = %+v, %v, want Artist B, true", got, ok)
	}
	if _, ok := findRelease(releases, "ccc"); ok {
		t.Error("findRelease(ccc): want not found, got a match")
	}
	if _, ok := findRelease(releases, ""); ok {
		t.Error("findRelease(\"\"): want not found (unmatched manual entry), got a match")
	}
}

func TestTrackInfoFor(t *testing.T) {
	release := musicbrainz.Release{
		Tracks: []musicbrainz.Track{
			{Number: 1, Title: "Where I Need to Be", DurationSeconds: 207},
			{Number: 2, Title: "California (Cast Iron Soul)", DurationSeconds: 245},
		},
	}

	title, duration := trackInfoFor(release, 2)
	if title != "California (Cast Iron Soul)" || duration != 245 {
		t.Errorf("trackInfoFor(2) = %q, %d, want %q, 245", title, duration, "California (Cast Iron Soul)")
	}

	title, duration = trackInfoFor(release, 99)
	if title != "" || duration != 0 {
		t.Errorf("trackInfoFor(99) = %q, %d, want \"\", 0", title, duration)
	}
}

func TestCandidatesToManifest(t *testing.T) {
	ranked := []identify.RankedCandidate{
		{Candidate: identify.Candidate{TMDBID: 68721, Title: "Iron Man 3", Year: 2013}, Score: 0.97},
	}
	want := []manifest.Candidate{
		{TMDBID: 68721, Title: "Iron Man 3", Year: 2013, Score: 0.97},
	}
	if got := candidatesToManifest(ranked); !reflect.DeepEqual(got, want) {
		t.Errorf("candidatesToManifest() = %+v, want %+v", got, want)
	}
}

func TestMBReleasesToManifest(t *testing.T) {
	releases := []musicbrainz.Release{
		{MBReleaseID: "aaa", MBReleaseGroupID: "bbb", Artist: "Jamestown Revival", Album: "Utah", Year: 2016},
	}
	want := []manifest.Candidate{
		{MBReleaseID: "aaa", MBReleaseGroupID: "bbb", Artist: "Jamestown Revival", Album: "Utah", Year: 2016},
	}
	if got := mbReleasesToManifest(releases); !reflect.DeepEqual(got, want) {
		t.Errorf("mbReleasesToManifest() = %+v, want %+v", got, want)
	}
}

func TestSubtitlesToManifest(t *testing.T) {
	reason := "size ratio 0.06 vs stream 12"
	streams := []subtitle.SubtitleStream{
		{
			StreamIndex:           7,
			Codec:                 "hdmv_pgs_subtitle",
			Language:              "eng",
			SizeBytes:             2100000,
			ForcedCandidate:       true,
			ForcedCandidateReason: &reason,
			NeedsOCR:              true,
			Converted:             true,
		},
	}
	want := []manifest.Subtitle{
		{
			StreamIndex:           7,
			Codec:                 "hdmv_pgs_subtitle",
			Language:              "eng",
			SizeBytes:             2100000,
			ForcedCandidate:       true,
			ForcedCandidateReason: &reason,
			NeedsOCR:              true,
			Converted:             true,
		},
	}
	got := subtitlesToManifest(streams)
	if len(got) != 1 || got[0].StreamIndex != want[0].StreamIndex || got[0].ForcedCandidate != want[0].ForcedCandidate ||
		got[0].ForcedCandidateReason == nil || *got[0].ForcedCandidateReason != *want[0].ForcedCandidateReason {
		t.Errorf("subtitlesToManifest() = %+v, want %+v", got, want)
	}
}

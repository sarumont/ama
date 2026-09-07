package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestRoundTrip is the real verification that these types match
// docs/MANIFEST.md: both documented examples must decode without loss and
// re-encode to the same JSON. A field missing from the Go types is dropped on
// decode and absent on encode, so the comparison catches it.
func TestRoundTrip(t *testing.T) {
	for _, name := range []string{"bluray.json", "cd.json"} {
		t.Run(name, func(t *testing.T) {
			original := readFixture(t, name)

			var m Manifest
			if err := json.Unmarshal(original, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			encoded, err := json.Marshal(&m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			want := decodeAny(t, original)
			got := decodeAny(t, encoded)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("round trip changed the manifest\n want: %s\n  got: %s", original, encoded)
			}
		})
	}
}

func TestUnmarshalBluRay(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal(readFixture(t, "bluray.json"), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if m.ID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("ID = %q", m.ID)
	}
	if m.Status != StatusComplete {
		t.Errorf("Status = %q, want %q", m.Status, StatusComplete)
	}
	if want := time.Date(2026, 8, 19, 14, 23, 0, 0, time.UTC); !m.RippedAt.Equal(want) {
		t.Errorf("RippedAt = %v, want %v", m.RippedAt, want)
	}
	if m.Disc.Type != DiscTypeBluRay {
		t.Errorf("Disc.Type = %q, want %q", m.Disc.Type, DiscTypeBluRay)
	}
	if m.Disc.Label == nil || *m.Disc.Label != "MARVELS_IRON_MAN_3_BLU_RAY" {
		t.Errorf("Disc.Label = %v", m.Disc.Label)
	}
	if m.Identification.TMDBID == nil || *m.Identification.TMDBID != 68721 {
		t.Errorf("Identification.TMDBID = %v", m.Identification.TMDBID)
	}
	if m.Identification.Confidence == nil || *m.Identification.Confidence != 0.97 {
		t.Errorf("Identification.Confidence = %v", m.Identification.Confidence)
	}
	if got := len(m.Identification.Candidates); got != 2 {
		t.Fatalf("len(Candidates) = %d, want 2", got)
	}
	if got := m.Identification.Candidates[1].Title; got != "Avengers: Age of Ultron" {
		t.Errorf("Candidates[1].Title = %q", got)
	}

	if got := len(m.Tracks); got != 2 {
		t.Fatalf("len(Tracks) = %d, want 2", got)
	}
	feature := m.Tracks[0]
	if feature.MakeMKVIndex == nil || *feature.MakeMKVIndex != 0 {
		t.Errorf("Tracks[0].MakeMKVIndex = %v, want 0", feature.MakeMKVIndex)
	}
	if feature.Role != RoleFeature {
		t.Errorf("Tracks[0].Role = %q, want %q", feature.Role, RoleFeature)
	}
	if feature.SizeBytes == nil || *feature.SizeBytes != 28000000000 {
		t.Errorf("Tracks[0].SizeBytes = %v", feature.SizeBytes)
	}
	if feature.Edition != nil {
		t.Errorf("Tracks[0].Edition = %v, want nil", *feature.Edition)
	}
	if m.Tracks[1].Role != RoleExtra {
		t.Errorf("Tracks[1].Role = %q, want %q", m.Tracks[1].Role, RoleExtra)
	}

	if got := len(m.Subtitles); got != 2 {
		t.Fatalf("len(Subtitles) = %d, want 2", got)
	}
	forced := m.Subtitles[0]
	if !forced.ForcedCandidate {
		t.Error("Subtitles[0].ForcedCandidate = false, want true")
	}
	if forced.PairedWithStreamIndex == nil || *forced.PairedWithStreamIndex != 12 {
		t.Errorf("Subtitles[0].PairedWithStreamIndex = %v", forced.PairedWithStreamIndex)
	}
	if m.Subtitles[1].ForcedCandidateReason != nil {
		t.Errorf("Subtitles[1].ForcedCandidateReason = %v, want nil", *m.Subtitles[1].ForcedCandidateReason)
	}

	if m.Output.ProcessedFeature != "Iron Man 3 (2013).processed.mkv" {
		t.Errorf("Output.ProcessedFeature = %q", m.Output.ProcessedFeature)
	}
	if m.Radarr == nil || !m.Radarr.Added || m.Radarr.AddedAt == nil {
		t.Fatalf("Radarr = %+v", m.Radarr)
	}
	if m.Sonarr == nil || m.Sonarr.Added || m.Sonarr.AddedAt != nil {
		t.Errorf("Sonarr = %+v, want zero state", m.Sonarr)
	}
}

func TestUnmarshalCD(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal(readFixture(t, "cd.json"), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if m.Disc.Type != DiscTypeCD {
		t.Errorf("Disc.Type = %q, want %q", m.Disc.Type, DiscTypeCD)
	}
	if m.Disc.Label != nil {
		t.Errorf("Disc.Label = %v, want nil", *m.Disc.Label)
	}
	if m.Identification.Method != "musicbrainz" {
		t.Errorf("Identification.Method = %q", m.Identification.Method)
	}
	if m.Identification.Artist != "Jamestown Revival" {
		t.Errorf("Identification.Artist = %q", m.Identification.Artist)
	}
	if m.Identification.MBReleaseGroupID == "" {
		t.Error("Identification.MBReleaseGroupID is empty")
	}
	if m.Identification.TMDBID != nil {
		t.Errorf("Identification.TMDBID = %v, want nil on a CD", *m.Identification.TMDBID)
	}

	if got := len(m.Tracks); got != 1 {
		t.Fatalf("len(Tracks) = %d, want 1", got)
	}
	track := m.Tracks[0]
	if track.Number == nil || *track.Number != 1 {
		t.Errorf("Tracks[0].Number = %v", track.Number)
	}
	if track.AccurateRip == nil || !*track.AccurateRip {
		t.Errorf("Tracks[0].AccurateRip = %v", track.AccurateRip)
	}
	if track.Title != "Where I Need to Be" {
		t.Errorf("Tracks[0].Title = %q", track.Title)
	}

	if m.Subtitles != nil {
		t.Errorf("Subtitles = %v, want nil on a CD", m.Subtitles)
	}
	if m.Radarr != nil || m.Sonarr != nil {
		t.Errorf("Radarr = %v, Sonarr = %v, want nil on a CD", m.Radarr, m.Sonarr)
	}
	if m.Output.MainFeature != "" {
		t.Errorf("Output.MainFeature = %q, want empty on a CD", m.Output.MainFeature)
	}
}

// TestMarshalOmitsOtherVariant checks the two shapes stay separated on the way
// out: a CD manifest carries no Blu-ray keys and a Blu-ray track keeps its
// documented "edition": null.
func TestMarshalOmitsOtherVariant(t *testing.T) {
	cd := New(DiscTypeCD, "/dev/sr0")
	number := 1
	cd.Tracks = []Track{{Number: &number, Title: "Where I Need to Be", DurationSeconds: 207}}

	encoded, err := json.Marshal(cd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := decodeAny(t, encoded).(map[string]any)
	for _, key := range []string{"subtitles", "radarr", "sonarr"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("CD manifest carries %q", key)
		}
	}
	track := decoded["tracks"].([]any)[0].(map[string]any)
	for _, key := range []string{"makemkv_index", "role", "edition", "size_bytes"} {
		if _, ok := track[key]; ok {
			t.Errorf("CD track carries Blu-ray key %q", key)
		}
	}

	bd := New(DiscTypeBluRay, "/dev/sr0")
	index := 0
	bd.Tracks = []Track{{MakeMKVIndex: &index, Role: RoleFeature, DurationSeconds: 7647}}
	encoded, err = json.Marshal(bd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	track = decodeAny(t, encoded).(map[string]any)["tracks"].([]any)[0].(map[string]any)
	edition, ok := track["edition"]
	if !ok || edition != nil {
		t.Errorf("Blu-ray track edition = %v (present: %t), want an explicit null", edition, ok)
	}
	if got := track["makemkv_index"]; got != float64(0) {
		t.Errorf("Blu-ray track makemkv_index = %v, want 0", got)
	}
}

func TestMarshalEmptyWarningsAndErrors(t *testing.T) {
	m := Manifest{}
	encoded, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := decodeAny(t, encoded).(map[string]any)
	for _, key := range []string{"warnings", "errors"} {
		value, ok := decoded[key]
		if !ok {
			t.Fatalf("%q is missing", key)
		}
		list, ok := value.([]any)
		if !ok || len(list) != 0 {
			t.Errorf("%q = %v, want []", key, value)
		}
	}
}

func TestNew(t *testing.T) {
	before := time.Now().UTC()
	m := New(DiscTypeBluRay, "/dev/sr0")

	if len(m.ID) != 36 {
		t.Errorf("ID = %q, want a 36 character UUID", m.ID)
	}
	if m.AMAVersion != Version {
		t.Errorf("AMAVersion = %q, want %q", m.AMAVersion, Version)
	}
	if m.Status != StatusPendingConfirmation {
		t.Errorf("Status = %q, want %q", m.Status, StatusPendingConfirmation)
	}
	if m.RippedAt.Before(before) || m.RippedAt.After(time.Now().UTC()) {
		t.Errorf("RippedAt = %v, want a timestamp from this test", m.RippedAt)
	}
	if m.Disc.Type != DiscTypeBluRay || m.Disc.Device != "/dev/sr0" {
		t.Errorf("Disc = %+v", m.Disc)
	}
	if other := New(DiscTypeCD, "/dev/sr0"); other.ID == m.ID {
		t.Errorf("two manifests share the ID %q", m.ID)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

// decodeAny decodes JSON into plain Go values so two encodings can be compared
// without caring about key order or whitespace.
func decodeAny(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	return v
}

package cd

import (
	"os"
	"path/filepath"
	"testing"
)

func readLogFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "whipper-log", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func TestParseLog(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    RipResult
	}{
		{
			name:    "all tracks accurately ripped",
			fixture: "success.log",
			want: RipResult{
				AccurateRipSummary: AccurateRipAllAccurate,
				HealthStatus:       HealthOK,
				Tracks: []RippedTrack{
					{
						Number:            1,
						Filename:          "/music/Test Artist/Test Album/01 - First Track.flac",
						PeakLevel:         0.988321,
						ExtractionQuality: "100.00 %",
						TestCRC:           "A1B2C3D4",
						CopyCRC:           "A1B2C3D4",
						AccurateRipV1:     &ARResult{Result: "Found, exact match", Confidence: 12, LocalCRC: "DEADBEEF", RemoteCRC: "DEADBEEF"},
						AccurateRipV2:     &ARResult{Result: "Found, exact match", Confidence: 9, LocalCRC: "CAFEF00D", RemoteCRC: "CAFEF00D"},
						Status:            TrackStatusCopyOK,
					},
					{
						Number:            2,
						Filename:          "/music/Test Artist/Test Album/02 - Second Track.flac",
						PeakLevel:         0.912345,
						ExtractionQuality: "100.00 %",
						TestCRC:           "11223344",
						CopyCRC:           "11223344",
						AccurateRipV1:     &ARResult{Result: "Found, exact match", Confidence: 12, LocalCRC: "0BADF00D", RemoteCRC: "0BADF00D"},
						AccurateRipV2:     &ARResult{Result: "Found, exact match", Confidence: 9, LocalCRC: "8BADF00D", RemoteCRC: "8BADF00D"},
						Status:            TrackStatusCopyOK,
					},
				},
			},
		},
		{
			name:    "not in accuraterip database",
			fixture: "not-in-database.log",
			want: RipResult{
				AccurateRipSummary: AccurateRipNotInDatabase,
				HealthStatus:       HealthOK,
				Tracks: []RippedTrack{
					{
						Number:            1,
						Filename:          "/music/Obscure Artist/Obscure Album/01 - Only Track.flac",
						PeakLevel:         0.955512,
						ExtractionQuality: "100.00 %",
						TestCRC:           "5A5A5A5A",
						CopyCRC:           "5A5A5A5A",
						AccurateRipV1:     &ARResult{Result: "Track not present in AccurateRip database"},
						AccurateRipV2:     &ARResult{Result: "Track not present in AccurateRip database"},
						Status:            TrackStatusCopyOK,
					},
				},
			},
		},
		{
			name:    "accuraterip mismatch",
			fixture: "mismatch.log",
			want: RipResult{
				AccurateRipSummary: AccurateRipNoneVerifiable,
				HealthStatus:       HealthOK,
				Tracks: []RippedTrack{
					{
						Number:            1,
						Filename:          "/music/Pressing Variant/Some Album/01 - Track One.flac",
						PeakLevel:         0.943211,
						ExtractionQuality: "100.00 %",
						TestCRC:           "9F9F9F9F",
						CopyCRC:           "9F9F9F9F",
						AccurateRipV1:     &ARResult{Result: "No match", LocalCRC: "12345678", RemoteCRC: "87654321"},
						AccurateRipV2:     &ARResult{Result: "No match", LocalCRC: "ABCDEF01", RemoteCRC: "10FEDCBA"},
						Status:            TrackStatusCopyOK,
					},
				},
			},
		},
		{
			name:    "partial skip",
			fixture: "partial-skip.log",
			want: RipResult{
				AccurateRipSummary: "Some tracks could not be verified as accurate (1/2 got no match)",
				HealthStatus:       HealthSkipped,
				Tracks: []RippedTrack{
					{
						Number:            1,
						Filename:          "/music/Flaky Disc/Flaky Album/01 - Good Track.flac",
						PeakLevel:         0.921212,
						ExtractionQuality: "100.00 %",
						TestCRC:           "1A2B3C4D",
						CopyCRC:           "1A2B3C4D",
						AccurateRipV1:     &ARResult{Result: "Found, exact match", Confidence: 5, LocalCRC: "AABBCCDD", RemoteCRC: "AABBCCDD"},
						AccurateRipV2:     &ARResult{Result: "Found, exact match", Confidence: 4, LocalCRC: "11335577", RemoteCRC: "11335577"},
						Status:            TrackStatusCopyOK,
					},
					{
						Number: 2,
						Status: TrackStatusSkipped,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := readLogFixture(t, tt.fixture)
			got, err := parseLog(data)
			if err != nil {
				t.Fatalf("parseLog: %v", err)
			}
			assertRipResult(t, *got, tt.want)
		})
	}
}

func TestParseLogNoTracks(t *testing.T) {
	_, err := parseLog([]byte("Conclusive status report:\n  AccurateRip summary: All tracks accurately ripped\n  Health status: No errors occurred\n"))
	if err == nil {
		t.Fatal("parseLog: want error for a log with no Tracks section, got nil")
	}
}

func TestRippedTrackVerified(t *testing.T) {
	tests := []struct {
		name  string
		track RippedTrack
		want  bool
	}{
		{"exact match v1", RippedTrack{AccurateRipV1: &ARResult{Result: "Found, exact match"}}, true},
		{"exact match v2 only", RippedTrack{AccurateRipV2: &ARResult{Result: "Found, exact match"}}, true},
		{"no match", RippedTrack{AccurateRipV1: &ARResult{Result: "No match"}}, false},
		{"not present", RippedTrack{AccurateRipV1: &ARResult{Result: "Track not present in AccurateRip database"}}, false},
		{"nil results", RippedTrack{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.track.Verified(); got != tt.want {
				t.Errorf("Verified() = %v, want %v", got, tt.want)
			}
		})
	}
}

// assertRipResult compares two RipResults field by field with readable
// failure messages; reflect.DeepEqual on a struct this nested just says
// "not equal" without saying where.
func assertRipResult(t *testing.T, got, want RipResult) {
	t.Helper()
	if got.AccurateRipSummary != want.AccurateRipSummary {
		t.Errorf("AccurateRipSummary = %q, want %q", got.AccurateRipSummary, want.AccurateRipSummary)
	}
	if got.HealthStatus != want.HealthStatus {
		t.Errorf("HealthStatus = %q, want %q", got.HealthStatus, want.HealthStatus)
	}
	if len(got.Tracks) != len(want.Tracks) {
		t.Fatalf("len(Tracks) = %d, want %d (%+v)", len(got.Tracks), len(want.Tracks), got.Tracks)
	}
	for i := range want.Tracks {
		g, w := got.Tracks[i], want.Tracks[i]
		if g.Number != w.Number || g.Filename != w.Filename || g.PeakLevel != w.PeakLevel ||
			g.ExtractionQuality != w.ExtractionQuality || g.TestCRC != w.TestCRC || g.CopyCRC != w.CopyCRC ||
			g.Status != w.Status {
			t.Errorf("Tracks[%d] = %+v, want %+v", i, g, w)
		}
		if !equalAR(g.AccurateRipV1, w.AccurateRipV1) {
			t.Errorf("Tracks[%d].AccurateRipV1 = %+v, want %+v", i, g.AccurateRipV1, w.AccurateRipV1)
		}
		if !equalAR(g.AccurateRipV2, w.AccurateRipV2) {
			t.Errorf("Tracks[%d].AccurateRipV2 = %+v, want %+v", i, g.AccurateRipV2, w.AccurateRipV2)
		}
	}
}

func equalAR(a, b *ARResult) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

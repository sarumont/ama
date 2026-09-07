package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestWriteReadRoundTrip is the core guarantee: a manifest that goes through
// Write and comes back through Read is the same manifest. Both documented
// variants are covered, and the on-disk bytes are compared against the fixture
// so an encoding regression in Write shows up here too.
func TestWriteReadRoundTrip(t *testing.T) {
	for _, fixture := range []string{"bluray.json", "cd.json"} {
		t.Run(fixture, func(t *testing.T) {
			original := readFixture(t, fixture)
			var want Manifest
			if err := json.Unmarshal(original, &want); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}

			path := filepath.Join(t.TempDir(), "rip.manifest.json")
			if err := Write(path, &want); err != nil {
				t.Fatalf("Write: %v", err)
			}

			got, err := Read(path)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !reflect.DeepEqual(&want, got) {
				t.Errorf("Read returned a different manifest\n want: %+v\n  got: %+v", &want, got)
			}

			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading written manifest: %v", err)
			}
			if !reflect.DeepEqual(decodeAny(t, original), decodeAny(t, onDisk)) {
				t.Errorf("written manifest differs from the fixture\n want: %s\n  got: %s", original, onDisk)
			}
			if !strings.HasSuffix(string(onDisk), "}\n") {
				t.Errorf("manifest is not indented JSON ending in a newline: %q", onDisk)
			}
		})
	}
}

// TestWriteCreatesParentDirectory covers the choice to MkdirAll: a rip's output
// directory does not exist before the rip, so Write makes it rather than forcing
// every caller to remember to.
func TestWriteCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Iron Man 3 (2013)", "Iron Man 3 (2013).manifest.json")
	if err := Write(path, New(DiscTypeBluRay, "/dev/sr0")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("manifest was not created: %v", err)
	}
}

// TestWriteLeavesNoTempFile checks the success path cleans up after itself: the
// deferred Remove must not fire on a name that has already been renamed away,
// and nothing but the manifest may be left in the rip directory.
func TestWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rip.manifest.json")
	for range 3 {
		if err := Write(path, New(DiscTypeBluRay, "/dev/sr0")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	assertOnlyFile(t, dir, "rip.manifest.json")
}

// TestWriteReplacesAtomically checks the destination is never left holding a
// mixture of the old and new manifests. The old content here is much longer than
// the new one, so a non-atomic overwrite in place would leave a tail of the old
// manifest behind and the result would not parse.
func TestWriteReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rip.manifest.json")

	var old Manifest
	if err := json.Unmarshal(readFixture(t, "bluray.json"), &old); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if err := Write(path, &old); err != nil {
		t.Fatalf("Write old: %v", err)
	}

	replacement := New(DiscTypeCD, "/dev/sr0")
	if err := Write(path, replacement); err != nil {
		t.Fatalf("Write replacement: %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ID != replacement.ID {
		t.Errorf("ID = %q, want the replacement's %q", got.ID, replacement.ID)
	}
	if got.Tracks != nil {
		t.Errorf("Tracks = %v, want nil — the old manifest's tracks survived", got.Tracks)
	}
	assertOnlyFile(t, dir, "rip.manifest.json")
}

// TestWriteErrorsLeaveNothingBehind forces failures at the two points that can
// fail once the temp file exists or before it does, and checks each one reports
// an error and drops no partial state.
func TestWriteErrorsLeaveNothingBehind(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the destination path, having arranged for the write to
		// fail, plus the directory that must be left clean.
		setup func(t *testing.T) (path, dir string)
	}{
		{
			// A directory sitting on the destination name makes the rename
			// fail after the temp file has been written and synced, which is
			// the only way to reach the deferred cleanup on a real filesystem.
			name: "rename fails",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				path := filepath.Join(dir, "rip.manifest.json")
				if err := os.MkdirAll(filepath.Join(path, "occupied"), dirPerm); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return path, dir
			},
		},
		{
			// A plain file where the rip directory belongs makes MkdirAll fail
			// before any temp file exists.
			name: "parent directory cannot be created",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				blocker := filepath.Join(dir, "Iron Man 3 (2013)")
				if err := os.WriteFile(blocker, []byte("not a directory"), filePerm); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return filepath.Join(blocker, "rip.manifest.json"), dir
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, dir := tc.setup(t)
			if err := Write(path, New(DiscTypeBluRay, "/dev/sr0")); err == nil {
				t.Fatal("Write succeeded, want an error")
			}
			for _, entry := range readDir(t, dir) {
				if strings.HasSuffix(entry, ".tmp") {
					t.Errorf("Write left a temp file behind: %s", entry)
				}
			}
		})
	}
}

// TestUpdateStatusSequence walks the four documented transitions from
// docs/CLAUDE.md "Manifest Updates" and checks each one persists its own fields
// and preserves everything the earlier steps wrote. This is the append-only
// convention in action.
func TestUpdateStatusSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Iron Man 3 (2013)", "Iron Man 3 (2013).manifest.json")

	initial := New(DiscTypeBluRay, "/dev/sr0")
	if err := Write(path, initial); err != nil {
		t.Fatalf("Write: %v", err)
	}

	index := 0
	steps := []struct {
		name   string
		mutate func(*Manifest) error
		want   Status
		verify func(t *testing.T, m *Manifest)
	}{
		{
			name: "identification confirmed",
			mutate: func(m *Manifest) error {
				m.Identification.Title = "Iron Man 3"
				m.Identification.Year = 2013
				m.Identification.Confirmed = true
				m.Status = StatusRipping
				return nil
			},
			want: StatusRipping,
			verify: func(t *testing.T, m *Manifest) {
				if !m.Identification.Confirmed {
					t.Error("Identification.Confirmed = false")
				}
			},
		},
		{
			name: "rip complete",
			mutate: func(m *Manifest) error {
				m.Tracks = append(m.Tracks, Track{
					MakeMKVIndex:    &index,
					Role:            RoleFeature,
					DurationSeconds: 7647,
					OutputFile:      "Iron Man 3 (2013).mkv",
				})
				m.Status = StatusAnalyzing
				return nil
			},
			want: StatusAnalyzing,
			verify: func(t *testing.T, m *Manifest) {
				if len(m.Tracks) != 1 {
					t.Errorf("len(Tracks) = %d, want 1", len(m.Tracks))
				}
			},
		},
		{
			name: "subtitle analysis",
			mutate: func(m *Manifest) error {
				m.Subtitles = append(m.Subtitles, Subtitle{StreamIndex: 7, NeedsOCR: true})
				m.Status = StatusPendingOCR
				return nil
			},
			want: StatusPendingOCR,
			verify: func(t *testing.T, m *Manifest) {
				if len(m.Subtitles) != 1 {
					t.Errorf("len(Subtitles) = %d, want 1", len(m.Subtitles))
				}
			},
		},
		{
			name: "radarr import",
			mutate: func(m *Manifest) error {
				m.Radarr = &Arr{Added: true, ImportTriggered: true}
				m.Status = StatusComplete
				return nil
			},
			want: StatusComplete,
			verify: func(t *testing.T, m *Manifest) {
				if m.Radarr == nil || !m.Radarr.ImportTriggered {
					t.Errorf("Radarr = %+v", m.Radarr)
				}
			},
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if err := Update(path, step.mutate); err != nil {
				t.Fatalf("Update: %v", err)
			}
			m, err := Read(path)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if m.Status != step.want {
				t.Errorf("Status = %q, want %q", m.Status, step.want)
			}
			// Everything the rip has learned so far must still be there.
			if m.ID != initial.ID {
				t.Errorf("ID = %q, want %q", m.ID, initial.ID)
			}
			if m.Disc.Device != "/dev/sr0" {
				t.Errorf("Disc.Device = %q", m.Disc.Device)
			}
			if m.Identification.Title != "Iron Man 3" {
				t.Errorf("Identification.Title = %q, lost by a later step", m.Identification.Title)
			}
			step.verify(t, m)
		})
	}

	// The last step's manifest carries every earlier step's data.
	final, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(final.Tracks) != 1 || len(final.Subtitles) != 1 || final.Radarr == nil {
		t.Errorf("final manifest lost data: %+v", final)
	}
}

// TestSetStatusAndAppendHelpers covers the convenience wrappers, including
// AddError's documented side effect of moving the manifest to StatusError.
func TestSetStatusAndAppendHelpers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rip.manifest.json")
	if err := Write(path, New(DiscTypeBluRay, "/dev/sr0")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := SetStatus(path, StatusRipping); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := AddWarning(path, "two titles within 10% of the feature duration"); err != nil {
		t.Fatalf("AddWarning: %v", err)
	}

	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Status != StatusRipping {
		t.Errorf("Status = %q, want %q — a warning must not change the status", m.Status, StatusRipping)
	}
	if len(m.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want one entry", m.Warnings)
	}

	if err := AddError(path, "makemkvcon exited 1"); err != nil {
		t.Fatalf("AddError: %v", err)
	}
	m, err = Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Status != StatusError {
		t.Errorf("Status = %q, want %q", m.Status, StatusError)
	}
	if len(m.Errors) != 1 || m.Errors[0] != "makemkvcon exited 1" {
		t.Errorf("Errors = %v", m.Errors)
	}
	if len(m.Warnings) != 1 {
		t.Errorf("Warnings = %v, want the earlier warning to survive", m.Warnings)
	}
}

// TestUpdateMutatorErrorLeavesManifestUnchanged checks a failing mutator is not
// half-applied to the file.
func TestUpdateMutatorErrorLeavesManifestUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rip.manifest.json")
	if err := Write(path, New(DiscTypeBluRay, "/dev/sr0")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}

	sentinel := errors.New("tmdb lookup failed")
	err = Update(path, func(m *Manifest) error {
		m.Status = StatusComplete
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Update error = %v, want it to wrap %v", err, sentinel)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("manifest changed despite the mutator failing\n before: %s\n  after: %s", before, after)
	}
	assertOnlyFile(t, dir, "rip.manifest.json")
}

// TestUpdateSerializesConcurrentWriters is the reason Update holds a lock across
// its read-modify-write: without it these appends would read the same manifest
// and clobber each other. One daemon process is the only writer, so a mutex is
// enough and no file locking is involved.
func TestUpdateSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rip.manifest.json")
	if err := Write(path, New(DiscTypeBluRay, "/dev/sr0")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := AddWarning(path, string(rune('a'+i))); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AddWarning: %v", err)
	}

	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(m.Warnings) != writers {
		t.Errorf("len(Warnings) = %d, want %d — an update was lost", len(m.Warnings), writers)
	}
}

// TestReadErrors checks both failure modes are distinguishable and name the file.
func TestReadErrors(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.manifest.json")
	if err := os.WriteFile(malformed, []byte(`{"id": "abc", "status":`), filePerm); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name       string
		path       string
		wantIs     error
		wantInText string
	}{
		{
			name:       "missing file",
			path:       filepath.Join(dir, "absent.manifest.json"),
			wantIs:     os.ErrNotExist,
			wantInText: "absent.manifest.json",
		},
		{
			name:       "malformed json",
			path:       malformed,
			wantInText: "parsing manifest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Read(tc.path)
			if err == nil {
				t.Fatalf("Read returned %+v, want an error", m)
			}
			if m != nil {
				t.Errorf("Read returned a manifest alongside the error: %+v", m)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tc.wantIs)
			}
			if !strings.Contains(err.Error(), tc.wantInText) {
				t.Errorf("error %q does not mention %q", err, tc.wantInText)
			}
		})
	}
}

// TestUpdateMissingFile checks Update surfaces Read's error rather than creating
// a manifest out of nothing.
func TestUpdateMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.manifest.json")
	err := Update(path, func(m *Manifest) error {
		m.Status = StatusComplete
		return nil
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Update error = %v, want it to wrap os.ErrNotExist", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("Update created the manifest it was supposed to be updating")
	}
}

func TestPathAndDir(t *testing.T) {
	tests := []struct {
		name     string
		root     string
		manifest *Manifest
		wantDir  string
		wantPath string
	}{
		{
			name: "bluray with year",
			root: "/media/library/movies",
			manifest: &Manifest{
				Disc:           Disc{Type: DiscTypeBluRay},
				Identification: Identification{Title: "Iron Man 3", Year: 2013},
			},
			wantDir:  "/media/library/movies/Iron Man 3 (2013)",
			wantPath: "/media/library/movies/Iron Man 3 (2013)/Iron Man 3 (2013).manifest.json",
		},
		{
			name: "bluray without a known year",
			root: "/media/library/movies",
			manifest: &Manifest{
				Disc:           Disc{Type: DiscTypeBluRay},
				Identification: Identification{Title: "Iron Man 3"},
			},
			wantDir:  "/media/library/movies/Iron Man 3",
			wantPath: "/media/library/movies/Iron Man 3/Iron Man 3.manifest.json",
		},
		{
			name: "bluray title containing a separator",
			root: "/media/library/movies",
			manifest: &Manifest{
				Disc:           Disc{Type: DiscTypeBluRay},
				Identification: Identification{Title: "Face/Off", Year: 1997},
			},
			wantDir:  "/media/library/movies/Face-Off (1997)",
			wantPath: "/media/library/movies/Face-Off (1997)/Face-Off (1997).manifest.json",
		},
		{
			name: "cd nests under the artist",
			root: "/media/library/music",
			manifest: &Manifest{
				Disc: Disc{Type: DiscTypeCD},
				Identification: Identification{
					Artist: "Jamestown Revival",
					Album:  "The Education of a Wandering Man",
					Year:   2014,
				},
			},
			wantDir:  "/media/library/music/Jamestown Revival/The Education of a Wandering Man",
			wantPath: "/media/library/music/Jamestown Revival/The Education of a Wandering Man/The Education of a Wandering Man.manifest.json",
		},
		{
			name: "cd without a known artist",
			root: "/media/library/music",
			manifest: &Manifest{
				Disc:           Disc{Type: DiscTypeCD},
				Identification: Identification{Album: "The Education of a Wandering Man"},
			},
			wantDir:  "/media/library/music/The Education of a Wandering Man",
			wantPath: "/media/library/music/The Education of a Wandering Man/The Education of a Wandering Man.manifest.json",
		},
		{
			name:     "unidentified disc falls back to the id",
			root:     "/media/library/movies",
			manifest: &Manifest{ID: "550e8400", Disc: Disc{Type: DiscTypeBluRay}},
			wantDir:  "/media/library/movies/550e8400",
			wantPath: "/media/library/movies/550e8400/550e8400.manifest.json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Dir(tc.root, tc.manifest); got != tc.wantDir {
				t.Errorf("Dir = %q, want %q", got, tc.wantDir)
			}
			if got := Path(tc.root, tc.manifest); got != tc.wantPath {
				t.Errorf("Path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// TestPathIsWritable ties the two halves together: the path Path builds is one
// Write can actually create, directories and all.
func TestPathIsWritable(t *testing.T) {
	m := New(DiscTypeBluRay, "/dev/sr0")
	m.Identification.Title = "Iron Man 3"
	m.Identification.Year = 2013

	root := t.TempDir()
	path := Path(root, m)
	if err := Write(path, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ID != m.ID {
		t.Errorf("ID = %q, want %q", got.ID, m.ID)
	}
	assertOnlyFile(t, Dir(root, m), "Iron Man 3 (2013).manifest.json")
}

// assertOnlyFile checks dir holds exactly the named entry — the check that no
// temp file survived.
func assertOnlyFile(t *testing.T, dir, want string) {
	t.Helper()
	if got := readDir(t, dir); len(got) != 1 || got[0] != want {
		t.Errorf("directory %s holds %v, want only %q", dir, got, want)
	}
}

func readDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

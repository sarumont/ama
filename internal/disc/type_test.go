package disc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mountLayout builds a directory tree standing in for a mounted volume. Each
// entry is a path relative to the root; one ending in "/" becomes a directory,
// anything else a file.
func mountLayout(t *testing.T, entries ...string) string {
	t.Helper()

	root := t.TempDir()
	for _, e := range entries {
		p := filepath.Join(root, strings.TrimSuffix(e, "/"))
		if strings.HasSuffix(e, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", p, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

func TestKindFromMount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []string
		want    DiscKind
	}{
		{
			name:    "BDMV at the root is a Blu-ray",
			entries: []string{"BDMV/", "BDMV/PLAYLIST/", "CERTIFICATE/"},
			want:    KindBluRay,
		},
		{
			name:    "VIDEO_TS at the root is a DVD",
			entries: []string{"VIDEO_TS/", "VIDEO_TS/VIDEO_TS.IFO", "AUDIO_TS/"},
			want:    KindDVD,
		},
		{
			name:    "a hybrid disc rips as the Blu-ray",
			entries: []string{"VIDEO_TS/", "BDMV/"},
			want:    KindBluRay,
		},
		{
			name:    "lowercased names still match",
			entries: []string{"bdmv/"},
			want:    KindBluRay,
		},
		{
			name:    "lowercased VIDEO_TS still matches",
			entries: []string{"video_ts/"},
			want:    KindDVD,
		},
		{
			name:    "a data disc is unknown",
			entries: []string{"backup/", "notes.txt"},
			want:    KindUnknown,
		},
		{
			name:    "an empty volume is unknown",
			entries: nil,
			want:    KindUnknown,
		},
		{
			name:    "a file named BDMV is not a Blu-ray",
			entries: []string{"BDMV"},
			want:    KindUnknown,
		},
		{
			name:    "BDMV nested below the root is not a Blu-ray",
			entries: []string{"disc1/BDMV/"},
			want:    KindUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := KindFromMount(mountLayout(t, tt.entries...)); got != tt.want {
				t.Errorf("KindFromMount(%v) = %v, want %v", tt.entries, got, tt.want)
			}
		})
	}
}

func TestKindFromMountSymlinkedDirectory(t *testing.T) {
	t.Parallel()

	root := mountLayout(t, "real/")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "BDMV")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if got := KindFromMount(root); got != KindBluRay {
		t.Errorf("KindFromMount with a symlinked BDMV = %v, want %v", got, KindBluRay)
	}
}

func TestKindFromMountUnreadableRoot(t *testing.T) {
	t.Parallel()

	if got := KindFromMount(filepath.Join(t.TempDir(), "absent")); got != KindUnknown {
		t.Errorf("KindFromMount on a missing root = %v, want %v", got, KindUnknown)
	}
}

// fakeContentChecker reports one scripted answer for every call.
type fakeContentChecker struct {
	content DiscContent
	err     error
}

func (f fakeContentChecker) Content(string) (DiscContent, error) {
	return f.content, f.err
}

func TestDetectKind(t *testing.T) {
	t.Parallel()

	errDrive := errors.New("drive busy")
	errMount := errors.New("wrong fs type")

	tests := []struct {
		name      string
		content   DiscContent
		checkErr  error
		entries   []string
		mountErr  error
		want      DiscKind
		wantErr   bool
		wantMount bool
	}{
		{
			name:    "audio disc is a CD without mounting",
			content: ContentAudio,
			want:    KindCD,
		},
		{
			name:    "mixed mode disc is a CD without mounting",
			content: ContentMixed,
			want:    KindCD,
		},
		{
			name:      "data disc with BDMV is a Blu-ray",
			content:   ContentData1,
			entries:   []string{"BDMV/"},
			want:      KindBluRay,
			wantMount: true,
		},
		{
			name:      "data disc with VIDEO_TS is a DVD",
			content:   ContentData1,
			entries:   []string{"VIDEO_TS/"},
			want:      KindDVD,
			wantMount: true,
		},
		{
			name:      "XA data disc is mounted like any other data disc",
			content:   ContentXA22,
			entries:   []string{"BDMV/"},
			want:      KindBluRay,
			wantMount: true,
		},
		{
			name:      "a mountable disc with neither directory is unknown, not an error",
			content:   ContentData1,
			entries:   []string{"stuff/"},
			want:      KindUnknown,
			wantMount: true,
		},
		{
			name:      "a drive reporting nothing still gets mounted",
			content:   ContentNoInfo,
			entries:   []string{"BDMV/"},
			want:      KindBluRay,
			wantMount: true,
		},
		{
			name:     "an unreadable drive is an error",
			checkErr: errDrive,
			want:     KindUnknown,
			wantErr:  true,
		},
		{
			name:      "an unmountable data disc is an error",
			content:   ContentData1,
			mountErr:  errMount,
			want:      KindUnknown,
			wantErr:   true,
			wantMount: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mounted, released := false, false
			mount := func(string) (string, func(), error) {
				mounted = true
				if tt.mountErr != nil {
					return "", nil, tt.mountErr
				}
				return mountLayout(t, tt.entries...), func() { released = true }, nil
			}

			checker := fakeContentChecker{content: tt.content, err: tt.checkErr}
			got, err := detectKind(testDevice, checker, mount)

			if got != tt.want {
				t.Errorf("kind = %v, want %v", got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.checkErr != nil && !errors.Is(err, tt.checkErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.checkErr)
			}
			if tt.mountErr != nil && !errors.Is(err, tt.mountErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.mountErr)
			}
			if mounted != tt.wantMount {
				t.Errorf("mounted = %v, want %v", mounted, tt.wantMount)
			}
			// Every successful mount must be released, or the daemon leaks a
			// mount point per disc and the drive will not eject.
			if wantRelease := tt.wantMount && tt.mountErr == nil; released != wantRelease {
				t.Errorf("released = %v, want %v", released, wantRelease)
			}
		})
	}
}

func TestDetectKindMissingDevice(t *testing.T) {
	t.Parallel()

	// A nil checker means the ioctl checker, which cannot open a device that
	// is not there.
	if _, err := DetectKind(testDevice, nil); err == nil {
		t.Error("DetectKind on a missing device returned no error")
	}
}

func TestIoctlContentCheckerMissingDevice(t *testing.T) {
	t.Parallel()

	if _, err := (IoctlContentChecker{}).Content(testDevice); err == nil {
		t.Error("Content on a missing device returned no error")
	}
}

func TestExistingMountAbsentDevice(t *testing.T) {
	t.Parallel()

	if root, ok := existingMount(testDevice); ok {
		t.Errorf("existingMount(%q) = %q, want no mount", testDevice, root)
	}
}

func TestDiscKindString(t *testing.T) {
	t.Parallel()

	for kind, want := range map[DiscKind]string{
		KindUnknown:  "unknown",
		KindBluRay:   "bluray",
		KindDVD:      "dvd",
		KindCD:       "cd",
		DiscKind(99): "unknown(99)",
	} {
		if got := kind.String(); got != want {
			t.Errorf("DiscKind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}

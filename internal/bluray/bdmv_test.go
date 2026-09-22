package bluray

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestReadDiscInfo(t *testing.T) {
	tests := []struct {
		name string
		// root is the disc root; when empty, rootFn builds one in a temp dir.
		root       string
		rootFn     func(t *testing.T) string
		wantTitle  string
		wantYear   *int
		wantLang   string
		wantSource string
		wantErr    error
	}{
		{
			name:       "english only",
			root:       "testdata/eng-only",
			wantTitle:  "Iron Man 3",
			wantLang:   "eng",
			wantSource: "bdmt_eng.xml",
		},
		{
			name:       "year embedded in title is extracted and trimmed",
			root:       "testdata/with-year",
			wantTitle:  "Blade Runner",
			wantYear:   ptr(1982),
			wantLang:   "eng",
			wantSource: "bdmt_eng.xml",
		},
		{
			name:       "multiple languages prefers english over the first file",
			root:       "testdata/multi-language",
			wantTitle:  "Spirited Away",
			wantLang:   "eng",
			wantSource: "bdmt_eng.xml",
		},
		{
			name:       "no english variant returns what the disc has",
			root:       "testdata/jpn-only",
			wantTitle:  "七人の侍",
			wantLang:   "jpn",
			wantSource: "bdmt_jpn.xml",
		},
		{
			name:       "unparseable english falls back to a valid sibling",
			root:       "testdata/mixed-validity",
			wantTitle:  "乱",
			wantLang:   "jpn",
			wantSource: "bdmt_jpn.xml",
		},
		{
			name:       "lowercase bdmv/meta/dl path",
			root:       "testdata/lowercase-path",
			wantTitle:  "The Fifth Element",
			wantLang:   "eng",
			wantSource: "bdmt_eng.xml",
		},
		{
			name:    "malformed xml",
			root:    "testdata/malformed",
			wantErr: ErrNoMetadata,
		},
		{
			name:    "metadata declares no title",
			root:    "testdata/untitled",
			wantErr: ErrNoMetadata,
		},
		{
			name:    "missing bdmv directory",
			rootFn:  func(t *testing.T) string { return t.TempDir() },
			wantErr: ErrNoMetadata,
		},
		{
			name: "missing meta/dl directory",
			rootFn: func(t *testing.T) string {
				root := t.TempDir()
				mkdirAll(t, filepath.Join(root, "BDMV", "PLAYLIST"))
				return root
			},
			wantErr: ErrNoMetadata,
		},
		{
			name: "empty meta/dl directory",
			rootFn: func(t *testing.T) string {
				root := t.TempDir()
				mkdirAll(t, filepath.Join(root, "BDMV", "META", "DL"))
				return root
			},
			wantErr: ErrNoMetadata,
		},
		{
			name: "meta/dl holds only non-xml files",
			rootFn: func(t *testing.T) string {
				root := t.TempDir()
				dl := filepath.Join(root, "BDMV", "META", "DL")
				mkdirAll(t, dl)
				writeFile(t, filepath.Join(dl, "bdmt_eng.jpg"), "not xml")
				return root
			},
			wantErr: ErrNoMetadata,
		},
		{
			name: "root does not exist",
			rootFn: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "no-such-disc")
			},
			wantErr: ErrNoMetadata,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.root
			if tt.rootFn != nil {
				root = tt.rootFn(t)
			}

			got, err := ReadDiscInfo(root)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ReadDiscInfo(%q) error = %v, want %v", root, err, tt.wantErr)
				}
				if got != nil {
					t.Errorf("ReadDiscInfo(%q) = %+v, want nil result alongside error", root, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadDiscInfo(%q) unexpected error: %v", root, err)
			}
			if got.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tt.wantTitle)
			}
			if !equalYear(got.Year, tt.wantYear) {
				t.Errorf("Year = %s, want %s", formatYear(got.Year), formatYear(tt.wantYear))
			}
			if got.Language != tt.wantLang {
				t.Errorf("Language = %q, want %q", got.Language, tt.wantLang)
			}
			if got.SourceFile != tt.wantSource {
				t.Errorf("SourceFile = %q, want %q", got.SourceFile, tt.wantSource)
			}
		})
	}
}

// TestReadDiscInfoErrorDetail checks that a parse failure survives on the
// ErrNoMetadata chain, so a caller that falls back to the disc label can still
// log why the metadata was unusable.
func TestReadDiscInfoErrorDetail(t *testing.T) {
	_, err := ReadDiscInfo("testdata/malformed")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("error = %v, want it to wrap ErrNoMetadata", err)
	}
	if !strings.Contains(err.Error(), "bdmt_eng.xml") {
		t.Errorf("error %q does not name the offending file", err)
	}
}

func TestSplitTrailingYear(t *testing.T) {
	tests := []struct {
		in        string
		wantTitle string
		wantYear  *int
	}{
		{"Iron Man 3", "Iron Man 3", nil},
		{"Blade Runner (1982)", "Blade Runner", ptr(1982)},
		{"Blade Runner  (1982)", "Blade Runner", ptr(1982)},
		{"Dune: Part Two (2024)", "Dune: Part Two", ptr(2024)},
		{"A Trip to the Moon (1902)", "A Trip to the Moon", ptr(1902)},
		// Not a year: leave the title untouched rather than guess.
		{"Blade Runner 2049", "Blade Runner 2049", nil},
		{"Ocean's Eleven (Remastered)", "Ocean's Eleven (Remastered)", nil},
		{"Apollo 13 (1995) Special Edition", "Apollo 13 (1995) Special Edition", nil},
		{"(2001)", "(2001)", nil},
		{"THX 1138 (1138)", "THX 1138 (1138)", nil},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			gotTitle, gotYear := splitTrailingYear(tt.in)
			if gotTitle != tt.wantTitle {
				t.Errorf("title = %q, want %q", gotTitle, tt.wantTitle)
			}
			if !equalYear(gotYear, tt.wantYear) {
				t.Errorf("year = %s, want %s", formatYear(gotYear), formatYear(tt.wantYear))
			}
		})
	}
}

func ptr(n int) *int { return &n }

func equalYear(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func formatYear(y *int) string {
	if y == nil {
		return "<nil>"
	}
	return strconv.Itoa(*y)
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

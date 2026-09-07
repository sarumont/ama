// Package bluray implements the Blu-ray side of the rip pipeline.
package bluray

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ErrNoMetadata reports that a disc carries no usable BDMV/META/DL metadata:
// the directory is absent or empty, every XML file in it failed to parse, or
// none of them declared a title.
//
// This is not a fatal condition. Plenty of discs — especially catalogue and
// pre-2010 titles — ship no META/DL at all, so callers are expected to test
// with errors.Is and fall back to the volume label as the TMDB search string.
// When a file was found but could not be parsed the parse failure is joined
// onto this sentinel, so the detail survives for logging.
var ErrNoMetadata = errors.New("bluray: no BDMV disc metadata")

// DiscInfo is the disc-level metadata read from BDMV/META/DL.
type DiscInfo struct {
	// Title is the disc title with surrounding whitespace trimmed. A trailing
	// parenthesised year is removed when one was present; see Year.
	Title string
	// Year is the release year, or nil when the metadata does not carry one.
	//
	// The disclib schema has no year element, so on the overwhelming majority
	// of discs this is nil. It is only non-nil when the authoring house wrote
	// the year into the title itself ("Blade Runner (1982)"), which some
	// catalogue reissues do.
	Year *int
	// Language is the ISO 639-2 code the file declared, e.g. "eng". Empty when
	// the file omits di:language.
	Language string
	// SourceFile is the base name of the XML the values came from, e.g.
	// "bdmt_eng.xml", recorded so a manifest can say where a title came from.
	SourceFile string
}

// ReadDiscInfo reads disc metadata from the Blu-ray mounted at root, where root
// is the directory containing BDMV (a mount point, or a fixture directory in
// tests).
//
// Discs may carry one metadata file per language. ReadDiscInfo prefers a file
// declaring English, then one named for English, then the alphabetically first
// file that parsed — so the result is deterministic and an English-only disc,
// a multi-language disc, and a Japanese-only disc all yield something usable.
//
// A disc with no metadata, or with only unparseable metadata, yields
// ErrNoMetadata rather than a hard failure.
func ReadDiscInfo(root string) (*DiscInfo, error) {
	dir, err := metaDLDir(root)
	if err != nil {
		return nil, err
	}

	names, err := xmlFileNames(dir)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: %s holds no XML files", ErrNoMetadata, dir)
	}

	var best *DiscInfo
	var bestRank int
	var parseErrs []error
	for _, name := range names {
		info, err := parseDiscInfoFile(filepath.Join(dir, name))
		if err != nil {
			parseErrs = append(parseErrs, err)
			continue
		}
		if rank := languageRank(info, name); best == nil || rank > bestRank {
			best, bestRank = info, rank
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w in %s: %w", ErrNoMetadata, dir, errors.Join(parseErrs...))
	}
	return best, nil
}

// metaDLDir locates BDMV/META/DL under root. Path components are matched
// case-insensitively: the spec says BDMV/META/DL, but the casing that actually
// reaches us depends on how the UDF volume was mounted or extracted.
func metaDLDir(root string) (string, error) {
	dir := root
	for _, component := range []string{"BDMV", "META", "DL"} {
		next, err := resolveChildDir(dir, component)
		if err != nil {
			return "", err
		}
		dir = next
	}
	return dir, nil
}

func resolveChildDir(parent, name string) (string, error) {
	if info, err := os.Stat(filepath.Join(parent, name)); err == nil && info.IsDir() {
		return filepath.Join(parent, name), nil
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %s does not exist", ErrNoMetadata, filepath.Join(parent, name))
		}
		return "", fmt.Errorf("bluray: reading %s: %w", parent, err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.EqualFold(entry.Name(), name) {
			return filepath.Join(parent, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("%w: %s does not exist", ErrNoMetadata, filepath.Join(parent, name))
}

// xmlFileNames returns the .xml file names in dir, sorted, so that selection
// among equally preferred languages is stable across runs and filesystems.
func xmlFileNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("bluray: reading %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".xml") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// languageRank scores a parsed file so English wins. A declared di:language
// beats a guess from the bdmt_{lang}.xml file name, which in turn beats
// nothing; equal ranks keep the first file in sorted order.
func languageRank(info *DiscInfo, name string) int {
	switch {
	case strings.EqualFold(info.Language, "eng"):
		return 2
	case info.Language == "" && strings.Contains(strings.ToLower(name), "eng"):
		return 1
	default:
		return 0
	}
}

// disclib mirrors the BDMV disc metadata document. Namespaces are deliberately
// left off the field tags: encoding/xml then matches on local name alone, which
// tolerates the namespace-prefix variations real discs ship with.
type disclib struct {
	XMLName  xml.Name `xml:"disclib"`
	DiscInfo discinfo `xml:"discinfo"`
}

type discinfo struct {
	Title    discTitle `xml:"title"`
	Language string    `xml:"language"`
}

type discTitle struct {
	Name string `xml:"name"`
}

func parseDiscInfoFile(path string) (*DiscInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bluray: reading %s: %w", path, err)
	}

	var doc disclib
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("bluray: parsing %s: %w", path, err)
	}

	title := strings.TrimSpace(doc.DiscInfo.Title.Name)
	if title == "" {
		return nil, fmt.Errorf("bluray: %s declares no disc title", path)
	}

	title, year := splitTrailingYear(title)
	return &DiscInfo{
		Title:      title,
		Year:       year,
		Language:   strings.TrimSpace(doc.DiscInfo.Language),
		SourceFile: filepath.Base(path),
	}, nil
}

// trailingYear matches a parenthesised four-digit year at the end of a title.
var trailingYear = regexp.MustCompile(`^(.*\S)\s*\((1[89]\d{2}|20\d{2})\)$`)

// splitTrailingYear peels a trailing "(1982)" off a title. The title is
// otherwise left alone — normalising it for matching is fuzzy.go's job.
func splitTrailingYear(title string) (string, *int) {
	m := trailingYear.FindStringSubmatch(title)
	if m == nil {
		return title, nil
	}
	year, err := strconv.Atoi(m[2])
	if err != nil {
		return title, nil
	}
	return m[1], &year
}

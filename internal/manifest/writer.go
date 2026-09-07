package manifest

// This file is the only way a manifest reaches or leaves the disk. Three rules
// shape it, all from docs/CLAUDE.md:
//
// # Writes are atomic
//
// A manifest is rewritten at every status transition, and a half-written file
// left by a crash or a container restart would destroy the authoritative record
// of a rip. Write therefore serializes into a temp file in the destination's own
// directory (so the rename never crosses a filesystem), syncs it, and renames it
// over the destination. A reader sees either the entire old manifest or the
// entire new one, never a partial write, and a failure anywhere before the
// rename leaves the destination untouched and no temp file behind.
//
// # The manifest is append-only
//
// "Update fields, never delete them." Nothing in Go can enforce this at the type
// level, so it is a convention every caller — main.go, the web handlers, the
// *arr clients — must keep: a mutator passed to Update may set a field that is
// still unset, refine one it owns, and append to Warnings and Errors, but must
// never clear a field another step already populated or truncate those two
// slices. AddWarning and AddError are the only supported way to grow them.
//
// # One writer, one disc at a time
//
// "No background queue — one disc at a time; the drive is the queue." AMA runs
// as a single daemon process, so it is the only writer of any manifest and no
// cross-process file locking is needed. Update still guards its read-modify-write
// with a per-path mutex, because that one process is concurrent internally: an
// HTTP handler and the rip pipeline can both reach for the same manifest, and
// without the mutex one of the two writes would be silently lost.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// dirPerm and filePerm are the modes for the rip directory and the manifest.
// os.CreateTemp makes the temp file 0600, so Write widens it before the rename;
// the manifest is meant to be readable by whatever tooling consumes the library.
const (
	dirPerm  os.FileMode = 0o755
	filePerm os.FileMode = 0o644
)

// locks holds one mutex per manifest path, keyed by its cleaned path. Entries
// are never evicted: a daemon touches one manifest per disc, so the map grows by
// one small entry per rip and is not worth the complexity of reference counting.
var locks sync.Map

// lockFor returns the mutex guarding path, creating it on first use.
func lockFor(path string) *sync.Mutex {
	mu, _ := locks.LoadOrStore(filepath.Clean(path), &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// Write serializes m to path atomically, creating the parent directory if it is
// missing. Callers that are modifying an existing manifest should use Update
// instead, which reads and writes under one lock.
func Write(path string, m *Manifest) error {
	mu := lockFor(path)
	mu.Lock()
	defer mu.Unlock()
	return write(path, m)
}

// write is Write without the lock, for callers that already hold it.
func write(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding manifest for %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating manifest directory %s: %w", dir, err)
	}

	// The temp file must share a directory with the destination for the rename
	// to be atomic, and the leading dot keeps it out of the way of anything
	// globbing the rip directory if a hard kill lands between the two.
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp manifest in %s: %w", dir, err)
	}
	// After a successful rename this name is gone and Remove is a no-op; on
	// every failure path it is what keeps temp files from accumulating.
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp manifest %s: %w", tmp.Name(), err)
	}
	// Sync before the rename so the rename cannot expose a file whose contents
	// have not reached the disk yet.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp manifest %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp manifest %s: %w", tmp.Name(), err)
	}
	if err := os.Chmod(tmp.Name(), filePerm); err != nil {
		return fmt.Errorf("setting mode on temp manifest %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("renaming temp manifest over %s: %w", path, err)
	}
	return nil
}

// Read loads the manifest at path. A missing file is reported with os.ErrNotExist
// wrapped, so callers can distinguish "no rip here" from a corrupt manifest with
// errors.Is.
func Read(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w", path, err)
	}
	return &m, nil
}

// Update applies fn to the manifest at path and writes the result atomically.
//
// This is the only supported way to change a manifest that already exists. It
// exists so no caller can accidentally read, sit on the result, and write it
// back later over someone else's changes: the read, the mutation and the write
// all happen under one lock. If fn returns an error the manifest is left exactly
// as it was.
//
// fn must respect the append-only convention described at the top of this file.
func Update(path string, fn func(*Manifest) error) error {
	mu := lockFor(path)
	mu.Lock()
	defer mu.Unlock()

	m, err := Read(path)
	if err != nil {
		return err
	}
	if err := fn(m); err != nil {
		return fmt.Errorf("updating manifest %s: %w", path, err)
	}
	return write(path, m)
}

// SetStatus records a lifecycle transition. It is the update point for each of
// the four documented transitions when nothing else changes; when fields are
// populated at the same time — tracks after a rip, subtitles after analysis —
// set them and the status together in a single Update so the manifest is never
// on disk claiming a status whose data has not landed yet.
func SetStatus(path string, status Status) error {
	return Update(path, func(m *Manifest) error {
		m.Status = status
		return nil
	})
}

// AddWarning appends a non-fatal note to the manifest. The rip continues and the
// status is left alone.
func AddWarning(path, warning string) error {
	return Update(path, func(m *Manifest) error {
		m.Warnings = append(m.Warnings, warning)
		return nil
	})
}

// AddError appends a failure and moves the manifest to StatusError, because
// docs/MANIFEST.md defines that status as "one or more errors; see errors
// array" — the two always travel together. Recoverable problems belong in
// AddWarning, and a per-subtitle OCR failure belongs in that subtitle's
// ConversionError field.
func AddError(path, message string) error {
	return Update(path, func(m *Manifest) error {
		m.Errors = append(m.Errors, message)
		m.Status = StatusError
		return nil
	})
}

// Path returns where m's manifest belongs under the library root: the rip
// directory from Dir plus the shared base name, as documented in
// docs/MANIFEST.md "Location".
//
//	Path("/media/library/movies", ironMan3) // .../Iron Man 3 (2013)/Iron Man 3 (2013).manifest.json
//	Path("/media/library/music", wanderingMan) // .../Jamestown Revival/The Education of a Wandering Man/The Education of a Wandering Man.manifest.json
//
// root is the matching output root from the config — output.movies for a
// Blu-ray, output.music for a CD.
func Path(root string, m *Manifest) string {
	return filepath.Join(Dir(root, m), name(m)+".manifest.json")
}

// Dir returns the directory a rip's output belongs in: "{root}/{Title} ({Year})"
// for a Blu-ray and "{root}/{Artist}/{Album}" for a CD, per the two examples in
// docs/MANIFEST.md. It is exported so the rip code names the directory the same
// way the manifest does rather than rebuilding the convention.
func Dir(root string, m *Manifest) string {
	if m.Disc.Type == DiscTypeCD {
		if artist := sanitize(m.Identification.Artist); artist != "" {
			return filepath.Join(root, artist, name(m))
		}
	}
	return filepath.Join(root, name(m))
}

// name is the base every file in a rip shares — "Iron Man 3 (2013)" for a
// Blu-ray, the album title for a CD. docs/MANIFEST.md shows no manifest file
// name for a CD, so it follows the Blu-ray convention of "{base}.manifest.json"
// inside the album directory.
//
// A manifest that has not been identified yet has no title, and joining an empty
// one would put the rip directly in the library root; such a manifest falls back
// to its UUID. In practice this does not happen: the first documented write is
// after identification is confirmed.
func name(m *Manifest) string {
	if m.Disc.Type == DiscTypeCD {
		if album := sanitize(m.Identification.Album); album != "" {
			return album
		}
		return m.ID
	}
	title := sanitize(m.Identification.Title)
	if title == "" {
		return m.ID
	}
	if m.Identification.Year > 0 {
		return fmt.Sprintf("%s (%d)", title, m.Identification.Year)
	}
	return title
}

// sanitize makes a title safe as a single path element. A separator is the only
// character that would silently change what the path means — "Face/Off" would
// otherwise become a directory named "Face" — so it is the only one replaced;
// everything else is left as the metadata source spelled it.
func sanitize(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "/", "-"))
}

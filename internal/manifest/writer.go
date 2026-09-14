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
// slices. Call the Manifest.AddWarning/Manifest.AddError methods on the
// manifest fn is given to grow them — never the package-level AddWarning,
// AddError, SetStatus, Update or Write for the same path, all of which take
// the lock Update is already holding and will deadlock the goroutine (and,
// since the lock is never released, every later Update or Write on that path
// too).
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

// lockFor returns the mutex guarding path, creating it on first use. The key
// is the absolute path rather than just the cleaned one: filepath.Clean is
// purely lexical, so a relative and an absolute spelling of the same file —
// or two paths that reach it through a symlinked root — would otherwise land
// on different mutexes and silently defeat Update's lost-update guarantee.
func lockFor(path string) *sync.Mutex {
	key, err := filepath.Abs(path)
	if err != nil {
		key = filepath.Clean(path)
	}
	mu, _ := locks.LoadOrStore(key, &sync.Mutex{})
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
	// A nil m marshals to the JSON literal `null` via Manifest's MarshalJSON
	// (which has a value receiver, so *Manifest satisfies json.Marshaler and
	// encoding/json short-circuits a nil pointer to "null" instead of
	// erroring). That would silently overwrite the authoritative record of a
	// rip, and Read of the result comes back as a non-nil zero Manifest with
	// no error, so the caller has nothing to detect the loss with. Reject it
	// here instead.
	if m == nil {
		return fmt.Errorf("writing manifest %s: nil manifest", path)
	}

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
// fn must respect the append-only convention described at the top of this file,
// using the Manifest.AddWarning/Manifest.AddError methods to grow Warnings and
// Errors. fn must not call the package-level AddWarning, AddError, SetStatus,
// Update or Write for path — those take the same lock Update is already
// holding for the duration of fn and will deadlock the goroutine.
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

// AddWarning appends a non-fatal note to the manifest at path as its own
// Update. The rip continues and the status is left alone.
//
// Call this from top-level rip/handler code only. A mutator already running
// inside an Update for path must call the Manifest.AddWarning method on the
// manifest it was given instead — this function takes the same per-path lock
// Update holds and would deadlock.
func AddWarning(path, warning string) error {
	return Update(path, func(m *Manifest) error {
		m.AddWarning(warning)
		return nil
	})
}

// AddWarning appends a non-fatal note to m. Unlike the package-level
// AddWarning, this does not read, lock or write anything — it is the form to
// call from within a mutator passed to Update, on the manifest that mutator
// was given.
func (m *Manifest) AddWarning(warning string) {
	m.Warnings = append(m.Warnings, warning)
}

// AddError appends a failure to the manifest at path and moves it to
// StatusError, as its own Update, because docs/MANIFEST.md defines that
// status as "one or more errors; see errors array" — the two always travel
// together. Recoverable problems belong in AddWarning, and a per-subtitle OCR
// failure belongs in that subtitle's ConversionError field.
//
// Call this from top-level rip/handler code only. A mutator already running
// inside an Update for path must call the Manifest.AddError method on the
// manifest it was given instead — this function takes the same per-path lock
// Update holds and would deadlock.
func AddError(path, message string) error {
	return Update(path, func(m *Manifest) error {
		m.AddError(message)
		return nil
	})
}

// AddError appends a failure to m and moves it to StatusError. Unlike the
// package-level AddError, this does not read, lock or write anything — it is
// the form to call from within a mutator passed to Update, on the manifest
// that mutator was given.
func (m *Manifest) AddError(message string) {
	m.Errors = append(m.Errors, message)
	m.Status = StatusError
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
		return fallbackID(m)
	}
	title := sanitize(m.Identification.Title)
	if title == "" {
		return fallbackID(m)
	}
	if m.Identification.Year > 0 {
		return fmt.Sprintf("%s (%d)", title, m.Identification.Year)
	}
	return title
}

// fallbackID returns m.ID, generating and storing a fresh UUID first if it is
// empty. m.ID is only guaranteed non-empty for a manifest built by New; one
// built directly, or read back from a document with no "id" key, has
// ID == "". Falling back to an empty string would defeat the point of this
// fallback — Dir and Path would then resolve to the library root itself, and
// every such manifest would collide on the same hidden ".manifest.json"
// there — so a missing ID is filled in on first use instead.
func fallbackID(m *Manifest) string {
	if m.ID == "" {
		m.ID = newUUID()
	}
	return m.ID
}

// sanitize makes a title safe as a single path element. A separator is the
// only character that would silently change what the path means by itself —
// "Face/Off" would otherwise become a directory named "Face" — so it is the
// only one replaced; everything else is left as the metadata source spelled
// it. "." and ".." are rejected outright instead: filepath.Join cleans them
// away rather than preserving them, so a title of ".." would otherwise walk
// the rip directory one level above the library root.
func sanitize(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "/", "-"))
	if s == "." || s == ".." {
		return ""
	}
	return s
}

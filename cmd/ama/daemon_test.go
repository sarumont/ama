package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sarumont/ama/internal/manifest"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRelocate(t *testing.T) {
	root := t.TempDir()

	m := manifest.New(manifest.DiscTypeBluRay, "/dev/sr0")
	oldPath := manifest.Path(root, m)
	if err := manifest.Write(oldPath, m); err != nil {
		t.Fatalf("writing initial manifest: %v", err)
	}

	// Confirmation sets the fields Dir/Path key on.
	m.Identification.Title = "Iron Man 3"
	m.Identification.Year = 2013

	newPath, err := relocate(root, m, oldPath)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	wantPath := filepath.Join(root, "Iron Man 3 (2013)", "Iron Man 3 (2013).manifest.json")
	if newPath != wantPath {
		t.Errorf("newPath = %q, want %q", newPath, wantPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("manifest missing at new path: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old manifest still exists at %q: %v", oldPath, err)
	}
	if _, err := os.Stat(filepath.Dir(oldPath)); !os.IsNotExist(err) {
		t.Errorf("old (now-empty) directory still exists: %v", err)
	}
}

func TestRelocateNoOpWhenPathUnchanged(t *testing.T) {
	root := t.TempDir()
	m := manifest.New(manifest.DiscTypeCD, "/dev/sr0")
	path := manifest.Path(root, m)
	if err := manifest.Write(path, m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}

	// No identifying fields changed, so Dir/Path resolve the same way.
	got, err := relocate(root, m, path)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if got != path {
		t.Errorf("relocate() = %q, want unchanged %q", got, path)
	}
}

func TestWaitForConfirmation(t *testing.T) {
	root := t.TempDir()
	m := manifest.New(manifest.DiscTypeBluRay, "/dev/sr0")
	path := manifest.Path(root, m)
	if err := manifest.Write(path, m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}

	d := &daemon{log: newTestLogger()}

	confirmed := make(chan struct{})
	go func() {
		<-time.After(20 * time.Millisecond)
		_ = manifest.Update(path, func(mm *manifest.Manifest) error {
			mm.Identification.Confirmed = true
			return nil
		})
		close(confirmed)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := d.waitForConfirmation(ctx, path)
	<-confirmed
	if err != nil {
		t.Fatalf("waitForConfirmation: %v", err)
	}
	if !got.Identification.Confirmed {
		t.Error("returned manifest is not confirmed")
	}
}

func TestWaitForConfirmationCancelled(t *testing.T) {
	root := t.TempDir()
	m := manifest.New(manifest.DiscTypeBluRay, "/dev/sr0")
	path := manifest.Path(root, m)
	if err := manifest.Write(path, m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}

	d := &daemon{log: newTestLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := d.waitForConfirmation(ctx, path); err == nil {
		t.Error("waitForConfirmation with a cancelled context: want an error, got nil")
	}
}

func TestRecordErrorAndWarning(t *testing.T) {
	root := t.TempDir()
	m := manifest.New(manifest.DiscTypeCD, "/dev/sr0")
	path := manifest.Path(root, m)
	if err := manifest.Write(path, m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	log := newTestLogger()

	recordWarning(path, log, "something odd but survivable")
	got, err := manifest.Read(path)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "something odd but survivable" {
		t.Errorf("warnings = %v, want [\"something odd but survivable\"]", got.Warnings)
	}
	if got.Status == manifest.StatusError {
		t.Error("a warning must not flip status to error")
	}

	recordError(path, log, "ripping failed", os.ErrClosed)
	got, err = manifest.Read(path)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	if len(got.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one", got.Errors)
	}
	if got.Status != manifest.StatusError {
		t.Errorf("status = %q, want %q", got.Status, manifest.StatusError)
	}
}

func TestFinishDoesNotOverwriteError(t *testing.T) {
	root := t.TempDir()
	m := manifest.New(manifest.DiscTypeCD, "/dev/sr0")
	path := manifest.Path(root, m)
	if err := manifest.Write(path, m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	log := newTestLogger()
	recordError(path, log, "ripping failed", os.ErrClosed)

	d := &daemon{log: log}
	d.finish(path)

	got, err := manifest.Read(path)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	if got.Status != manifest.StatusError {
		t.Errorf("status = %q, want finish() to leave an errored manifest alone", got.Status)
	}
}

func TestFinishMarksComplete(t *testing.T) {
	root := t.TempDir()
	m := manifest.New(manifest.DiscTypeCD, "/dev/sr0")
	path := manifest.Path(root, m)
	if err := manifest.Write(path, m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}

	d := &daemon{log: newTestLogger()}
	d.finish(path)

	got, err := manifest.Read(path)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	if got.Status != manifest.StatusComplete {
		t.Errorf("status = %q, want %q", got.Status, manifest.StatusComplete)
	}
	if got.RippedAt.IsZero() {
		t.Error("ripped_at was not set")
	}
}

package testutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ExitError reports a fixture recorded with a non-zero exit status. It mirrors
// exec.ExitError closely enough that production code can treat both alike.
type ExitError struct {
	Code   int
	Stderr []byte
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.Code)
}

// fixture is one recorded invocation.
type fixture struct {
	dir      string
	stdout   []byte
	stderr   []byte
	exitCode int
}

// FixtureRunner replays recorded external-tool output. It implements both
// [Runner] and [CaptureRunner], and fails the test on any invocation it has no
// recording for, so a silently changed command line cannot pass as a no-op.
type FixtureRunner struct {
	t        testing.TB
	fixtures map[string]fixture
}

// NewFixtureRunner indexes every fixture directory under dir — see the package
// doc for the layout — keyed by the argv in its cmd file.
func NewFixtureRunner(t testing.TB, dir string) *FixtureRunner {
	t.Helper()

	r := &FixtureRunner{t: t, fixtures: make(map[string]fixture)}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		argv, err := readCmd(filepath.Join(path, "cmd"))
		if err != nil || argv == nil {
			return err
		}
		f, err := readFixture(path)
		if err != nil {
			return err
		}
		key := Key(argv[0], argv[1:]...)
		if prev, ok := r.fixtures[key]; ok {
			return fmt.Errorf("%s and %s both record %q", prev.dir, path, key)
		}
		r.fixtures[key] = f
		return nil
	})
	if err != nil {
		t.Fatalf("testutil: loading fixtures from %s: %v", dir, err)
	}
	if len(r.fixtures) == 0 {
		t.Fatalf("testutil: no fixtures under %s (a fixture directory must contain a cmd file)", dir)
	}
	return r
}

// Run replays the recording for this invocation, reporting a non-zero recorded
// exit status as an [ExitError] carrying the recorded stderr.
func (r *FixtureRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.t.Helper()
	stdout, _, err := r.RunCapture(ctx, name, args...)
	return stdout, err
}

// RunCapture replays the recording, returning stdout and stderr separately.
func (r *FixtureRunner) RunCapture(_ context.Context, name string, args ...string) (stdout, stderr []byte, err error) {
	r.t.Helper()

	key := Key(name, args...)
	f, ok := r.fixtures[key]
	if !ok {
		r.t.Fatalf("testutil: no fixture for %q\nrecorded invocations:\n  %s", key, strings.Join(r.keys(), "\n  "))
		return nil, nil, nil
	}
	if f.exitCode != 0 {
		return f.stdout, f.stderr, &ExitError{Code: f.exitCode, Stderr: f.stderr}
	}
	return f.stdout, f.stderr, nil
}

// keys lists the recorded invocations, sorted, for failure messages.
func (r *FixtureRunner) keys() []string {
	keys := make([]string, 0, len(r.fixtures))
	for k := range r.fixtures {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// readCmd parses a cmd file into argv: one token per line, blank lines and
// "#" comments ignored. It returns a nil argv when the file does not exist, so
// non-fixture directories are simply skipped.
func readCmd(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var argv []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		argv = append(argv, line)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s lists no command", path)
	}
	return argv, nil
}

// readFixture reads the stdout, stderr, and exitcode files of a fixture
// directory. Only stdout is required.
func readFixture(dir string) (fixture, error) {
	f := fixture{dir: dir}

	var err error
	if f.stdout, err = os.ReadFile(filepath.Join(dir, "stdout")); err != nil {
		return f, err
	}
	if f.stderr, err = os.ReadFile(filepath.Join(dir, "stderr")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}

	raw, err := os.ReadFile(filepath.Join(dir, "exitcode"))
	if errors.Is(err, fs.ErrNotExist) {
		return fixture{dir: f.dir, stdout: f.stdout, stderr: f.stderr}, nil
	}
	if err != nil {
		return f, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return f, fmt.Errorf("%s/exitcode: %w", dir, err)
	}
	f.exitCode = code
	return f, nil
}

// LoadFixture reads a fixture file, joining the path segments and failing the
// test with a message naming the missing file. Use it for captures a test parses
// directly, where no command invocation is involved.
func LoadFixture(t testing.TB, segments ...string) []byte {
	t.Helper()

	path := filepath.Join(segments...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testutil: reading fixture %s: %v", path, err)
	}
	return data
}

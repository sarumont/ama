package testutil

import (
	"context"
	"fmt"
	"strings"
)

// Runner is the seam AMA packages use to invoke an external tool. Production
// code wraps exec.CommandContext; tests substitute [FakeRunner] or
// [FixtureRunner].
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// CaptureRunner is the Runner variant for tools whose stderr is parsed rather
// than merely reported — whipper writes its rip progress there, for example.
type CaptureRunner interface {
	RunCapture(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// Key is the canonical lookup key for an invocation: the command name and its
// arguments joined by single spaces. Fixture and fake lookups both use it, so a
// test can name an expected call the way it would be typed in a shell.
func Key(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// FakeRunner is a Runner returning canned output, for tests whose sample data is
// small enough to live inline. Responses is consulted first, keyed by [Key]; if
// the invocation is not listed, Sequence supplies output by call order. An
// invocation matching neither returns an error, and Err short-circuits every
// call for testing failure paths. Prefer [FixtureRunner] once the sample data
// grows past a few lines.
//
// A FakeRunner is not safe for concurrent use.
type FakeRunner struct {
	// Responses maps Key(name, args...) to that invocation's stdout.
	Responses map[string][]byte

	// Sequence supplies stdout by call order for invocations absent from
	// Responses: the first unmatched call gets Sequence[0], and so on.
	Sequence [][]byte

	// Err, when set, is returned by every call instead of any output.
	Err error

	// Calls records every invocation in order, as [Key] renders it.
	Calls []string

	// seq counts how much of Sequence has been consumed.
	seq int
}

// Run records the invocation and returns its canned output.
func (r *FakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := Key(name, args...)
	r.Calls = append(r.Calls, key)

	if r.Err != nil {
		return nil, r.Err
	}
	if out, ok := r.Responses[key]; ok {
		return out, nil
	}
	if r.seq < len(r.Sequence) {
		out := r.Sequence[r.seq]
		r.seq++
		return out, nil
	}
	return nil, fmt.Errorf("testutil: FakeRunner has no response for %q (call %d)", key, len(r.Calls))
}

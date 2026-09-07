// Package testutil provides the shared harness AMA uses to test code that
// shells out to external tools (makemkvcon, ffprobe, whipper, mkvmerge,
// tesseract) without a real optical drive, a MakeMKV license, or a network.
//
// # The seam
//
// Every package that runs an external tool depends on a small interface rather
// than calling exec.Command directly:
//
//	type Runner interface {
//		Run(ctx context.Context, name string, args ...string) ([]byte, error)
//	}
//
// Production code holds a Runner field, defaulting to an os/exec-backed
// implementation the package owns:
//
//	type execRunner struct{}
//
//	func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
//		return exec.CommandContext(ctx, name, args...).Output()
//	}
//
// Tests swap in [FakeRunner] (canned bytes, written inline in a table-driven
// test) or [FixtureRunner] (recorded captures on disk). Use CaptureRunner
// instead of Runner when a tool's stderr is part of the output being parsed.
//
// # Fixture layout
//
// A fixture is a directory holding one recorded invocation:
//
//	internal/bluray/testdata/makemkvcon/iron-man-3/
//	    cmd        argv that produced the capture, one token per line
//	    stdout     recorded stdout (required)
//	    stderr     recorded stderr (optional, defaults to empty)
//	    exitcode   recorded exit status (optional, defaults to 0)
//
// The cmd file makes a capture auditable and regenerable. Lines starting with
// "#" are comments and are the right place to record provenance: the tool
// version, the disc, the date, and whether the capture was hand-constructed.
//
// [FixtureRunner] indexes a directory tree by the argv in each cmd file and
// fails the test on any invocation it does not recognise, so a silently changed
// command line cannot pass as a no-op.
//
// # Recording a new fixture
//
// Run the tool with stdout and stderr captured separately, then scrub the
// capture before committing it:
//
//	mkdir -p internal/bluray/testdata/makemkvcon/iron-man-3
//	cd internal/bluray/testdata/makemkvcon/iron-man-3
//	makemkvcon -r info disc:0 >stdout 2>stderr; echo $? >exitcode
//	printf '%s\n' '# makemkvcon v1.17.9, 2026-08-21' makemkvcon -r info disc:0 >cmd
//
// Scrub license keys, API tokens, home directory paths, and anything that
// identifies a specific physical disc copy before the capture is committed.
// Fixtures must stay plain text and diff-reviewable; never commit media files.
// If a test needs an MKV, generate a tiny synthetic one at test time.
//
// testdata/README.md at the repository root holds the full contributor guide:
// per-tool recording commands, redaction rules, and naming conventions.
// Reference captures demonstrating the layout live in
// internal/testutil/testdata/examples.
package testutil

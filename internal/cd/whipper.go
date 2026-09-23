// Package cd wraps whipper, the external CLI AMA drives to rip a CD with
// AccurateRip verification.
//
// whipper.go covers the whipper subprocess itself: two entry points mirroring
// docs/ARCHITECTURE.md's CD flow -- Identify reads the disc's TOC and returns
// the disc ID plus candidate releases, and Rip drives the user-confirmed
// release through whipper's actual extraction. log.go covers the second half
// of Rip: parsing the YAML .log file whipper writes once ripping finishes.
//
// A research spike on issue #9 (see the issue's comments) settled the design
// questions this package answers:
//
//   - whipper is driven strictly non-interactively. -p/--prompt is never
//     passed, and the subprocess's stdin is left nil (-> /dev/null) as a
//     belt-and-braces guard: if some future whipper regression ever reaches
//     its one input() call, it gets EOF and fails loudly instead of hanging
//     AMA forever.
//   - `whipper cd info` is used only to obtain the disc ID (and to confirm a
//     CD is actually present). Its own "Matching releases:" text is never
//     scraped; internal/musicbrainz does that lookup independently over
//     plain HTTPS, in Go, so web/confirm.html gets typed JSON instead of a
//     CLI screen-scrape.
//   - Rip output is parsed from the YAML .log file whipper writes (a
//     ruamel.yaml round-trip dump), never from stdout text.
//   - XDG_CONFIG_HOME is pointed at a directory this package owns for every
//     whipper invocation, so a stray host whipper.conf cannot silently
//     re-enable prompting -- or override the drive's read offset -- through
//     argparse's config-file-supplies-defaults mechanism.
//
// This package deliberately does not import internal/manifest: whipper and
// MusicBrainz are the source of the raw data, and mapping it onto the
// manifest belongs to the layer above (see internal/radarr/client.go's
// package doc for the same rule applied to Radarr/Sonarr).
package cd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sarumont/ama/internal/musicbrainz"
)

// DefaultBinary is the whipper executable, looked up on PATH.
const DefaultBinary = "whipper"

// ExpectedVersion is the whipper release this package's argument list, exit
// code table, and .log YAML shape were validated against during the issue #9
// spike (see the issue's comments for the full research). Pinning the
// Dockerfile's installed whipper to this exact version -- not a range, so an
// upstream change cannot silently alter behaviour underneath this wrapper --
// is issue #52's job. If that pin ever moves, re-validate this file against
// the new release's CHANGELOG before bumping this constant.
const ExpectedVersion = "0.10.0"

// track- and disc-templates whipper lays the rip out with. %A is artist, %d
// is disc (album) title, %t is the zero-padded track number, %n is track
// title. Together they produce docs/MANIFEST.md's documented CD layout:
// {artist}/{album}/{NN} - {title}.flac.
const (
	trackTemplate = "%A/%d/%t - %n"
	discTemplate  = "%A/%d/%A - %d"

	// loggerPlugin selects whipper's own built-in "whipper" result logger
	// (-L), the one that writes the YAML .log this package parses. It is not
	// the name of an AMA-authored plugin.
	loggerPlugin = "whipper"
)

// whipper's documented exit codes (see the issue #9 spike, which cites
// whipper/command/main.py and command/cd.py).
const (
	exitSuccess      = 0
	exitRuntimeError = 1 // real failure: CRC mismatch, unrippable track, ...
	exitPartialSkip  = 5 // finished, but -k let some tracks be skipped
	exitSystemError  = 255
)

// ErrNotInstalled reports that the whipper executable could not be found.
var ErrNotInstalled = errors.New("cd: whipper not found in PATH")

// CommandRunner runs whipper to completion and returns its captured stdout
// and stderr. It has exactly [internal/testutil.CaptureRunner]'s shape, so
// tests can pass a *testutil.FakeRunner or *testutil.FixtureRunner directly.
//
// whipper's own progress reporting -- a single cdparanoia
// carriage-return-updated meter on stderr, not structured per-record output
// like MakeMKV's PRGC/PRGV -- carries little structure worth streaming
// line-by-line, and a CD rip runs for minutes rather than the multi-hour
// Blu-ray rips that justified bluray.CommandRunner's live callback. A
// capture-then-parse Runner is simpler and sufficient for v1; see
// internal/testutil/runner.go's CaptureRunner doc, which was written with
// this exact tool in mind. What the web UI queue view shows while a rip is
// in progress comes from the manifest's "ripping" status plus the track list
// Identify already returned, not from live per-line parsing.
type CommandRunner interface {
	RunCapture(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// ExecRunner is the real CommandRunner, running whipper as a subprocess.
type ExecRunner struct {
	// ConfigHome is set as the subprocess's XDG_CONFIG_HOME, created if
	// missing, so whipper never reads a host's own
	// ~/.config/whipper/whipper.conf -- see the package doc for why that
	// matters. Empty means DefaultConfigHome().
	ConfigHome string
}

// RunCapture implements CommandRunner. The subprocess is killed when ctx is
// cancelled, which is what stops a rip; a cancelled ctx is reported as its
// own Err() rather than as whatever exec makes of a killed process, so
// callers can tell a cancelled rip from a genuine failure with errors.Is.
func (r ExecRunner) RunCapture(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error) {
	configHome := r.ConfigHome
	if configHome == "" {
		configHome = DefaultConfigHome()
	}
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		return nil, nil, fmt.Errorf("cd: creating whipper XDG_CONFIG_HOME %s: %w", configHome, err)
	}

	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+configHome)
	// cmd.Stdin is deliberately left nil: Go hands the child /dev/null, so
	// if whipper ever reaches its one input() call (guarded by --prompt,
	// which this package never passes) it gets EOF and fails loudly instead
	// of hanging AMA forever.

	if runErr := cmd.Run(); runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return outBuf.Bytes(), errBuf.Bytes(), fmt.Errorf("%w: %v", ctxErr, runErr)
		}
		return outBuf.Bytes(), errBuf.Bytes(), runErr
	}
	return outBuf.Bytes(), errBuf.Bytes(), nil
}

// DefaultConfigHome is the XDG_CONFIG_HOME ExecRunner points whipper at when
// ConfigHome is unset: a directory under os.TempDir() that this package
// creates and owns, never a host $HOME. It intentionally holds no
// whipper.conf of its own, so whipper falls back to its own safe
// (non-prompting) argparse defaults rather than any host configuration --
// see the package doc.
func DefaultConfigHome() string {
	return filepath.Join(os.TempDir(), "ama-whipper-xdg-config")
}

// CommandError reports a whipper invocation that failed to run or exited
// with a status this package treats as a hard failure, with whatever it
// wrote to stderr.
type CommandError struct {
	Op     string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("whipper: %s: %v", e.Op, e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func (e *CommandError) Unwrap() error { return e.Err }

// exitCode extracts the process's exit status from err, if err's chain
// carries one -- true both for a real *exec.ExitError and for the
// *testutil.ExitError a FixtureRunner or FakeRunner produces in tests, since
// both implement ExitCode() int.
func exitCode(err error) (int, bool) {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode(), true
	}
	return 0, false
}

// LookupResult is Identify's outcome: the disc ID whipper's TOC read
// produced, and whatever MusicBrainz releases AMA's own lookup found for it.
// A disc absent from MusicBrainz is not an error -- Releases is simply nil,
// so the caller can present a "no candidates" result the same way a TMDB
// miss is handled, per the issue's "Needs input" question 3 (keep, don't
// discard, a disc AccurateRip or MusicBrainz has nothing on).
type LookupResult struct {
	DiscID   string
	Releases []musicbrainz.Release
}

// Client drives whipper for CD identification and ripping.
type Client struct {
	// Runner executes whipper. Nil means ExecRunner{}.
	Runner CommandRunner
	// Binary is the whipper executable. Empty means DefaultBinary.
	Binary string
	// MusicBrainz looks up release candidates once Identify has a disc ID.
	// Required: Identify returns an error if it is nil.
	MusicBrainz *musicbrainz.Client
	// Log receives non-fatal warnings, e.g. an incomplete AccurateRip
	// verification. Nil means slog.Default().
	Log *slog.Logger
}

// NewClient returns a Client that shells out to the real whipper and looks
// releases up against mb.
func NewClient(mb *musicbrainz.Client) *Client {
	return &Client{MusicBrainz: mb}
}

// Identify reads device's TOC via `whipper cd info` and returns the disc ID
// plus whatever release candidates internal/musicbrainz finds for it, for
// web/confirm.html to present. It does not rip anything.
//
// `whipper cd info` legitimately exits 255 even when the TOC read itself
// succeeded, if whipper's own MusicBrainz lookup (which this package ignores
// in favour of its own, per the package doc) found nothing. Any exit 255
// that still produced a parseable disc ID is treated as success here, not as
// a hard error; only a non-255/non-zero exit, or a 255 with no disc ID at all
// (no CD in the drive, a failed TOC read, a missing dependency), is.
func (c *Client) Identify(ctx context.Context, device string) (*LookupResult, error) {
	if c.MusicBrainz == nil {
		return nil, errors.New("cd: identify: Client.MusicBrainz is not configured")
	}
	if device == "" {
		return nil, errors.New("cd: identify: device is required")
	}

	stdout, stderr, runErr := c.runner().RunCapture(ctx, c.binary(), "cd", "info", "-d", device)
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: %v", ctxErr, runErr)
		}
		if code, ok := exitCode(runErr); !ok || code != exitSystemError {
			return nil, c.commandError("cd info", stderr, runErr)
		}
		// exit 255: fall through and see whether a disc id came back anyway.
	}

	discID, perr := parseDiscID(stdout)
	if perr != nil {
		if runErr != nil {
			return nil, c.commandError("cd info", stderr, runErr)
		}
		return nil, fmt.Errorf("cd: identify: %w", perr)
	}

	releases, err := c.MusicBrainz.ByDiscID(ctx, discID)
	if err != nil {
		return nil, fmt.Errorf("cd: identify: musicbrainz lookup for disc %s: %w", discID, err)
	}
	return &LookupResult{DiscID: discID, Releases: releases}, nil
}

// discIDPrefix is the line `whipper cd info` prints the disc ID on.
const discIDPrefix = "MusicBrainz disc id "

// parseDiscID extracts the disc ID from `whipper cd info` stdout. This is
// the one line of whipper's cd info output this package reads; everything
// else (the "Matching releases:" block) is intentionally ignored -- see the
// package doc.
func parseDiscID(stdout []byte) (string, error) {
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		if id, ok := strings.CutPrefix(line, discIDPrefix); ok {
			id = strings.TrimSpace(id)
			if id == "" {
				return "", errors.New("whipper printed an empty MusicBrainz disc id")
			}
			return id, nil
		}
	}
	return "", errors.New("whipper cd info output did not contain a MusicBrainz disc id line")
}

// Rip drives whipper's actual extraction, pinned to release -- the one the
// user confirmed in web/confirm.html from Identify's candidates -- so the
// rip can never silently pick a different release than the one shown. It
// never passes -U/--unknown: if the pinned release ID does not come back
// from whipper's own MusicBrainz call, whipper exits rather than falling
// back to "Unknown Artist" tagging, which is the failure mode AMA wants.
//
// outputRoot is config's output.music; whipper's own --disc-template and
// --track-template lay the rip out under it as
// {artist}/{album}/{NN} - {title}.flac, matching docs/MANIFEST.md.
//
// Exit 5 ("finished, some tracks skipped" -- reachable only because Rip
// always passes -k) is not treated as a failure: the .log is still written
// and still parsed, and its per-track Status says which tracks were
// skipped. Exit 1 (RuntimeError: a real extraction failure) and 255
// (SystemError and friends) are hard failures and return a *CommandError.
// AccurateRip verification failing -- the disc's pressing not matching, or
// not being in the database at all -- is not a whipper failure at all (exit
// 0); it is reported through RipResult for the caller to record in the
// manifest, per the package doc.
func (c *Client) Rip(ctx context.Context, device string, release musicbrainz.Release, outputRoot string) (*RipResult, error) {
	if device == "" {
		return nil, errors.New("cd: rip: device is required")
	}
	if release.MBReleaseID == "" {
		return nil, errors.New("cd: rip: release.MBReleaseID is required")
	}
	if outputRoot == "" {
		return nil, errors.New("cd: rip: outputRoot is required")
	}

	args := []string{
		"cd", "rip",
		"-d", device,
		"-R", release.MBReleaseID,
		"-O", outputRoot,
		"--track-template", trackTemplate,
		"--disc-template", discTemplate,
		"-L", loggerPlugin,
		"--cdr",
		"-k",
	}

	_, stderr, runErr := c.runner().RunCapture(ctx, c.binary(), args...)
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: %v", ctxErr, runErr)
		}
		if code, ok := exitCode(runErr); !ok || code != exitPartialSkip {
			return nil, c.commandError("cd rip", stderr, runErr)
		}
		// exit 5: some tracks were skipped because -k was passed; the log is
		// still complete for the tracks that did rip and worth parsing.
	}

	path := logPath(outputRoot, release)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cd: rip: reading whipper's log %s: %w", path, err)
	}
	result, err := parseLog(data)
	if err != nil {
		return nil, fmt.Errorf("cd: rip: parsing whipper's log %s: %w", path, err)
	}
	if result.AccurateRipSummary != AccurateRipAllAccurate {
		c.logger().Warn("cd: rip finished without full AccurateRip verification",
			"summary", string(result.AccurateRipSummary), "health", string(result.HealthStatus))
	}
	return result, nil
}

// logPath predicts the .log file whipper writes for --disc-template
// '%A/%d/%A - %d' and the given confirmed release, so Rip can find it
// without scraping whipper's own stdout for a path.
//
// whipper sanitizes template substitutions that are not safe path
// components; this mirrors only the one case that actually turns up in
// practice -- a literal "/" inside an artist or album name, e.g. "AC/DC" --
// rather than replicating whipper's full sanitizer. See "Needs input" in the
// PR description: this is unverified against a real whipper run.
func logPath(outputRoot string, release musicbrainz.Release) string {
	artist := sanitizePathComponent(release.Artist)
	album := sanitizePathComponent(release.Album)
	name := artist + " - " + album
	return filepath.Join(outputRoot, artist, album, name+".log")
}

func sanitizePathComponent(s string) string {
	return strings.ReplaceAll(s, "/", "_")
}

func (c *Client) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return DefaultBinary
}

func (c *Client) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c *Client) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// commandError builds the error Identify/Rip return for a failed
// invocation, recognising a missing whipper binary as ErrNotInstalled.
func (c *Client) commandError(op string, stderr []byte, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrNotInstalled, err)
	}
	return &CommandError{Op: op, Stderr: string(stderr), Err: err}
}

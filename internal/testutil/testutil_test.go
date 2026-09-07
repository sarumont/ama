package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// examplesDir holds the reference captures shipped with this package.
const examplesDir = "testdata/examples"

func TestKey(t *testing.T) {
	if got, want := Key("makemkvcon", "-r", "info", "disc:0"), "makemkvcon -r info disc:0"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
	if got, want := Key("whipper"), "whipper"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
}

func TestFakeRunnerResponses(t *testing.T) {
	runner := &FakeRunner{
		Responses: map[string][]byte{
			"ffprobe -show_streams a.mkv": []byte("streams for a"),
			"ffprobe -show_streams b.mkv": []byte("streams for b"),
		},
	}

	for _, name := range []string{"b", "a"} {
		out, err := runner.Run(context.Background(), "ffprobe", "-show_streams", name+".mkv")
		if err != nil {
			t.Fatalf("Run(%s): %v", name, err)
		}
		if got, want := string(out), "streams for "+name; got != want {
			t.Errorf("Run(%s) = %q, want %q", name, got, want)
		}
	}

	want := []string{"ffprobe -show_streams b.mkv", "ffprobe -show_streams a.mkv"}
	if !reflect.DeepEqual(runner.Calls, want) {
		t.Errorf("Calls = %q, want %q", runner.Calls, want)
	}
}

func TestFakeRunnerSequence(t *testing.T) {
	runner := &FakeRunner{
		Responses: map[string][]byte{"makemkvcon -r info disc:0": []byte("keyed")},
		Sequence:  [][]byte{[]byte("first"), []byte("second")},
	}

	// A keyed match must not consume the sequence.
	if out, err := runner.Run(context.Background(), "makemkvcon", "-r", "info", "disc:0"); err != nil || string(out) != "keyed" {
		t.Fatalf("keyed call = %q, %v; want %q, nil", out, err, "keyed")
	}
	for _, want := range []string{"first", "second"} {
		out, err := runner.Run(context.Background(), "makemkvcon", "-r", "mkv", "disc:0", "0", "/out")
		if err != nil {
			t.Fatalf("sequenced call: %v", err)
		}
		if string(out) != want {
			t.Errorf("sequenced call = %q, want %q", out, want)
		}
	}

	// The sequence is exhausted, so an unknown call is now an error.
	if _, err := runner.Run(context.Background(), "makemkvcon", "-r", "mkv", "disc:0", "0", "/out"); err == nil {
		t.Error("Run() after exhausting Sequence = nil error, want error")
	}
}

func TestFakeRunnerErr(t *testing.T) {
	sentinel := errors.New("makemkvcon: no license")
	runner := &FakeRunner{
		Responses: map[string][]byte{"makemkvcon -r info disc:0": []byte("ignored")},
		Err:       sentinel,
	}

	out, err := runner.Run(context.Background(), "makemkvcon", "-r", "info", "disc:0")
	if !errors.Is(err, sentinel) {
		t.Errorf("Run() error = %v, want %v", err, sentinel)
	}
	if out != nil {
		t.Errorf("Run() stdout = %q, want nil", out)
	}
	if len(runner.Calls) != 1 {
		t.Errorf("Calls = %q, want one recorded call", runner.Calls)
	}
}

func TestFakeRunnerUnknownCall(t *testing.T) {
	runner := &FakeRunner{}

	_, err := runner.Run(context.Background(), "ffprobe", "-show_streams", "a.mkv")
	if err == nil {
		t.Fatal("Run() = nil error, want error for unmatched invocation")
	}
	if !strings.Contains(err.Error(), "ffprobe -show_streams a.mkv") {
		t.Errorf("error %q does not name the invocation", err)
	}
}

func TestFixtureRunner(t *testing.T) {
	runner := NewFixtureRunner(t, examplesDir)

	stdout, stderr, err := runner.RunCapture(context.Background(), "makemkvcon", "-r", "info", "disc:0")
	if err != nil {
		t.Fatalf("RunCapture(): %v", err)
	}
	if !strings.Contains(string(stdout), "TCOUNT:3") {
		t.Errorf("stdout does not contain the recorded title count:\n%s", stdout)
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %q, want empty for a fixture with no stderr file", stderr)
	}
}

func TestFixtureRunnerImplementsRunner(t *testing.T) {
	var runner Runner = NewFixtureRunner(t, examplesDir)

	stdout, err := runner.Run(context.Background(), "ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_streams", "/media/temp/EXAMPLE_FEATURE_t00.mkv")
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}

	var probe struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout, &probe); err != nil {
		t.Fatalf("decoding recorded ffprobe output: %v", err)
	}

	var pgs int
	for _, s := range probe.Streams {
		if s.CodecType == "subtitle" && s.CodecName == "hdmv_pgs_subtitle" {
			pgs++
		}
	}
	if want := 3; pgs != want {
		t.Errorf("recorded PGS subtitle streams = %d, want %d", pgs, want)
	}
}

func TestFixtureRunnerExitCode(t *testing.T) {
	runner := NewFixtureRunner(t, examplesDir)

	stdout, stderr, err := runner.RunCapture(context.Background(), "makemkvcon", "-r", "info", "disc:1")
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("RunCapture() error = %v, want *ExitError", err)
	}
	if exitErr.Code != 1 {
		t.Errorf("ExitError.Code = %d, want 1", exitErr.Code)
	}
	if !strings.Contains(string(exitErr.Stderr), "Failed to open disc") {
		t.Errorf("ExitError.Stderr = %q, want the recorded stderr", exitErr.Stderr)
	}
	if !strings.Contains(string(stderr), "Failed to open disc") {
		t.Errorf("stderr = %q, want the recorded stderr", stderr)
	}
	if !strings.Contains(string(stdout), "TCOUNT:0") {
		t.Errorf("stdout = %q, want the output recorded alongside the failure", stdout)
	}
}

// TestFixtureRunnerUnmatchedFails proves an unrecorded invocation fails the test
// rather than passing as a no-op. It runs against a stub testing.TB so the
// failure can be observed without failing this test.
func TestFixtureRunnerUnmatchedFails(t *testing.T) {
	stub := &stubTB{TB: t}
	runner := NewFixtureRunner(stub, examplesDir)

	runner.RunCapture(context.Background(), "makemkvcon", "-r", "info", "disc:9") //nolint:errcheck // stub records the failure

	if !stub.failed {
		t.Fatal("RunCapture() with no matching fixture did not fail the test")
	}
	if !strings.Contains(stub.message, "makemkvcon -r info disc:9") {
		t.Errorf("failure message %q does not name the attempted invocation", stub.message)
	}
	if !strings.Contains(stub.message, "makemkvcon -r info disc:0") {
		t.Errorf("failure message %q does not list the recorded invocations", stub.message)
	}
}

func TestLoadFixture(t *testing.T) {
	data := LoadFixture(t, examplesDir, "whipper", "cd-info", "stdout")
	if !strings.Contains(string(data), "MusicBrainz disc id") {
		t.Errorf("loaded fixture does not look like whipper output:\n%s", data)
	}
}

func TestLoadFixtureMissing(t *testing.T) {
	stub := &stubTB{TB: t}
	path := filepath.Join(examplesDir, "whipper", "cd-info", "nonexistent")

	LoadFixture(stub, path)

	if !stub.failed {
		t.Fatal("LoadFixture() of a missing file did not fail the test")
	}
	if !strings.Contains(stub.message, path) {
		t.Errorf("failure message %q does not name the missing file", stub.message)
	}
}

// stubTB records the first Fatalf instead of aborting, so tests can assert on
// the harness's own failure behaviour. Fatalf does not stop the caller here, so
// only use it where the code under test tolerates continuing.
type stubTB struct {
	testing.TB
	failed  bool
	message string
}

func (s *stubTB) Fatalf(format string, args ...any) {
	if !s.failed {
		s.failed = true
		s.message = fmt.Sprintf(format, args...)
	}
}

func (s *stubTB) Errorf(format string, args ...any) {
	s.Fatalf(format, args...)
}

func (s *stubTB) Helper() {}

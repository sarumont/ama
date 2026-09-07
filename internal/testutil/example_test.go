package testutil_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/sarumont/ama/internal/testutil"
)

// discScanner stands in for a production wrapper such as internal/bluray. It
// depends on the Runner seam, never on exec.Command, so tests can substitute
// recorded output for a real drive.
type discScanner struct {
	runner testutil.Runner
}

// titleCount parses the TCOUNT record out of makemkvcon robot-mode output.
func (s discScanner) titleCount(ctx context.Context, device string) (string, error) {
	stdout, err := s.runner.Run(ctx, "makemkvcon", "-r", "info", device)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		if count, ok := strings.CutPrefix(line, "TCOUNT:"); ok {
			return count, nil
		}
	}
	return "", fmt.Errorf("no TCOUNT record in makemkvcon output")
}

// ExampleFakeRunner shows the inline form: canned output keyed by the exact
// command line, which suits table-driven tests whose sample data is a few lines.
func ExampleFakeRunner() {
	runner := &testutil.FakeRunner{
		Responses: map[string][]byte{
			"makemkvcon -r info disc:0": []byte("DRV:0,2,999,12\nTCOUNT:2\n"),
		},
	}
	scanner := discScanner{runner: runner}

	count, err := scanner.titleCount(context.Background(), "disc:0")
	fmt.Println(count, err)
	fmt.Println(runner.Calls)
	// Output:
	// 2 <nil>
	// [makemkvcon -r info disc:0]
}

// ExampleFakeRunner_err shows how to drive the failure path of code under test.
func ExampleFakeRunner_err() {
	runner := &testutil.FakeRunner{Err: fmt.Errorf("exit status 1")}
	scanner := discScanner{runner: runner}

	_, err := scanner.titleCount(context.Background(), "disc:0")
	fmt.Println(err)
	// Output:
	// exit status 1
}

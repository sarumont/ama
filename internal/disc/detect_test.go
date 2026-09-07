package disc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// testInterval keeps the poll loop fast. The tests never wait a real poll
// interval out; they wait on the events channel.
const testInterval = time.Millisecond

// testDevice is a label only — no fake checker ever touches the filesystem.
const testDevice = "/dev/fake0"

// step is one scripted reply from fakeChecker.
type step struct {
	status DriveStatus
	err    error
}

// fakeChecker replays a script of drive states, one per Status call, and holds
// the final step once the script runs out so the drive settles into a steady
// state instead of ending the test early.
type fakeChecker struct {
	mu    sync.Mutex
	steps []step
	calls int
}

func (f *fakeChecker) Status(string) (DriveStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.steps[min(f.calls, len(f.steps)-1)]
	f.calls++
	return s.status, s.err
}

func TestDetectorEvents(t *testing.T) {
	t.Parallel()

	errDevice := errors.New("device busy")

	tests := []struct {
		name  string
		steps []step
		want  []EventType
	}{
		{
			name:  "disc already in drive at startup",
			steps: []step{{status: StatusDiscOK}},
			want:  []EventType{DiscInserted},
		},
		{
			name:  "empty drive emits nothing",
			steps: []step{{status: StatusNoDisc}},
			want:  nil,
		},
		{
			name:  "insertion detected",
			steps: []step{{status: StatusNoDisc}, {status: StatusDiscOK}},
			want:  []EventType{DiscInserted},
		},
		{
			name: "spin-up is not an insertion until readable",
			steps: []step{
				{status: StatusNoDisc},
				{status: StatusNotReady},
				{status: StatusNotReady},
				{status: StatusDiscOK},
			},
			want: []EventType{DiscInserted},
		},
		{
			name:  "disc never becoming readable emits nothing",
			steps: []step{{status: StatusNoDisc}, {status: StatusNotReady}},
			want:  nil,
		},
		{
			name: "no repeat insertion while the same disc stays loaded",
			steps: []step{
				{status: StatusDiscOK},
				{status: StatusDiscOK},
				{status: StatusDiscOK},
			},
			want: []EventType{DiscInserted},
		},
		{
			name:  "removal detected",
			steps: []step{{status: StatusDiscOK}, {status: StatusNoDisc}},
			want:  []EventType{DiscInserted, DiscRemoved},
		},
		{
			name:  "open tray counts as removal",
			steps: []step{{status: StatusDiscOK}, {status: StatusTrayOpen}},
			want:  []EventType{DiscInserted, DiscRemoved},
		},
		{
			name: "absent to present to absent to present",
			steps: []step{
				{status: StatusNoDisc},
				{status: StatusDiscOK},
				{status: StatusNoDisc},
				{status: StatusDiscOK},
			},
			want: []EventType{DiscInserted, DiscRemoved, DiscInserted},
		},
		{
			name:  "transient error is surfaced and polling continues",
			steps: []step{{err: errDevice}, {status: StatusDiscOK}},
			want:  []EventType{DriveError, DiscInserted},
		},
		{
			name: "error does not re-announce a disc already present",
			steps: []step{
				{status: StatusDiscOK},
				{err: errDevice},
				{status: StatusDiscOK},
			},
			want: []EventType{DiscInserted, DriveError},
		},
		{
			name:  "unreported status leaves the drive state alone",
			steps: []step{{status: StatusDiscOK}, {status: StatusNoInfo}},
			want:  []EventType{DiscInserted},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			d := New(testDevice, testInterval, &fakeChecker{steps: tt.steps})
			go d.Run(ctx)

			var got []EventType
			timeout := time.After(5 * time.Second)
			for len(got) < len(tt.want) {
				select {
				case e, ok := <-d.Events():
					if !ok {
						t.Fatalf("events channel closed after %v, want %v", got, tt.want)
					}
					if e.Device != testDevice {
						t.Errorf("event device = %q, want %q", e.Device, testDevice)
					}
					if (e.Err != nil) != (e.Type == DriveError) {
						t.Errorf("event %v has Err = %v", e.Type, e.Err)
					}
					got = append(got, e.Type)
				case <-timeout:
					t.Fatalf("timed out with %v, want %v", got, tt.want)
				}
			}

			// The script has settled; nothing more should arrive.
			select {
			case e, ok := <-d.Events():
				if ok {
					t.Errorf("unexpected extra event %v after %v", e.Type, got)
				}
			case <-time.After(50 * testInterval):
			}

			for i, want := range tt.want {
				if got[i] != want {
					t.Fatalf("events = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestDetectorRunStopsOnCancel(t *testing.T) {
	t.Parallel()

	// A drive that is always loaded means Run is blocked trying to deliver
	// DiscInserted to a consumer that never reads, which is the case where
	// cancellation is easiest to get wrong.
	checker := &fakeChecker{steps: []step{{status: StatusDiscOK}}}
	d := New(testDevice, testInterval, checker)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if _, ok := <-d.Events(); ok {
		t.Error("events channel still open after Run returned")
	}
}

func TestNewDefaultsToIoctlChecker(t *testing.T) {
	t.Parallel()

	if _, ok := New(testDevice, testInterval, nil).checker.(IoctlChecker); !ok {
		t.Error("New with a nil checker did not default to IoctlChecker")
	}
}

func TestIoctlCheckerMissingDevice(t *testing.T) {
	t.Parallel()

	if _, err := (IoctlChecker{}).Status(testDevice); err == nil {
		t.Error("Status on a missing device returned no error")
	}
}

func TestEjectMissingDevice(t *testing.T) {
	t.Parallel()

	if err := Eject(testDevice); err == nil {
		t.Error("Eject on a missing device returned no error")
	}
}

func TestStatusAndEventTypeStrings(t *testing.T) {
	t.Parallel()

	if got := StatusDiscOK.String(); got != "disc_ok" {
		t.Errorf("StatusDiscOK.String() = %q", got)
	}
	if got := DriveStatus(99).String(); got != "unknown(99)" {
		t.Errorf("DriveStatus(99).String() = %q", got)
	}
	if got := DiscRemoved.String(); got != "disc_removed" {
		t.Errorf("DiscRemoved.String() = %q", got)
	}
	if got := EventType(99).String(); got != "unknown(99)" {
		t.Errorf("EventType(99).String() = %q", got)
	}
}

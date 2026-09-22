//go:build linux

// Package disc watches an optical drive and reports when a disc appears or
// goes away. It deliberately knows nothing about what kind of disc it is —
// identifying Blu-ray versus CD is a separate concern.
//
// Presence is read with the Linux CDROM_DRIVE_STATUS ioctl rather than by
// stat'ing or opening the device for reading. The ioctl is the only option
// that distinguishes "no disc" from "tray open" from "loaded but still
// spinning up", and that third state matters: a drive reports the disc a
// second or two before it is readable, so a presence check based on a
// successful read (or on ENOMEDIUM from a blocking open) either reports the
// disc late or reports it while it still cannot be used. The ioctl also needs
// no external binary and no mount, which suits the container deployment where
// the device is passed straight through with `devices: - /dev/sr0:/dev/sr0`.
//
// The actual ioctl sits behind StatusChecker so the poll loop can be tested
// against a scripted drive with no hardware present.
package disc

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Linux uapi/linux/cdrom.h ioctl requests and the "current slot" selector used
// by drive status queries.
const (
	cdromEject       = 0x5309
	cdromDriveStatus = 0x5326
	cdslCurrent      = 0x7fffffff
)

// DriveStatus is the state of an optical drive. The values match the CDS_*
// constants returned by CDROM_DRIVE_STATUS so the ioctl result maps across
// directly.
type DriveStatus int

const (
	// StatusNoInfo means the drive does not report a status.
	StatusNoInfo DriveStatus = 0
	// StatusNoDisc means the tray is closed and empty.
	StatusNoDisc DriveStatus = 1
	// StatusTrayOpen means the tray is open.
	StatusTrayOpen DriveStatus = 2
	// StatusNotReady means a disc is loaded but not yet readable, typically
	// because the drive is still spinning up.
	StatusNotReady DriveStatus = 3
	// StatusDiscOK means a disc is loaded and readable.
	StatusDiscOK DriveStatus = 4
)

// String implements fmt.Stringer.
func (s DriveStatus) String() string {
	switch s {
	case StatusNoInfo:
		return "no_info"
	case StatusNoDisc:
		return "no_disc"
	case StatusTrayOpen:
		return "tray_open"
	case StatusNotReady:
		return "not_ready"
	case StatusDiscOK:
		return "disc_ok"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// EventType is the kind of change a Detector reports.
type EventType int

const (
	// DiscInserted is emitted once per physical insertion, when a disc first
	// becomes readable. It is not repeated while that disc stays in the drive.
	DiscInserted EventType = iota
	// DiscRemoved is emitted when a previously readable disc is gone.
	DiscRemoved
	// DriveError is emitted once when a presence check first starts failing.
	// It is not repeated while the checks keep failing; another DriveError
	// means the checks recovered and then failed again. The Detector keeps
	// polling either way; the drive state it was tracking is left unchanged.
	DriveError
	// DriveStalled is emitted once when the drive has reported
	// StatusNotReady for staleAfterNotReady consecutive polls, in case a
	// disc never finishes spinning up (bad media, a dying drive). Not
	// repeated while it stays not-ready.
	DriveStalled
)

// String implements fmt.Stringer.
func (t EventType) String() string {
	switch t {
	case DiscInserted:
		return "disc_inserted"
	case DiscRemoved:
		return "disc_removed"
	case DriveError:
		return "drive_error"
	case DriveStalled:
		return "drive_stalled"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// Event is a single change observed on the drive.
type Event struct {
	Type   EventType
	Device string
	// Err is the failed presence check, set only when Type is DriveError.
	Err error
}

// StatusChecker reports the current state of an optical drive. Implementations
// must be safe to call repeatedly; the Detector calls one on every tick.
type StatusChecker interface {
	Status(device string) (DriveStatus, error)
}

// IoctlChecker reads drive status with the CDROM_DRIVE_STATUS ioctl. It is the
// production StatusChecker and requires a real Linux optical device.
type IoctlChecker struct{}

// Status implements StatusChecker.
func (IoctlChecker) Status(device string) (DriveStatus, error) {
	// O_NONBLOCK is required: a blocking open of an empty drive fails with
	// ENOMEDIUM, which is exactly the state we are trying to observe.
	f, err := os.OpenFile(device, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return StatusNoInfo, fmt.Errorf("open %s: %w", device, err)
	}
	defer func() { _ = f.Close() }()

	status, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), cdromDriveStatus, cdslCurrent)
	if errno != 0 {
		return StatusNoInfo, fmt.Errorf("CDROM_DRIVE_STATUS %s: %w", device, errno)
	}
	return DriveStatus(status), nil
}

// ejectDevice opens the drive and issues CDROMEJECT.
func ejectDevice(device string) error {
	f, err := os.OpenFile(device, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", device, err)
	}
	defer func() { _ = f.Close() }()

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), cdromEject, 0); errno != 0 {
		return fmt.Errorf("CDROMEJECT %s: %w", device, errno)
	}
	return nil
}

// eventBufferSize is a starting guess at how many events a slow consumer can
// fall behind by before a send blocks and delays polling. Revisit with real
// usage; see the tracking issue for identity-aware presence in case a
// consumer needs to tell two different discs apart across a gap this large.
const eventBufferSize = 16

// Detector polls one optical drive and reports insertions and removals.
type Detector struct {
	device             string
	interval           time.Duration
	staleAfterNotReady int
	checker            StatusChecker
	events             chan Event
	ejectRequests      chan ejectRequest
}

// ejectRequest asks Run to eject the drive between polls, so the eject never
// races a concurrent status check for the same device.
type ejectRequest struct {
	resp chan error
}

// minInterval is the smallest poll interval New accepts. time.NewTicker
// panics on a non-positive duration, and config.Load deliberately does not
// validate, so a config typo like disc.poll_interval: 0 must not be able to
// crash the daemon.
const minInterval = time.Second

// defaultStaleAfterNotReady is how many consecutive StatusNotReady polls
// New waits out before emitting DriveStalled when staleAfterNotReady is left
// at zero. 12 polls at the default 5s poll interval is 60 seconds.
const defaultStaleAfterNotReady = 12

// New returns a Detector for device, polling every interval. A nil checker
// selects IoctlChecker. A non-positive interval is clamped to minInterval. A
// non-positive staleAfterNotReady selects defaultStaleAfterNotReady.
func New(device string, interval time.Duration, checker StatusChecker, staleAfterNotReady int) *Detector {
	if checker == nil {
		checker = IoctlChecker{}
	}
	if interval <= 0 {
		interval = minInterval
	}
	if staleAfterNotReady <= 0 {
		staleAfterNotReady = defaultStaleAfterNotReady
	}
	return &Detector{
		device:             device,
		interval:           interval,
		staleAfterNotReady: staleAfterNotReady,
		checker:            checker,
		events:             make(chan Event, eventBufferSize),
		ejectRequests:      make(chan ejectRequest),
	}
}

// Events returns the channel events are delivered on. It is closed when Run
// returns. Sends have eventBufferSize of headroom before they block and delay
// polling.
func (d *Detector) Events() <-chan Event {
	return d.events
}

// Eject opens the drive tray. It is the primitive behind
// disc.eject_on_complete. The request is serviced by Run — which must
// already be running in another goroutine — between polls, so it never races
// Run's own device access the way an independent open/CDROMEJECT can (the
// drive reports EBUSY when use_count is already above one). A disc that is
// ejected produces a removal event like any other; one that is not stays
// present without producing a second insertion event.
func (d *Detector) Eject(ctx context.Context) error {
	req := ejectRequest{resp: make(chan error, 1)}
	select {
	case d.ejectRequests <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run polls until ctx is cancelled, then closes the events channel and
// returns. The drive is checked once immediately, so a disc already sitting in
// the drive at startup is reported without waiting out a full interval.
//
// Run is a single long-lived goroutine's worth of work and must not be called
// more than once per Detector.
func (d *Detector) Run(ctx context.Context) {
	defer close(d.events)

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	present := false
	// staleChecks counts consecutive polls that could not confirm the
	// drive's state (a failed open, or CDS_NO_INFO). If it crosses
	// maxStaleChecks while present is true, present is dropped: otherwise a
	// device that stays unreadable (unplugged, lost passthrough) forever
	// strands the detector believing a disc is still loaded, and a
	// different disc inserted once it recovers is never announced.
	const maxStaleChecks = 3
	staleChecks := 0
	// erroring tracks whether the last poll failed, so DriveError is emitted
	// once on the transition into failing rather than on every failed poll.
	erroring := false
	// notReadyStreak counts consecutive StatusNotReady polls; stalledEmitted
	// guards DriveStalled the same way erroring guards DriveError.
	notReadyStreak := 0
	stalledEmitted := false
	for {
		status, err := d.checker.Status(d.device)
		if err == nil {
			erroring = false
		}
		switch {
		case err != nil:
			// Surface it and keep polling: an unplugged or busy device is
			// usually transient, and dropping the state we hold would
			// re-announce the same disc once the device recovers.
			staleChecks++
			if staleChecks >= maxStaleChecks {
				present = false
			}
			wasErroring := erroring
			erroring = true
			if !wasErroring {
				if !d.emit(ctx, Event{Type: DriveError, Device: d.device, Err: err}) {
					return
				}
			}
		case status == StatusNoInfo:
			staleChecks++
			if staleChecks >= maxStaleChecks {
				present = false
			}
			notReadyStreak = 0
			stalledEmitted = false
		case status == StatusNotReady:
			// A disc spinning up is not yet an insertion.
			staleChecks = 0
			notReadyStreak++
			if notReadyStreak == d.staleAfterNotReady && !stalledEmitted {
				stalledEmitted = true
				if !d.emit(ctx, Event{Type: DriveStalled, Device: d.device}) {
					return
				}
			}
		case status == StatusDiscOK && !present:
			staleChecks = 0
			notReadyStreak = 0
			stalledEmitted = false
			present = true
			if !d.emit(ctx, Event{Type: DiscInserted, Device: d.device}) {
				return
			}
		case (status == StatusNoDisc || status == StatusTrayOpen) && present:
			staleChecks = 0
			notReadyStreak = 0
			stalledEmitted = false
			present = false
			if !d.emit(ctx, Event{Type: DiscRemoved, Device: d.device}) {
				return
			}
		default:
			staleChecks = 0
			notReadyStreak = 0
			stalledEmitted = false
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case req := <-d.ejectRequests:
			req.resp <- ejectDevice(d.device)
		}
	}
}

// emit delivers one event, reporting false if ctx was cancelled first.
func (d *Detector) emit(ctx context.Context, e Event) bool {
	select {
	case d.events <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

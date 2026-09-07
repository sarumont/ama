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
	// DriveError is emitted when a presence check fails. The Detector keeps
	// polling; the drive state it was tracking is left unchanged.
	DriveError
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
	defer f.Close()

	status, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), cdromDriveStatus, cdslCurrent)
	if errno != 0 {
		return StatusNoInfo, fmt.Errorf("CDROM_DRIVE_STATUS %s: %w", device, errno)
	}
	return DriveStatus(status), nil
}

// Eject opens the drive tray. It is the primitive behind disc.eject_on_complete
// and is never called by the Detector itself — a disc that is ejected produces
// a removal event like any other, and one that is not stays present without
// producing a second insertion event.
func Eject(device string) error {
	f, err := os.OpenFile(device, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", device, err)
	}
	defer f.Close()

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), cdromEject, 0); errno != 0 {
		return fmt.Errorf("CDROMEJECT %s: %w", device, errno)
	}
	return nil
}

// Detector polls one optical drive and reports insertions and removals.
type Detector struct {
	device   string
	interval time.Duration
	checker  StatusChecker
	events   chan Event
}

// New returns a Detector for device, polling every interval. A nil checker
// selects IoctlChecker.
func New(device string, interval time.Duration, checker StatusChecker) *Detector {
	if checker == nil {
		checker = IoctlChecker{}
	}
	return &Detector{
		device:   device,
		interval: interval,
		checker:  checker,
		events:   make(chan Event),
	}
}

// Events returns the channel events are delivered on. It is closed when Run
// returns. Sends are unbuffered, so a slow consumer delays polling rather than
// dropping an insertion.
func (d *Detector) Events() <-chan Event {
	return d.events
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
	for {
		status, err := d.checker.Status(d.device)
		switch {
		case err != nil:
			// Surface it and keep polling: an unplugged or busy device is
			// usually transient, and dropping the state we hold would
			// re-announce the same disc once the device recovers.
			if !d.emit(ctx, Event{Type: DriveError, Device: d.device, Err: err}) {
				return
			}
		case status == StatusDiscOK && !present:
			present = true
			if !d.emit(ctx, Event{Type: DiscInserted, Device: d.device}) {
				return
			}
		case (status == StatusNoDisc || status == StatusTrayOpen) && present:
			present = false
			if !d.emit(ctx, Event{Type: DiscRemoved, Device: d.device}) {
				return
			}
		}
		// StatusNotReady and StatusNoInfo are deliberately indeterminate: a
		// disc spinning up is not yet an insertion, and a drive that briefly
		// stops answering is not a removal.

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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

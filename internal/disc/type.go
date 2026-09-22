//go:build linux

package disc

// Disc kind detection. A DiscInserted event says a disc is readable; this says
// what it is, which decides whether the disc goes down the Blu-ray path, the CD
// path, or neither.
//
// There is no single check that answers the question. An audio CD carries no
// filesystem at all, so it has to be recognised at the device level, and the
// CDROM_DISC_STATUS ioctl does that without mounting anything — mounting a CDDA
// disc cannot work. Blu-ray and DVD both carry UDF, so the filesystem type does
// not separate them either; what separates them is the directory the spec puts
// at the root of the volume, BDMV for Blu-ray and VIDEO_TS for DVD-Video.
//
// So: ask the drive what it is holding, and only if that says "data" mount the
// volume and look at the root. The root inspection is KindFromMount, which is a
// pure function of a directory tree and is where all the interesting logic
// lives; mounting is a thin wrapper around syscall.Mount that unit tests cannot
// exercise without root and a real drive.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// cdromDiscStatus is the Linux uapi/linux/cdrom.h CDROM_DISC_STATUS request.
// Unlike CDROM_DRIVE_STATUS it reports what is on the disc rather than whether
// a disc is there at all.
const cdromDiscStatus = 0x5327

// Root directories that identify a video disc. Both are uppercase by spec, but
// the comparison is case-insensitive because a Joliet or Rock Ridge name table
// can hand back a lowercased name.
const (
	bdmvDir    = "BDMV"
	videoTSDir = "VIDEO_TS"
)

// DiscKind is what sort of disc is in the drive.
type DiscKind int

const (
	// KindUnknown is a disc that is readable but is none of the kinds below —
	// a data disc, a blank, or something malformed. Callers must treat this as
	// a loud failure: eject and report it rather than guessing at a rip.
	KindUnknown DiscKind = iota
	// KindBluRay is a disc with a BDMV directory at the volume root.
	KindBluRay
	// KindDVD is a disc with a VIDEO_TS directory at the volume root. DVD
	// ripping is out of scope for v1, so this is a normal return and not an
	// error: callers log a warning, skip the disc, and eject it.
	KindDVD
	// KindCD is an audio disc — CDDA, or mixed mode, where the audio tracks
	// are the part worth archiving.
	KindCD
)

// String implements fmt.Stringer.
func (k DiscKind) String() string {
	switch k {
	case KindUnknown:
		return "unknown"
	case KindBluRay:
		return "bluray"
	case KindDVD:
		return "dvd"
	case KindCD:
		return "cd"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

// DiscContent is what a drive reports the loaded disc holds. The values match
// the CDS_* constants returned by CDROM_DISC_STATUS.
type DiscContent int

const (
	// ContentNoInfo means the drive will not say.
	ContentNoInfo DiscContent = 0
	// ContentNoDisc means the drive answered but there is no disc loaded —
	// CDROMREADTOCHDR came back -ENOMEDIUM. A caller that just saw an
	// insertion event can hit this if DetectKind runs before the drive has
	// actually settled.
	ContentNoDisc DiscContent = 1
	// ContentTrayOpen means the tray is open.
	ContentTrayOpen DiscContent = 2
	// ContentAudio means every track is CDDA audio.
	ContentAudio DiscContent = 100
	// ContentData1 and the three below it are the data track modes. They are
	// reported separately by the kernel but mean the same thing here: mount it
	// and look inside.
	ContentData1 DiscContent = 101
	ContentData2 DiscContent = 102
	ContentXA21  DiscContent = 103
	ContentXA22  DiscContent = 104
	// ContentMixed means both audio and data tracks are present.
	ContentMixed DiscContent = 105
)

// String implements fmt.Stringer.
func (c DiscContent) String() string {
	switch c {
	case ContentNoInfo:
		return "no_info"
	case ContentNoDisc:
		return "no_disc"
	case ContentTrayOpen:
		return "tray_open"
	case ContentAudio:
		return "audio"
	case ContentData1:
		return "data1"
	case ContentData2:
		return "data2"
	case ContentXA21:
		return "xa21"
	case ContentXA22:
		return "xa22"
	case ContentMixed:
		return "mixed"
	default:
		return fmt.Sprintf("unknown(%d)", int(c))
	}
}

// ContentChecker reports what the disc in a drive holds. It exists so kind
// detection can be tested against a scripted drive, the same way StatusChecker
// serves the poll loop.
type ContentChecker interface {
	Content(device string) (DiscContent, error)
}

// IoctlContentChecker reads content type with the CDROM_DISC_STATUS ioctl. It
// is the production ContentChecker and requires a real Linux optical device.
type IoctlContentChecker struct{}

// Content implements ContentChecker.
func (IoctlContentChecker) Content(device string) (DiscContent, error) {
	// O_NONBLOCK for the same reason as IoctlChecker.Status: a blocking open
	// of an optical device waits on the drive.
	f, err := os.OpenFile(device, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ContentNoInfo, fmt.Errorf("open %s: %w", device, err)
	}
	defer func() { _ = f.Close() }()

	content, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), cdromDiscStatus, 0)
	if errno != 0 {
		return ContentNoInfo, fmt.Errorf("CDROM_DISC_STATUS %s: %w", device, errno)
	}
	return DiscContent(content), nil
}

// mounter makes the contents of a device readable at a path and returns a
// release function that undoes whatever it did, and whether the mount was
// reused rather than made by this call. The release function reports whether
// cleanup succeeded; a failed unmount leaves the disc stuck in the drive.
type mounter func(device string) (root string, reused bool, release func() error, err error)

// noRelease is a MountedDisc.Release for a kind that was never mounted:
// KindCD (no filesystem exists to mount), or a classification reached before
// or without a successful mount.
func noRelease() error { return nil }

// MountedDisc is the result of examining a device: the kind of disc found,
// and — for a data disc — the mount that was read to find out, held open so
// the caller can look inside it further before deciding what to do.
//
// bluray.ReadDiscInfo (docs/ARCHITECTURE.md step 3) reads BDMV/META/DL XML
// from exactly this Root, so a caller identifying a KindBluRay disc mounts it
// exactly once: Detect for the kind, then ReadDiscInfo(disc.Root) against the
// same mount, then Release when done with both.
type MountedDisc struct {
	// Kind is what the disc turned out to be.
	Kind DiscKind
	// Root is where the disc's filesystem is readable, for KindBluRay and
	// KindDVD. Empty for KindCD (there is no filesystem to mount) and for a
	// KindUnknown reached without a successful mount.
	Root string
	// Reused is true when Root is a mount this call did not create — something
	// outside AMA, such as a desktop automounter, already had the device
	// mounted there. Release is then a no-op: AMA must not unmount a mount it
	// does not own. A caller that means to Eject the disc needs to check this
	// first — CDROMEJECT is refused with EBUSY while anything is mounted on
	// the device, foreign or not, and unmounting someone else's mount to force
	// the eject through is a policy decision for the caller to make, not this
	// package's to make for them.
	Reused bool
	// Release undoes whatever Detect did to produce Root: unmounting and
	// removing the temporary mount point, unless Reused is true. Always
	// non-nil and safe to call exactly once, even when Root is empty.
	Release func() error
}

// DetectKind reports what kind of disc is in device and releases the mount
// immediately, for a caller that only needs the kind and never looks inside
// the volume itself. A nil checker selects IoctlContentChecker.
//
// A non-nil error means the disc could not be examined — the drive would not
// answer, or reported no disc loaded or an open tray. That is distinct from a
// successful examination that found nothing recognisable — including a mount
// failure, which is what a blank or malformed volume looks like — which
// returns KindUnknown with a nil error. Both are conditions the caller should
// refuse to rip on, but only the error says the disc is still unidentified;
// KindUnknown says it was identified as nothing AMA handles.
//
// KindDVD is likewise a normal return. DVD ripping is out of scope for v1 and
// the caller is expected to warn and skip rather than fail.
//
// A third case exists once the disc has been examined successfully: if
// releasing the mount afterwards fails, DetectKind still returns the kind it
// found, but with a non-nil error reporting the failed release. The disc is
// still identified; it is just still mounted.
//
// A caller that needs the mount root too — to read BDMV/META/DL XML off a
// Blu-ray before deciding what to do with it — wants Detect instead, which
// leaves the mount open for the caller to Release explicitly.
func DetectKind(device string, checker ContentChecker) (DiscKind, error) {
	disc, err := Detect(device, checker)
	if rerr := disc.Release(); rerr != nil {
		err = errors.Join(err, fmt.Errorf("release mount %s: %w", disc.Root, rerr))
	}
	return disc.Kind, err
}

// Detect examines device and returns a MountedDisc describing what was found.
// A nil checker selects IoctlContentChecker.
//
// The mount, if any, is left open: the caller owns disc.Release and must call
// it exactly once when done, whatever it goes on to do with disc.Root in the
// meantime. See MountedDisc for what a non-nil Root, a true Reused, and a
// failed Release mean.
//
// The error and KindUnknown cases follow the same rules as DetectKind's doc
// comment.
func Detect(device string, checker ContentChecker) (MountedDisc, error) {
	if checker == nil {
		checker = IoctlContentChecker{}
	}
	return detect(device, checker, mountReadOnly)
}

// detect is Detect with the mount step injected, so tests can drive the
// data-disc path without root or a real drive.
func detect(device string, checker ContentChecker, mount mounter) (MountedDisc, error) {
	content, err := checker.Content(device)
	if err != nil {
		return MountedDisc{Release: noRelease}, fmt.Errorf("disc content %s: %w", device, err)
	}

	// Audio first, and without mounting: a CDDA disc has no filesystem, so a
	// mount attempt fails and would turn a perfectly good CD into an error.
	// Mixed mode counts as a CD — the audio tracks are what gets archived.
	if content == ContentAudio || content == ContentMixed {
		return MountedDisc{Kind: KindCD, Release: noRelease}, nil
	}

	// Neither of these is "examined, nothing we handle" — the disc was not
	// examined at all. A caller that just saw an insertion event can hit
	// ContentNoDisc if Detect runs before the drive has settled.
	if content == ContentNoDisc || content == ContentTrayOpen {
		return MountedDisc{Release: noRelease}, fmt.Errorf("disc content %s: %s", device, content)
	}

	root, reused, release, err := mount(device)
	if err != nil {
		// A mount failure here is what a blank or malformed volume looks
		// like: the drive said "data disc" but neither udf nor iso9660 can
		// read a filesystem off it. That is examined-and-unhandled, not a
		// failed examination.
		return MountedDisc{Release: noRelease}, nil
	}

	return MountedDisc{
		Kind:    KindFromMount(root),
		Root:    root,
		Reused:  reused,
		Release: release,
	}, nil
}

// KindFromMount reports the kind of the disc mounted at root, by looking for
// the directory its spec puts at the volume root. It returns KindUnknown for
// anything else, including an unreadable root — a directory that cannot be
// listed holds nothing this can act on.
//
// It never returns KindCD: an audio disc has no filesystem to mount, so it can
// never be the thing at root.
func KindFromMount(root string) DiscKind {
	entries, err := os.ReadDir(root)
	if err != nil {
		return KindUnknown
	}

	// Blu-ray wins if both are present. Hybrid BD/DVD discs exist, and the
	// Blu-ray side is the one worth ripping.
	kind := KindUnknown
	for _, e := range entries {
		if !isDir(root, e) {
			continue
		}
		switch {
		case strings.EqualFold(e.Name(), bdmvDir):
			return KindBluRay
		case strings.EqualFold(e.Name(), videoTSDir):
			kind = KindDVD
		}
	}
	return kind
}

// isDir reports whether a root entry is a directory, following a symlink if the
// entry is one. Some rips leave the video directory as a link.
func isDir(root string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(filepath.Join(root, e.Name()))
	return err == nil && info.IsDir()
}

// mountReadOnly makes device readable at a path. If the device is already
// mounted — which it will be whenever something outside AMA, such as a desktop
// automounter, got there first — that mount is reused and left alone. Otherwise
// it is mounted read-only on a temporary directory that the release function
// unmounts and removes.
//
// Mounting needs CAP_SYS_ADMIN, which the container is expected to carry along
// with the passed-through device.
func mountReadOnly(device string) (string, bool, func() error, error) {
	if root, ok := existingMount(device); ok {
		return root, true, func() error { return nil }, nil
	}

	dir, err := os.MkdirTemp("", "ama-disc-")
	if err != nil {
		return "", false, nil, fmt.Errorf("temp mount point: %w", err)
	}

	// udf first: every Blu-ray and every video DVD is UDF. iso9660 covers the
	// older pressings that are ISO only.
	var errs []error
	for _, fstype := range []string{"udf", "iso9660"} {
		err := syscall.Mount(device, dir, fstype, syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
		if err == nil {
			return dir, false, func() error {
				var relErrs []error
				if err := syscall.Unmount(dir, 0); err != nil {
					relErrs = append(relErrs, fmt.Errorf("unmount %s: %w", dir, err))
				}
				if err := os.Remove(dir); err != nil {
					relErrs = append(relErrs, fmt.Errorf("remove %s: %w", dir, err))
				}
				return errors.Join(relErrs...)
			}, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", fstype, err))
	}

	_ = os.Remove(dir)
	return "", false, nil, errors.Join(errs...)
}

// existingMount returns the mount point device is already mounted on, if any.
func existingMount(device string) (string, bool) {
	target, err := filepath.EvalSymlinks(device)
	if err != nil {
		target = device
	}

	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return "", false
	}

	for _, line := range strings.Split(string(mounts), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		source, err := filepath.EvalSymlinks(fields[0])
		if err != nil {
			source = fields[0]
		}
		if source == target {
			// Mount points with spaces or tabs are escaped as octal in
			// /proc/self/mounts.
			return mountPathUnescaper.Replace(fields[1]), true
		}
	}
	return "", false
}

// mountPathUnescaper decodes the octal escapes /proc/self/mounts uses for the
// four characters that would otherwise break its field separation. A desktop
// automounter naming a mount point after the disc volume label makes spaces the
// common case.
var mountPathUnescaper = strings.NewReplacer(
	`\040`, " ",
	`\011`, "\t",
	`\012`, "\n",
	`\134`, `\`,
)

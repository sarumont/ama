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
	defer f.Close()

	content, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), cdromDiscStatus, 0)
	if errno != 0 {
		return ContentNoInfo, fmt.Errorf("CDROM_DISC_STATUS %s: %w", device, errno)
	}
	return DiscContent(content), nil
}

// mounter makes the contents of a device readable at a path and returns a
// release function that undoes whatever it did.
type mounter func(device string) (root string, release func(), err error)

// DetectKind reports what kind of disc is in device. A nil checker selects
// IoctlContentChecker.
//
// A non-nil error means the disc could not be examined — the drive would not
// answer, or a data volume would not mount. That is distinct from a successful
// examination that found nothing recognisable, which returns KindUnknown with a
// nil error. Both are conditions the caller should refuse to rip on, but only
// the error says the disc is still unidentified; KindUnknown says it was
// identified as nothing AMA handles.
//
// KindDVD is likewise a normal return. DVD ripping is out of scope for v1 and
// the caller is expected to warn and skip rather than fail.
func DetectKind(device string, checker ContentChecker) (DiscKind, error) {
	if checker == nil {
		checker = IoctlContentChecker{}
	}
	return detectKind(device, checker, mountReadOnly)
}

// detectKind is DetectKind with the mount step injected, so tests can drive the
// data-disc path without root or a real drive.
func detectKind(device string, checker ContentChecker, mount mounter) (DiscKind, error) {
	content, err := checker.Content(device)
	if err != nil {
		return KindUnknown, fmt.Errorf("disc content %s: %w", device, err)
	}

	// Audio first, and without mounting: a CDDA disc has no filesystem, so a
	// mount attempt fails and would turn a perfectly good CD into an error.
	// Mixed mode counts as a CD — the audio tracks are what gets archived.
	if content == ContentAudio || content == ContentMixed {
		return KindCD, nil
	}

	root, release, err := mount(device)
	if err != nil {
		return KindUnknown, fmt.Errorf("mount %s: %w", device, err)
	}
	defer release()

	return KindFromMount(root), nil
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
func mountReadOnly(device string) (string, func(), error) {
	if root, ok := existingMount(device); ok {
		return root, func() {}, nil
	}

	dir, err := os.MkdirTemp("", "ama-disc-")
	if err != nil {
		return "", nil, fmt.Errorf("temp mount point: %w", err)
	}

	// udf first: every Blu-ray and every video DVD is UDF. iso9660 covers the
	// older pressings that are ISO only.
	var errs []error
	for _, fstype := range []string{"udf", "iso9660"} {
		err := syscall.Mount(device, dir, fstype, syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
		if err == nil {
			return dir, func() {
				syscall.Unmount(dir, 0)
				os.Remove(dir)
			}, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", fstype, err))
	}

	os.Remove(dir)
	return "", nil, errors.Join(errs...)
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

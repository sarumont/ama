// Package bluray wraps the external tools AMA drives for the Blu-ray path.
//
// makemkv.go covers makemkvcon: writing the license key into MakeMKV's own
// settings file at startup, and running makemkvcon in robot mode (-r) to
// enumerate and rip titles. Robot mode is line based, every line is prefixed
// with a record type, and every string is quoted and backslash escaped, which
// makes it the only sane thing to parse. See
// https://www.makemkv.com/developers/usage.txt and apdefs.h from the MakeMKV
// open-source package for the record layouts and attribute ids used here.
//
// This package deliberately defines its own track types rather than importing
// internal/manifest: makemkvcon is the source of the raw numbers, and mapping
// them into the manifest (and classifying tracks into roles) belongs to the
// layers above.
package bluray

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultBinary is the makemkvcon executable, looked up on PATH.
const DefaultBinary = "makemkvcon"

// ErrNotInstalled reports that the makemkvcon executable could not be found.
var ErrNotInstalled = errors.New("makemkvcon not found in PATH")

// Track is one MakeMKV title. The fields mirror what the manifest's tracks[]
// entries need from this layer; role classification happens in titles.go.
type Track struct {
	// Index is the MakeMKV title index, i.e. the tN in the output file name.
	Index int
	// Name is MakeMKV's title name, usually derived from the disc volume label.
	Name string
	// DurationSeconds is the title runtime.
	DurationSeconds int
	// SizeBytes is the estimated (info) or final (rip) size of the title.
	SizeBytes int64
	// ChapterCount is the number of chapters in the title.
	ChapterCount int
	// AudioTrackCount is the number of audio streams in the title.
	AudioTrackCount int
	// HasCommentaryAudio reports whether any audio stream looks like a
	// commentary track. It is a convenience over AudioTracks.
	HasCommentaryAudio bool
	// AudioTracks carries the per-stream detail behind HasCommentaryAudio.
	AudioTracks []AudioTrack
	// OutputFileName is the file name MakeMKV writes for this title.
	OutputFileName string
	// OutputPath is the full path to the ripped file. Only Rip sets it.
	OutputPath string
}

// AudioTrack is one audio stream of a Track.
type AudioTrack struct {
	// Index is the MakeMKV stream index within the title.
	Index int
	// LanguageCode is the ISO 639-2 code, e.g. "eng".
	LanguageCode string
	// Name is MakeMKV's description of the stream, e.g. "Director's Comments".
	Name string
	// Flags is the raw ap_iaStreamFlags bitmask.
	Flags int
	// HasCommentaryFlag reports whether the stream is flagged (or named) as
	// director's commentary.
	HasCommentaryFlag bool
}

// CommandRunner runs an external command to completion and returns its standard
// output. It exists so tests can replace makemkvcon with recorded output; every
// AMA wrapper around an external tool should take one of these rather than
// calling os/exec directly.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout []byte, err error)
}

// ExecRunner is the real CommandRunner. The subprocess is killed when ctx is
// cancelled, which is what stops a long rip.
type ExecRunner struct{}

// Run executes name with args, returning whatever it wrote to stdout. A
// non-zero exit yields a *CommandError carrying stderr.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &CommandError{Name: name, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// CommandError reports a command that failed to run or exited non-zero, with
// whatever it wrote to stderr.
type CommandError struct {
	Name   string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("%s: %v", e.Name, e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func (e *CommandError) Unwrap() error { return e.Err }

// Client runs makemkvcon.
type Client struct {
	// Runner executes makemkvcon. Zero value means ExecRunner.
	Runner CommandRunner
	// Binary is the executable to run. Zero value means DefaultBinary.
	Binary string
	// MinLengthSeconds is makemkv.min_track_duration: titles shorter than this
	// are not enumerated or ripped. Zero means no minimum.
	MinLengthSeconds int
}

// NewClient returns a Client that shells out to the real makemkvcon.
func NewClient(minLengthSeconds int) *Client {
	return &Client{
		Runner:           ExecRunner{},
		Binary:           DefaultBinary,
		MinLengthSeconds: minLengthSeconds,
	}
}

// Info enumerates the titles on source without ripping. source is a makemkvcon
// source specifier such as "disc:0" or "dev:/dev/sr0".
//
// A disc with no titles long enough to keep is not an error: the result is an
// empty slice and the caller decides what that means.
func (c *Client) Info(ctx context.Context, source string) ([]Track, error) {
	return c.run(ctx, append(c.commonArgs(), "info", source)...)
}

// Rip writes every title of source into outputDir as MKV and returns the
// tracks with OutputPath set. Streams are copied as-is; no transcoding flag is
// ever passed.
func (c *Client) Rip(ctx context.Context, source, outputDir string) ([]Track, error) {
	tracks, err := c.run(ctx, append(c.commonArgs(), "mkv", source, "all", outputDir)...)
	if err != nil {
		return nil, err
	}

	var missing []string
	for i := range tracks {
		if tracks[i].OutputFileName == "" {
			missing = append(missing, fmt.Sprintf("title %d (no output file name reported)", tracks[i].Index))
			continue
		}
		path := filepath.Join(outputDir, tracks[i].OutputFileName)
		if _, err := os.Stat(path); err != nil {
			missing = append(missing, tracks[i].OutputFileName)
			continue
		}
		tracks[i].OutputPath = path
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("makemkvcon: expected output missing from %s: %s", outputDir, strings.Join(missing, ", "))
	}
	return tracks, nil
}

// commonArgs are the options shared by every invocation: robot mode, MakeMKV's
// own read cache, and the minimum title length.
func (c *Client) commonArgs() []string {
	args := []string{"-r", "--cache=1"}
	if c.MinLengthSeconds > 0 {
		args = append(args, "--minlength="+strconv.Itoa(c.MinLengthSeconds))
	}
	return args
}

func (c *Client) run(ctx context.Context, args ...string) ([]Track, error) {
	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	binary := c.Binary
	if binary == "" {
		binary = DefaultBinary
	}

	stdout, runErr := runner.Run(ctx, binary, args...)
	tracks, messages, parseErr := parseRobotOutput(stdout)

	if runErr != nil {
		if errors.Is(runErr, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrNotInstalled, runErr)
		}
		// Output is expected to be partial after a failure, so prefer
		// MakeMKV's own diagnosis over a parse complaint.
		if msg, ok := firstErrorMessage(messages); ok {
			return nil, fmt.Errorf("makemkvcon: %s: %w", msg, runErr)
		}
		return nil, fmt.Errorf("makemkvcon: %w", runErr)
	}
	if parseErr != nil {
		return nil, fmt.Errorf("makemkvcon: parsing output: %w", parseErr)
	}
	if msg, ok := firstErrorMessage(messages); ok {
		return nil, fmt.Errorf("makemkvcon: %s", msg)
	}
	return tracks, nil
}

// Attribute ids from AP_ItemAttributeId in apdefs.h.
const (
	attrType           = 1
	attrName           = 2
	attrLangCode       = 3
	attrChapterCount   = 8
	attrDuration       = 9
	attrDiskSizeBytes  = 11
	attrStreamFlags    = 22
	attrOutputFileName = 27
	attrTreeInfo       = 30
)

// streamTypeAudio is the message code SINFO carries alongside a localized
// "Audio" string for ap_iaType.
const streamTypeAudio = 6202

// Stream flags from AP_AVStreamFlag in apdefs.h.
const (
	streamFlagDirectorsComments          = 1
	streamFlagAlternateDirectorsComments = 2
)

// Message box flags from AP_UIMSG_* in apdefs.h.
const (
	uiMsgBoxMask     = 3854
	uiMsgBoxError    = 516
	uiMsgBoxYesNoErr = 1288
)

// message is one MSG record.
type message struct {
	code  int
	flags int
	text  string
}

// isError reports whether MakeMKV raised this message as an error dialog.
func (m message) isError() bool {
	switch m.flags & uiMsgBoxMask {
	case uiMsgBoxError, uiMsgBoxYesNoErr:
		return true
	}
	return false
}

func firstErrorMessage(messages []message) (string, bool) {
	for _, m := range messages {
		if m.isError() {
			return m.text, true
		}
	}
	return "", false
}

// stream accumulates the SINFO attributes of one stream.
type stream struct {
	index    int
	typeCode int
	langCode string
	name     string
	treeInfo string
	flags    int
}

// title accumulates the TINFO attributes of one title plus its streams.
type title struct {
	Track
	streams map[int]*stream
}

// parseRobotOutput parses makemkvcon -r output into titles and messages.
// Unrecognized record types are ignored; a malformed record of a type we do
// parse is an error.
func parseRobotOutput(data []byte) ([]Track, []message, error) {
	titles := map[int]*title{}
	var messages []message

	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		kind, rest := line[:colon], line[colon+1:]
		switch kind {
		case "MSG", "TCOUNT", "TINFO", "SINFO":
		default:
			continue
		}

		fields, err := splitRobotFields(rest)
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %s: %w", n+1, kind, err)
		}
		switch kind {
		case "MSG":
			msg, err := parseMessage(fields)
			if err != nil {
				return nil, nil, fmt.Errorf("line %d: MSG: %w", n+1, err)
			}
			messages = append(messages, msg)
		case "TCOUNT":
			if len(fields) != 1 {
				return nil, nil, fmt.Errorf("line %d: TCOUNT: want 1 field, got %d", n+1, len(fields))
			}
			if _, err := strconv.Atoi(fields[0]); err != nil {
				return nil, nil, fmt.Errorf("line %d: TCOUNT: %w", n+1, err)
			}
		case "TINFO":
			if err := applyTitleInfo(titles, fields); err != nil {
				return nil, nil, fmt.Errorf("line %d: TINFO: %w", n+1, err)
			}
		case "SINFO":
			if err := applyStreamInfo(titles, fields); err != nil {
				return nil, nil, fmt.Errorf("line %d: SINFO: %w", n+1, err)
			}
		}
	}

	return collectTracks(titles), messages, nil
}

// parseMessage reads MSG:code,flags,count,message,format,param...
func parseMessage(fields []string) (message, error) {
	if len(fields) < 4 {
		return message{}, fmt.Errorf("want at least 4 fields, got %d", len(fields))
	}
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return message{}, fmt.Errorf("code: %w", err)
	}
	flags, err := strconv.Atoi(fields[1])
	if err != nil {
		return message{}, fmt.Errorf("flags: %w", err)
	}
	return message{code: code, flags: flags, text: fields[3]}, nil
}

// applyTitleInfo reads TINFO:title,attribute,code,value.
func applyTitleInfo(titles map[int]*title, fields []string) error {
	if len(fields) != 4 {
		return fmt.Errorf("want 4 fields, got %d", len(fields))
	}
	index, attr, err := parseIndexAndAttr(fields[0], fields[1])
	if err != nil {
		return err
	}
	value := fields[3]
	t := titleAt(titles, index)

	switch attr {
	case attrName:
		t.Name = value
	case attrChapterCount:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("chapter count: %w", err)
		}
		t.ChapterCount = n
	case attrDuration:
		seconds, err := parseDuration(value)
		if err != nil {
			return fmt.Errorf("duration: %w", err)
		}
		t.DurationSeconds = seconds
	case attrDiskSizeBytes:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("size: %w", err)
		}
		t.SizeBytes = n
	case attrOutputFileName:
		t.OutputFileName = value
	}
	return nil
}

// applyStreamInfo reads SINFO:title,stream,attribute,code,value.
func applyStreamInfo(titles map[int]*title, fields []string) error {
	if len(fields) != 5 {
		return fmt.Errorf("want 5 fields, got %d", len(fields))
	}
	index, err := strconv.Atoi(fields[0])
	if err != nil {
		return fmt.Errorf("title index: %w", err)
	}
	streamIndex, attr, err := parseIndexAndAttr(fields[1], fields[2])
	if err != nil {
		return err
	}
	code, err := strconv.Atoi(fields[3])
	if err != nil {
		return fmt.Errorf("message code: %w", err)
	}
	value := fields[4]

	t := titleAt(titles, index)
	s := t.streams[streamIndex]
	if s == nil {
		s = &stream{index: streamIndex}
		t.streams[streamIndex] = s
	}

	switch attr {
	case attrType:
		s.typeCode = code
	case attrLangCode:
		s.langCode = value
	case attrName:
		s.name = value
	case attrTreeInfo:
		s.treeInfo = value
	case attrStreamFlags:
		flags, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("stream flags: %w", err)
		}
		s.flags = flags
	}
	return nil
}

func parseIndexAndAttr(indexField, attrField string) (index, attr int, err error) {
	index, err = strconv.Atoi(indexField)
	if err != nil {
		return 0, 0, fmt.Errorf("index: %w", err)
	}
	attr, err = strconv.Atoi(attrField)
	if err != nil {
		return 0, 0, fmt.Errorf("attribute id: %w", err)
	}
	return index, attr, nil
}

func titleAt(titles map[int]*title, index int) *title {
	t := titles[index]
	if t == nil {
		t = &title{Track: Track{Index: index}, streams: map[int]*stream{}}
		titles[index] = t
	}
	return t
}

// collectTracks flattens the accumulated titles, in title index order, and
// derives the audio summary. It always returns a non-nil slice.
func collectTracks(titles map[int]*title) []Track {
	indexes := make([]int, 0, len(titles))
	for index := range titles {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	tracks := make([]Track, 0, len(indexes))
	for _, index := range indexes {
		t := titles[index]
		track := t.Track
		for _, s := range sortedStreams(t.streams) {
			if s.typeCode != streamTypeAudio {
				continue
			}
			audio := AudioTrack{
				Index:             s.index,
				LanguageCode:      s.langCode,
				Name:              s.name,
				Flags:             s.flags,
				HasCommentaryFlag: isCommentary(s),
			}
			if audio.Name == "" {
				audio.Name = s.treeInfo
			}
			track.AudioTracks = append(track.AudioTracks, audio)
			if audio.HasCommentaryFlag {
				track.HasCommentaryAudio = true
			}
		}
		track.AudioTrackCount = len(track.AudioTracks)
		tracks = append(tracks, track)
	}
	return tracks
}

func sortedStreams(streams map[int]*stream) []*stream {
	out := make([]*stream, 0, len(streams))
	for _, s := range streams {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

// isCommentary reports whether a stream is director's commentary. The stream
// flags are authoritative, but plenty of discs leave them clear and only say so
// in the track name, so the name is checked too.
func isCommentary(s *stream) bool {
	if s.flags&(streamFlagDirectorsComments|streamFlagAlternateDirectorsComments) != 0 {
		return true
	}
	return mentionsCommentary(s.name) || mentionsCommentary(s.treeInfo)
}

func mentionsCommentary(s string) bool {
	return strings.Contains(strings.ToLower(s), "comment")
}

// parseDuration parses MakeMKV's "h:mm:ss" (or "mm:ss", or "ss") runtime.
func parseDuration(value string) (int, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("unexpected format %q", value)
	}
	seconds := 0
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("unexpected format %q", value)
		}
		seconds = seconds*60 + n
	}
	return seconds, nil
}

// splitRobotFields splits one robot-mode record body on commas, honouring
// quoted strings and backslash escapes, and returns the unquoted values.
func splitRobotFields(s string) ([]string, error) {
	var (
		fields  []string
		current strings.Builder
		quoted  bool
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			fields = append(fields, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if quoted || escaped {
		return nil, errors.New("unterminated quoted value")
	}
	return append(fields, current.String()), nil
}

// MakeMKV's settings file. The key is written there, rather than passed on the
// command line, because makemkvcon only reads it from settings.conf.
const (
	settingsDirName  = ".MakeMKV"
	settingsFileName = "settings.conf"
	appKeySetting    = "app_Key"
)

// SettingsPath reports where MakeMKV keeps its settings for the current user.
func SettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("makemkv: locating home directory: %w", err)
	}
	return filepath.Join(home, settingsDirName, settingsFileName), nil
}

// WriteLicenseKey stores the makemkv.key config value in MakeMKV's settings
// file, creating it if needed and leaving any other settings untouched. The
// daemon calls this once at startup; a purchased key never changes, so there is
// no rotation here.
func WriteLicenseKey(key string) error {
	path, err := SettingsPath()
	if err != nil {
		return err
	}
	return writeLicenseKeyTo(path, key)
}

func writeLicenseKeyTo(path, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("makemkv: license key is empty")
	}
	// The setting is a quoted string, so a key containing a quote, a backslash
	// or a newline would corrupt the file. Never echo the key itself.
	if strings.ContainsAny(key, "\"\\\n\r") {
		return errors.New("makemkv: license key contains invalid characters")
	}

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("makemkv: reading %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("makemkv: creating %s: %w", dir, err)
	}

	contents := setAppKey(string(existing), key)

	// Write to a temp file and rename, so a crash cannot leave MakeMKV with a
	// half-written settings file.
	tmp, err := os.CreateTemp(dir, settingsFileName+".*")
	if err != nil {
		return fmt.Errorf("makemkv: creating temp file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("makemkv: securing temp file: %w", err)
	}
	if _, err := tmp.WriteString(contents); err != nil {
		tmp.Close()
		return fmt.Errorf("makemkv: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("makemkv: writing temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("makemkv: replacing %s: %w", path, err)
	}
	return nil
}

// setAppKey replaces the app_Key line in a settings file, or appends one.
func setAppKey(contents, key string) string {
	line := fmt.Sprintf("%s = %q", appKeySetting, key)

	lines := strings.Split(contents, "\n")
	for i, existing := range lines {
		if !isAppKeyLine(existing) {
			continue
		}
		lines[i] = line
		return strings.Join(lines, "\n")
	}

	trimmed := strings.TrimRight(contents, "\n")
	if trimmed == "" {
		return line + "\n"
	}
	return trimmed + "\n" + line + "\n"
}

func isAppKeyLine(line string) bool {
	rest := strings.TrimSpace(line)
	if !strings.HasPrefix(rest, appKeySetting) {
		return false
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, appKeySetting))
	return strings.HasPrefix(rest, "=")
}

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
	"log/slog"
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
	// Index is the MakeMKV title index (the TINFO record index), renumbered
	// over whichever titles MakeMKV selected for this run; it does not
	// necessarily match the tNN in the output file name. It is also the
	// manifest's makemkv_index.
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

// CommandRunner runs an external command to completion, invoking onLine for
// every line it writes to stdout as the line arrives (without its trailing
// newline), so a long-running command — a multi-hour rip — can surface
// progress before it exits. It exists so tests can replay recorded output
// instead of shelling out; every AMA wrapper around an external tool should
// take one of these rather than calling os/exec directly.
//
// If onLine returns an error, Run stops feeding it further lines and returns
// that error. This does not itself kill the subprocess — callers that need
// that should cancel ctx from within onLine.
type CommandRunner interface {
	Run(ctx context.Context, name string, onLine func(line []byte) error, args ...string) error
}

// ExecRunner is the real CommandRunner. The subprocess is killed when ctx is
// cancelled, which is what stops a long rip.
type ExecRunner struct{}

// Run executes name with args, calling onLine as it produces output. A
// non-zero exit yields a *CommandError carrying stderr, unless ctx was
// cancelled or timed out first, in which case the returned error wraps
// ctx.Err() so callers can tell a cancelled rip from a genuine crash with
// errors.Is. A process that exits cleanly but hands onLine a line it
// rejects returns onLine's own error instead.
func (ExecRunner) Run(ctx context.Context, name string, onLine func(line []byte) error, args ...string) error {
	var stderr bytes.Buffer
	lw := &lineWriter{onLine: onLine}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = lw
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %v", ctxErr, err)
		}
		return &CommandError{Name: name, Stderr: stderr.String(), Err: err}
	}
	return lw.err
}

// lineWriter is an io.Writer that splits whatever it is given on '\n' and
// invokes onLine per complete line, buffering a partial trailing line across
// writes. Once onLine returns an error, lineWriter stops calling it and
// discards everything written afterward.
type lineWriter struct {
	onLine func([]byte) error
	buf    []byte
	err    error
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		if err := w.onLine(line); err != nil {
			w.err = err
			break
		}
	}
	return len(p), nil
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

// Progress reports rip progress derived from MakeMKV's PRGC/PRGV robot
// records. Operation is the name of whatever MakeMKV is currently doing (from
// the most recent PRGC), and Current/Total/Max are the matching PRGV triple:
// Current is progress within Operation, Total is overall progress across the
// whole Info/Rip call, both on a 0..Max scale.
type Progress struct {
	Operation           string
	Current, Total, Max int
}

// Client runs makemkvcon.
type Client struct {
	// Runner executes makemkvcon. Zero value means ExecRunner.
	Runner CommandRunner
	// Binary is the executable to run. Zero value means DefaultBinary.
	Binary string
	// MinLengthSeconds is makemkv.min_track_duration. Rip does not write
	// titles shorter than this to disk; Info ignores it and always reports
	// every title (see Info's doc comment for why). Zero means no minimum.
	MinLengthSeconds int
	// Log receives a warning for every error-dialog-flagged MSG record that
	// arrives on an otherwise successful (exit 0) run — MakeMKV raises those
	// for conditions it then recovers from (a retried SCSI read), and a
	// clean exit is trusted over them. Nil means slog.Default().
	Log *slog.Logger
	// OnProgress, if set, is called for every PRGV record during Info or Rip.
	// It must return quickly: MakeMKV blocks on its output pipe until
	// something reads it, so a slow OnProgress stalls the rip.
	OnProgress func(Progress)
}

// NewClient returns a Client that shells out to the real makemkvcon.
func NewClient(minLengthSeconds int) *Client {
	return &Client{
		Runner:           ExecRunner{},
		Binary:           DefaultBinary,
		MinLengthSeconds: minLengthSeconds,
	}
}

// Info enumerates every title on source without ripping, regardless of
// MinLengthSeconds. source is a makemkvcon source specifier such as "disc:0"
// or "dev:/dev/sr0".
//
// min_track_duration is deliberately not passed here: internal/bluray/titles.go
// classifies tracks by duration and, per docs/CLAUDE.md's rule order, a
// commentary-flagged title shorter than the floor is still a "commentary" —
// not a "skip" — which Classify can only decide if the title reaches it in
// the first place. Filtering at enumeration time would make that rule
// unreachable and silently drop the title instead. Classify owns the floor;
// this call reports everything and lets it decide.
func (c *Client) Info(ctx context.Context, source string) ([]Track, error) {
	tracks, _, err := c.run(ctx, "-r", "--cache=1024", "info", source)
	return tracks, err
}

// Rip writes every title of source into outputDir as MKV and returns the
// tracks with OutputPath set. Streams are copied as-is; no transcoding flag is
// ever passed. Unlike Info, this does apply MinLengthSeconds: ripping a title
// only for Classify to mark it skip wastes disk I/O Info's enumeration does
// not cost.
func (c *Client) Rip(ctx context.Context, source, outputDir string) ([]Track, error) {
	args := []string{"-r", "--cache=1024"}
	if c.MinLengthSeconds >= 0 {
		args = append(args, "--minlength="+strconv.Itoa(c.MinLengthSeconds))
	}
	tracks, messages, err := c.run(ctx, append(args, "mkv", source, "all", outputDir)...)
	if err != nil {
		return nil, err
	}

	// makemkvcon exits 0 even when some titles failed partway through a rip,
	// leaving a truncated MKV behind for each; the only record of that is
	// MSG:5036 ("%1 titles saved, %2 failed"). A file existing on disk is not
	// evidence it ripped cleanly, so this has to be checked before the
	// existence check below can be trusted.
	if text, failed, found := partialRipFailure(messages); found && failed > 0 {
		return nil, fmt.Errorf("makemkvcon: %s", text)
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

func (c *Client) run(ctx context.Context, args ...string) ([]Track, []message, error) {
	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	binary := c.Binary
	if binary == "" {
		binary = DefaultBinary
	}

	parser := &robotParser{titles: map[int]*title{}, onProgress: c.OnProgress}
	runErr := runner.Run(ctx, binary, parser.processLine, args...)

	if runErr != nil {
		var perr *parseError
		if errors.As(runErr, &perr) {
			// The process itself exited cleanly (a parseError only reaches
			// here when lineWriter's write-side check let it through, i.e.
			// cmd.Run succeeded), so there is nothing more MakeMKV can add.
			return nil, nil, fmt.Errorf("makemkvcon: parsing output: %w", perr.err)
		}
		if errors.Is(runErr, exec.ErrNotFound) {
			return nil, nil, fmt.Errorf("%w: %v", ErrNotInstalled, runErr)
		}
		// Output is expected to be partial after a failure, so prefer
		// MakeMKV's own diagnosis over a bare exit error. parser.messages
		// holds whatever accumulated before the process gave up, so this
		// still finds MakeMKV's diagnosis on truncated output.
		if msg, ok := firstErrorMessage(parser.messages); ok {
			return nil, nil, fmt.Errorf("makemkvcon: %s: %w", msg, runErr)
		}
		return nil, nil, fmt.Errorf("makemkvcon: %w", runErr)
	}

	// Exit 0: trust it. MakeMKV raises an error-dialog-flagged MSG both for
	// genuinely fatal conditions and for ones it then recovers from mid-run
	// (a retried SCSI read that still finishes the title) — failing here
	// regardless would throw away a completed multi-hour rip over a message
	// that turned out not to matter. Surface each as a warning instead.
	for _, m := range parser.messages {
		if m.isError() {
			c.logger().Warn("makemkvcon: recoverable condition", "code", m.code, "message", m.text)
		}
	}
	return collectTracks(parser.titles), parser.messages, nil
}

func (c *Client) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
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
	code   int
	flags  int
	text   string
	params []string
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

// msgTitlesSaved is MSG:5036, "%1 titles saved, %2 failed": the record
// makemkvcon mkv emits once at the end of a rip. It is not flagged as an
// error and the process still exits 0 even when some titles failed
// partway through, so this is the only place a partial rip is reported.
const msgTitlesSaved = 5036

// partialRipFailure reports MSG:5036's failed-title count, if the message is
// present. text is the message's own localized text, suitable for an error.
func partialRipFailure(messages []message) (text string, failed int, found bool) {
	for _, m := range messages {
		if m.code != msgTitlesSaved || len(m.params) < 2 {
			continue
		}
		n, err := strconv.Atoi(m.params[1])
		if err != nil {
			continue
		}
		return m.text, n, true
	}
	return "", 0, false
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

// parseError wraps a malformed-record error from robotParser.processLine, so
// Client.run can tell a bad record apart from the process itself failing —
// the two need different error messages and different treatment of whatever
// messages were accumulated first.
type parseError struct{ err error }

func (e *parseError) Error() string { return e.err.Error() }
func (e *parseError) Unwrap() error { return e.err }

// robotParser accumulates makemkvcon -r output one line at a time, so a
// long-running rip can surface PRGV progress before it exits rather than only
// after the whole process finishes. Unrecognized record types are ignored; a
// malformed record of a type it does parse stops processing with a
// *parseError. See parseMessage, applyTitleInfo, applyStreamInfo and the PRGC
// /PRGV cases below for the record layouts, from
// https://www.makemkv.com/developers/usage.txt and apdefs.h.
type robotParser struct {
	titles     map[int]*title
	messages   []message
	onProgress func(Progress)

	// progressOp is the operation name from the most recent PRGC record,
	// carried forward onto every PRGV until the next PRGC.
	progressOp string
	lineNum    int
}

// processLine is a CommandRunner onLine callback.
func (p *robotParser) processLine(line []byte) error {
	p.lineNum++
	s := strings.TrimRight(string(line), "\r")
	if s == "" {
		return nil
	}
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return nil
	}
	kind, rest := s[:colon], s[colon+1:]
	switch kind {
	case "MSG", "TCOUNT", "TINFO", "SINFO", "PRGC", "PRGV":
	default:
		return nil
	}

	fields, err := splitRobotFields(rest)
	if err != nil {
		return &parseError{fmt.Errorf("line %d: %s: %w", p.lineNum, kind, err)}
	}
	switch kind {
	case "MSG":
		msg, err := parseMessage(fields)
		if err != nil {
			return &parseError{fmt.Errorf("line %d: MSG: %w", p.lineNum, err)}
		}
		p.messages = append(p.messages, msg)
	case "TCOUNT":
		if len(fields) != 1 {
			return &parseError{fmt.Errorf("line %d: TCOUNT: want 1 field, got %d", p.lineNum, len(fields))}
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			return &parseError{fmt.Errorf("line %d: TCOUNT: %w", p.lineNum, err)}
		}
	case "TINFO":
		if err := applyTitleInfo(p.titles, fields); err != nil {
			return &parseError{fmt.Errorf("line %d: TINFO: %w", p.lineNum, err)}
		}
	case "SINFO":
		if err := applyStreamInfo(p.titles, fields); err != nil {
			return &parseError{fmt.Errorf("line %d: SINFO: %w", p.lineNum, err)}
		}
	case "PRGC":
		// PRGC:code,id,name — the current operation's name. A malformed PRGC
		// is not fatal: progress is advisory, and there will be another one
		// along shortly.
		if len(fields) == 3 {
			p.progressOp = fields[2]
		}
	case "PRGV":
		// PRGV:current,total,max — current operation / overall progress on a
		// shared 0..max scale. Same leniency as PRGC: skip rather than abort
		// a rip over one bad progress tick.
		if len(fields) == 3 && p.onProgress != nil {
			cur, err1 := strconv.Atoi(fields[0])
			tot, err2 := strconv.Atoi(fields[1])
			max, err3 := strconv.Atoi(fields[2])
			if err1 == nil && err2 == nil && err3 == nil {
				p.onProgress(Progress{Operation: p.progressOp, Current: cur, Total: tot, Max: max})
			}
		}
	}
	return nil
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
	m := message{code: code, flags: flags, text: fields[3]}
	if len(fields) > 5 {
		m.params = fields[5:]
	}
	return m, nil
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

	// appDefaultSelectionSetting controls which streams makemkvcon mkv keeps.
	// MakeMKV's built-in default profile drops audio/subtitle streams outside
	// the favourite language, which conflicts with this project's
	// preservation-first goal of bit-perfect MKVs, so it is pinned to "keep
	// everything" alongside the license key.
	appDefaultSelectionSetting = "app_DefaultSelectionString"
	appDefaultSelectionValue   = "+sel:all"
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
// file, creating it if needed and leaving any other settings untouched except
// app_DefaultSelectionString, which is pinned to keep every stream. The
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

	contents := setSetting(string(existing), appKeySetting, key)
	contents = setSetting(contents, appDefaultSelectionSetting, appDefaultSelectionValue)

	// Write to a temp file and rename, so a crash cannot leave MakeMKV with a
	// half-written settings file. The temp file's data and the directory
	// entry are both fsynced, since without that a crash (or power loss)
	// around the rename can still commit an empty settings.conf on ext4.
	tmp, err := os.CreateTemp(dir, settingsFileName+".*")
	if err != nil {
		return fmt.Errorf("makemkv: creating temp file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("makemkv: securing temp file: %w", err)
	}
	if _, err := tmp.WriteString(contents); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("makemkv: writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("makemkv: syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("makemkv: writing temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("makemkv: replacing %s: %w", path, err)
	}

	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("makemkv: opening %s: %w", dir, err)
	}
	defer func() { _ = dirFile.Close() }()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("makemkv: syncing %s: %w", dir, err)
	}
	return nil
}

// setSetting replaces name's line in a settings file, or appends one.
func setSetting(contents, name, value string) string {
	line := fmt.Sprintf("%s = %q", name, value)

	lines := strings.Split(contents, "\n")
	for i, existing := range lines {
		if !isSettingLine(existing, name) {
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

func isSettingLine(line, name string) bool {
	rest := strings.TrimSpace(line)
	if !strings.HasPrefix(rest, name) {
		return false
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, name))
	return strings.HasPrefix(rest, "=")
}

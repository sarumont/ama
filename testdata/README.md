# Recorded external-tool fixtures

AMA's most failure-prone logic — title selection, forced-subtitle detection, CD
track parsing — is driven entirely by the stdout of external tools that need a
real optical drive and a licensed MakeMKV. Those tools are never invoked from a
test. Instead, their output is **recorded once** and replayed, so the suite runs
with no drive, no license, and no network.

This file is the contributor guide: where fixtures live, how to record one, and
what to scrub before committing it. The harness itself is
[`internal/testutil`](../internal/testutil); its package doc covers the Go API.

## The seam

No package calls `exec.Command` directly. Each one that shells out depends on a
runner interface, defaulting to an `os/exec` implementation it owns:

```go
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}
```

Use `testutil.CaptureRunner` — `RunCapture(ctx, name, args...) (stdout, stderr []byte, err error)`
— instead when a tool's stderr is part of what gets parsed (whipper reports rip
progress there). Tests substitute `testutil.FakeRunner` for inline sample data,
or `testutil.FixtureRunner` to replay recordings from disk.

## Where fixtures live

Fixtures live next to the package that parses them, never in this directory:

```
internal/bluray/testdata/makemkvcon/iron-man-3/
internal/subtitle/testdata/ffprobe/iron-man-3-pgs/
internal/cd/testdata/whipper/kind-of-blue/
```

This directory holds only the convention. Reference captures demonstrating the
layout are in
[`internal/testutil/testdata/examples`](../internal/testutil/testdata/examples) —
they are hand-constructed, not recorded, and no package should assert against
them.

## Fixture layout

One directory per recorded invocation, four files:

| File | Required | Contents |
| --- | --- | --- |
| `cmd` | yes | The argv that produced the capture, **one token per line**. Blank lines and `#` comments are ignored. |
| `stdout` | yes | Recorded stdout, verbatim. |
| `stderr` | no | Recorded stderr. Absent means empty. |
| `exitcode` | no | Recorded exit status. Absent means `0`. |

`cmd` is what makes a capture auditable: it records exactly which command line
produced this output, so the fixture can be regenerated years later, and
`FixtureRunner` fails any test whose code drifts to a different command line
rather than silently returning nothing.

Put provenance in a `#` comment at the top of `cmd` — tool version, disc, date,
and whether the capture is synthetic:

```
# makemkvcon v1.17.9, recorded 2026-08-21 from a retail BD-50
makemkvcon
-r
info
disc:0
```

## Recording a fixture

Capture stdout, stderr, and the exit status separately, then scrub before
committing. Run from the repository root.

**makemkvcon** — disc scan:

```sh
dir=internal/bluray/testdata/makemkvcon/iron-man-3
mkdir -p "$dir"
makemkvcon -r info disc:0 >"$dir/stdout" 2>"$dir/stderr"; echo $? >"$dir/exitcode"
printf '%s\n' '# makemkvcon v1.17.9, recorded YYYY-MM-DD' makemkvcon -r info disc:0 >"$dir/cmd"
```

**ffprobe** — stream list of an already-ripped MKV:

```sh
dir=internal/subtitle/testdata/ffprobe/iron-man-3-pgs
mkdir -p "$dir"
ffprobe -v quiet -print_format json -show_streams "$mkv" >"$dir/stdout" 2>"$dir/stderr"
echo $? >"$dir/exitcode"
```

**whipper** — CD TOC and MusicBrainz lookup:

```sh
dir=internal/cd/testdata/whipper/kind-of-blue
mkdir -p "$dir"
whipper cd info -d /dev/sr0 >"$dir/stdout" 2>"$dir/stderr"; echo $? >"$dir/exitcode"
```

whipper is chatty and its progress output is not reproducible; trim rip-progress
lines down to the records the parser actually reads before committing.

Write the `cmd` file to match the command you actually ran — one token per line,
in order, with the same paths. Where the recorded command references a file
under the repository (an MKV path, an output directory), rewrite the path in
both `cmd` and the capture to a stable placeholder such as
`/media/temp/EXAMPLE_FEATURE_t00.mkv`, and have the test pass that same string.

## Scrub before committing

Recorded output carries more than the parser needs. Before `git add`:

- **MakeMKV license keys** — appear in `MSG:` records and in any `--key`
  argument. Remove the record; replace the argument with `<license-key>`.
- **API keys and tokens** — TMDB and Radarr keys in argv or URLs. Replace with
  `<tmdb-key>` / `<radarr-key>`.
- **Home directory and mount paths** — `/home/richard/...`, NAS host names,
  share names. Rewrite to `/media/...` or `/srv/...` placeholders.
- **MusicBrainz and TMDB identifiers tied to a personal collection** — disc IDs,
  submission URLs containing a TOC, and account-scoped links. Replace disc IDs
  with an obviously synthetic value and drop submission URLs.
- **Drive serial numbers and firmware strings** — the `DRV:` record names the
  physical drive. Generic model strings are fine; anything unique to the unit is
  not.
- **Disc-copy identifiers** — volume labels or content IDs that identify a
  specific retail copy rather than the release.

Scrubbing must keep the capture parseable: replace values, do not delete fields.
Re-run the test after scrubbing.

## Naming

- Directory per invocation, named for the **content**, kebab-case:
  `iron-man-3`, `kind-of-blue`, `no-disc`, `unlicensed`.
- Grouped by tool one level up: `testdata/makemkvcon/...`, `testdata/ffprobe/...`,
  `testdata/whipper/...`.
- Where several captures come from one disc, suffix the aspect being tested:
  `iron-man-3-commentary`, `iron-man-3-forced-subs`.
- Failure captures are named for the failure: `no-disc`, `read-error`.

## Rules

- **Plain text only.** Fixtures must be diff-reviewable. No MKVs, no ISOs, no
  compressed blobs. If a test genuinely needs an MKV, generate a tiny synthetic
  one at test time with ffmpeg and delete it afterwards.
- **No network.** A fixture replaces the call; a test must never fall back to a
  live TMDB or MusicBrainz lookup when a fixture is missing.
- **Fail loud on a miss.** `FixtureRunner` fails the test on an invocation it has
  no recording for. Do not add a permissive default.
- **Trim, but do not fabricate.** Cutting irrelevant lines from a real capture is
  fine. Editing values so the parser produces a nicer answer is not — write a
  synthetic fixture and label it as such in the `cmd` comment instead.

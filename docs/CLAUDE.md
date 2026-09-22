# AMA — Claude Code Handoff

This document gives Claude Code the context needed to develop AMA effectively.

## Project Summary

AMA (Automated Media Archiver) is a Go service that automates ripping Blu-ray
discs and CDs into an archival-quality media library. It wraps MakeMKV (BD) and
whipper (CD), handles disc identification via TMDB/MusicBrainz, processes
subtitles, and integrates with Radarr.

**Core principle: preservation first. No transcoding, ever.**

## Owner

Personal project by Richard, a Principal Engineer. Go is the primary language.
Infrastructure background: Docker, Terraform, Kubernetes, OPNsense.

## Tech Stack

- **Language**: Go (latest stable)
- **Web**: `html/template` + HTMX (no JS framework)
- **Config**: YAML via `gopkg.in/yaml.v3`
- **External tools**: `makemkvcon`, `whipper`, `ffprobe`, `ffmpeg`,
  `mkvmerge`, `mkvpropedit`, `tesseract` — all bundled in the container image
- **APIs**: TMDB (movie ID), MusicBrainz (via whipper), Radarr v3

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design. Key points:

- Single container, single daemon process: disc detection, identification,
  ripping, subtitle analysis, and PGS→SRT OCR all run in-process
- Building the container image (with makemkv, whipper, ffmpeg, mkvtoolnix,
  tesseract bundled) is itself a project deliverable
- Web UI: queue, confirm, history (3 views only)

## Build Order

Suggested implementation order for Claude Code:

1. `config/config.go` — load + validate YAML config
2. `internal/disc/detect.go` — poll /dev/sr0, emit disc events
3. `internal/disc/type.go` — identify BD vs CD
4. `internal/manifest/schema.go` — define all manifest types
5. `internal/manifest/writer.go` — atomic JSON write + update
6. `internal/bluray/makemkv.go` — shell out to makemkvcon, parse output
7. `internal/bluray/bdmv.go` — read BDMV/META/DL XML
8. `internal/bluray/titles.go` — title selection logic (see rules below)
9. `internal/identify/tmdb.go` — TMDB search + candidate ranking
10. `internal/subtitle/analyze.go` — ffprobe wrapper, forced detection
11. `internal/subtitle/ocr.go` — pgsrip/tesseract wrapper, PGS → SRT + mux
12. `internal/radarr/client.go` — add movie + trigger scan
13. `internal/web/` — server + handlers + templates
14. `cmd/ama/main.go` — wire everything together
15. `internal/cd/whipper.go` — whipper wrapper (can follow BD path)
16. `Dockerfile` — container image bundling all external tools

## Title Selection Rules

This is the most failure-prone component. Implement carefully:

```
given all tracks from makemkvcon:
  sort by duration descending
  main_feature = tracks[0]
  for each remaining track:
    if duration >= main_feature.duration * 0.90:
      role = "alternate_cut"   // flag for user review
    else if any_audio_track.has_commentary_flag:
      role = "commentary"
    else if duration < config.min_track_duration:
      role = "skip"
    else:
      role = "extra"
```

## Subtitle Forced Detection

```
for each language in english PGS track pairs:
  ratio = smaller.size_bytes / larger.size_bytes
  if ratio < config.subtitle.forced_ratio_threshold (default 0.25):
    smaller.forced_candidate = true
```

Only apply to English (`eng`) tracks. Other languages are reported but not
auto-flagged.

## Manifest Updates

The manifest is written multiple times during a rip:
- After identification confirmed: status = "ripping"
- After rip complete: status = "analyzing", tracks populated
- After subtitle analysis: subtitles populated, status = "pending_ocr"
- After Radarr import: radarr fields populated, status = "complete"

Always write atomically (write to temp file, rename).

## Key Decisions

- **No in-place MKV modification** — always write `*.processed.mkv` alongside
  the original; never replace the source file
- **Manifest is append-only** — update fields, never delete them
- **Web UI is read-mostly** — the only user actions are: confirm disc ID,
  override title classification, manually set forced subtitle track
- **No background queue** — one disc at a time; the drive is the queue

## Testing

- Unit test title selection logic exhaustively (table-driven)
- Integration tests should use recorded `makemkvcon` / `ffprobe` output
  fixtures rather than real disc drives
- Store fixtures in `testdata/`
- Never call `exec.Command` directly from a package that shells out — depend on
  a `Run(ctx, name string, args ...string) ([]byte, error)` runner interface so
  tests can substitute recorded output
- `internal/testutil` provides `FakeRunner` (inline canned output) and
  `FixtureRunner` (replays recorded captures, failing the test on an
  unrecorded invocation)
- [testdata/README.md](../testdata/README.md) is the contributor guide: fixture
  layout, how to record from real hardware, and what to scrub before committing

## Out of Scope (v1)

- DVD ripping
- TV series / Sonarr integration
- Automated extras classification (handled by separate manual workflow)
- Transcoding of any kind

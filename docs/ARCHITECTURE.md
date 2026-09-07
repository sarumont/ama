# AMA Architecture

## Overview

AMA runs as a single Docker container, driven by one daemon process. The
container image bundles every external tool the pipeline needs — including
tesseract and mkvtoolnix — so disc detection, identification, ripping,
analysis, and subtitle OCR all run in-process with no host-side component.

```
┌─────────────────────────────────────────────┐
│ Docker Container                            │
│                                             │
│  udev/poll → detect → identify → rip       │
│                ↓                            │
│           manifest.json                     │
│           subtitle analysis + OCR           │
│           web UI (port 8080)                │
└─────────────────────────────────────────────┘
```

## Repository Structure

```
ama/
  cmd/
    ama/
      main.go              # entry point, wires everything together
  internal/
    disc/
      detect.go            # /dev/sr* polling, disc insertion events
      type.go              # disc type detection (BD vs CD)
    bluray/
      makemkv.go           # makemkvcon CLI wrapper
      bdmv.go              # BDMV/META/DL XML reader
      titles.go            # title selection logic
    cd/
      whipper.go           # whipper CLI wrapper + output parser
    identify/
      tmdb.go              # TMDB API client
      fuzzy.go             # disc label → title candidate ranking
    subtitle/
      analyze.go           # ffprobe wrapper, PGS detection, forced heuristic
      ocr.go               # pgsrip subprocess wrapper, PGS → SRT conversion
    manifest/
      schema.go            # manifest types
      writer.go            # atomic JSON writes
    radarr/
      client.go            # Radarr + Sonarr API clients
    web/
      server.go            # HTTP server (html/template + HTMX)
      handlers.go          # route handlers
      templates/
        layout.html
        queue.html          # disc queue + rip status
        confirm.html        # ID confirmation + manifest preview
        history.html        # completed rips
  config/
    config.go              # config struct + YAML loading
  Dockerfile               # container image: ama binary + makemkv, whipper,
                            # ffmpeg, mkvtoolnix, tesseract
  .github/
    workflows/
      ci.yml               # lint + vet + build + test
  Makefile
  go.mod
  README.md
  LICENSE
```

## Rip Flow

### Blu-ray

```
1. Disc inserted → /dev/sr0 detected
2. disc/type.go: confirm BD (not CD/DVD)
3. bluray/bdmv.go: read BDMV/META/DL/*.xml
   → disc title, year (if present)
4. identify/tmdb.go: search TMDB
   → ranked candidates (title, year, poster, TMDB ID)
5. web/confirm.html: present top 3 to user
   → user selects or searches manually
   → confirmed TMDB ID written to manifest
6. bluray/makemkv.go: rip all titles above minimum duration
7. bluray/titles.go: classify tracks
   → longest track = main feature
   → tracks within 10% of main = alternate cuts (flag for review)
   → tracks with commentary audio = flagged
   → remaining = extras candidates
8. subtitle/analyze.go: analyze all subtitle streams
   → detect PGS tracks
   → detect forced candidates (eng, size ratio < 0.25)
   → write *.subtitles.json
9. subtitle/ocr.go: for each PGS track
   → PGS → SRT via pgsrip/tesseract
   → mkvmerge: produce *.processed.mkv (original preserved)
   → update manifest: subtitles[n].converted = true
10. manifest/writer.go: write complete manifest JSON
11. radarr/client.go: add movie by TMDB ID + trigger import scan
12. eject disc
```

### CD

```
1. Disc inserted → /dev/sr0 detected
2. disc/type.go: confirm CD
3. cd/whipper.go: read disc TOC → MusicBrainz lookup (whipper-native)
4. web/confirm.html: present MB candidates
   → user confirms album/artist/year
5. cd/whipper.go: rip with AccurateRip verification
   → FLAC output per track
   → embedded MusicBrainz metadata
6. manifest/writer.go: write manifest
7. eject disc
```

## Title Selection Logic

Title selection is the most failure-prone part of any automated ripping
pipeline. AMA applies the following rules in order:

1. **Longest track** → main feature
2. **Tracks within 10% of main feature duration** → flagged as alternate cuts,
   moved to `{edition-...}` naming, require manual confirmation in web UI
3. **Tracks with a secondary audio track flagged as commentary** → excluded
   from main feature candidates, placed in extras
4. **Remaining tracks above minimum duration** (configurable, default 60s) →
   extras candidates, placed in `extras/` subfolder for manual classification

The web UI surfaces any ambiguous classification decisions before the rip is
considered complete.

## Subtitle Processing

### Analysis (analyze.go)

- Runs immediately post-rip on all MKV outputs
- Uses `ffprobe` to enumerate subtitle streams
- Records: stream index, codec, language, size, disposition flags
- Detects forced candidates: English PGS pairs where smaller track is
  < 25% the size of the larger
- Writes `{movie}.subtitles.json` alongside the MKV

### OCR (ocr.go)

- Runs in the same process immediately after analysis
- For each PGS track: extracts `.sup` via ffmpeg, OCRs via pgsrip/tesseract
- Sets forced + default flags on confirmed forced candidates
- Muxes converted SRT tracks into `{movie}.processed.mkv` (original preserved)
- Updates manifest: `subtitles[n].converted = true`

## Web UI

Three views, served via Go `html/template` + HTMX for live updates:

| View | Path | Purpose |
|------|------|---------|
| Queue | `/` | Active and pending discs, progress indicators |
| Confirm | `/confirm/:id` | TMDB/MB candidate selection, manifest preview |
| History | `/history` | Completed rips, warnings, manifest links |

No JavaScript framework. HTMX polls `/api/status/:id` for progress updates.

## Manifest

See [MANIFEST.md](MANIFEST.md) for the full schema.

## Configuration

See [CONFIG.md](CONFIG.md) for the full configuration reference.

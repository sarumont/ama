# AMA — Automated Media Archiver

AMA is a self-hosted, archival-focused media ripping pipeline for Blu-ray discs
and CDs. It is designed around **preservation first** — no transcoding, no
lossy re-encoding. MKV streams are kept bit-perfect from the source disc.

AMA is **not** a download manager or a Sonarr/Radarr replacement. It is the
missing link between a physical disc and a well-organized Plex/Jellyfin library.

## Features

- **Blu-ray** — MakeMKV-based ripping with intelligent title selection
- **CD** — whipper-based ripping with AccurateRip verification and FLAC output
- **Disc identification** — BDMV metadata + TMDB fuzzy matching with manual
  confirmation step
- **Subtitle handling** — PGS track analysis, forced-track detection, and
  PGS → SRT OCR conversion (host-side, tesseract-backed)
- **Radarr/Sonarr integration** — automatic add + import scan trigger
- **JSON manifest** — every rip produces a structured manifest for audit,
  downstream tooling, and extras sorting workflows
- **Simple web UI** — disc queue, identification confirmation, rip history

## Non-Goals (v1)

- Transcoding or re-encoding of any kind
- DVD ripping (planned for v2)
- TV series / Sonarr integration (planned for v2)
- Automated extras classification (handled by separate workflow)

## Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for full design documentation.

## Requirements

### Host

- `tesseract-ocr` — PGS subtitle OCR
- `mkvtoolnix` — mkvpropedit / mkvmerge for subtitle muxing

### Container

- MakeMKV (licensed)
- whipper
- ffmpeg / ffprobe

## Configuration

See [docs/CONFIG.md](docs/CONFIG.md) for full configuration reference.

## License

MIT

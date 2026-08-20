# AMA Manifest Schema

Every rip produces a JSON manifest written alongside the output files.
The manifest is the authoritative record of the rip and is used by downstream
tooling (subtitle OCR processor, Radarr integration, extras sorting workflow).

## Location

```
{output_path}/{Title} ({Year})/
  {Title} ({Year}).mkv              ← main feature
  {Title} ({Year}).subtitles.json   ← subtitle analysis
  {Title} ({Year}).processed.mkv    ← post-OCR output (host-side)
  {Title} ({Year}).manifest.json    ← this file
  extras/
    ...
```

## Schema

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "ama_version": "0.1.0",
  "ripped_at": "2026-08-19T14:23:00Z",
  "status": "complete",

  "disc": {
    "type": "bluray",
    "label": "MARVELS_IRON_MAN_3_BLU_RAY",
    "device": "/dev/sr0"
  },

  "identification": {
    "method": "bdmv+tmdb",
    "tmdb_id": 68721,
    "imdb_id": "tt1300854",
    "title": "Iron Man 3",
    "year": 2013,
    "confidence": 0.97,
    "confirmed": true,
    "confirmed_at": "2026-08-19T14:23:45Z",
    "confirmed_by": "user",
    "candidates": [
      { "tmdb_id": 68721, "title": "Iron Man 3", "year": 2013, "score": 0.97 },
      { "tmdb_id": 99861, "title": "Avengers: Age of Ultron", "year": 2015, "score": 0.12 }
    ]
  },

  "tracks": [
    {
      "makemkv_index": 0,
      "duration_seconds": 7647,
      "size_bytes": 28000000000,
      "audio_track_count": 2,
      "chapter_count": 25,
      "role": "feature",
      "role_reason": "longest track",
      "output_file": "Iron Man 3 (2013).mkv",
      "edition": null
    },
    {
      "makemkv_index": 2,
      "duration_seconds": 659,
      "size_bytes": 890000000,
      "audio_track_count": 1,
      "chapter_count": 1,
      "role": "extra",
      "role_reason": "duration below feature threshold",
      "output_file": "extras/Iron Man 3 (2013)_t02.mkv",
      "edition": null
    }
  ],

  "subtitles": [
    {
      "stream_index": 7,
      "codec": "hdmv_pgs_subtitle",
      "language": "eng",
      "size_bytes": 2100000,
      "forced_flag_in_source": false,
      "default_flag_in_source": false,
      "hearing_impaired": false,
      "forced_candidate": true,
      "forced_candidate_reason": "size ratio 0.06 vs stream 12 (2100000 vs 33000000 bytes)",
      "paired_with_stream_index": 12,
      "needs_ocr": true,
      "converted": true,
      "conversion_error": null
    },
    {
      "stream_index": 12,
      "codec": "hdmv_pgs_subtitle",
      "language": "eng",
      "size_bytes": 33000000,
      "forced_flag_in_source": false,
      "default_flag_in_source": false,
      "hearing_impaired": false,
      "forced_candidate": false,
      "forced_candidate_reason": null,
      "paired_with_stream_index": 7,
      "needs_ocr": true,
      "converted": true,
      "conversion_error": null
    }
  ],

  "output": {
    "path": "/media/library/movies/Iron Man 3 (2013)/",
    "main_feature": "Iron Man 3 (2013).mkv",
    "processed_feature": "Iron Man 3 (2013).processed.mkv"
  },

  "radarr": {
    "added": true,
    "added_at": "2026-08-19T14:31:00Z",
    "import_triggered": true,
    "import_triggered_at": "2026-08-19T14:31:05Z"
  },

  "warnings": [],
  "errors": []
}
```

## Status Values

| Status | Meaning |
|--------|---------|
| `pending_confirmation` | Waiting for user to confirm disc identification |
| `ripping` | MakeMKV/whipper in progress |
| `analyzing` | Subtitle analysis running |
| `pending_ocr` | Waiting for host-side OCR processor |
| `complete` | All steps finished successfully |
| `error` | One or more errors; see `errors` array |

## Role Values (tracks)

| Role | Meaning |
|------|---------|
| `feature` | Main feature — longest track |
| `alternate_cut` | Within 10% of feature duration — flagged for review |
| `commentary` | Secondary audio track identified as commentary |
| `extra` | Below feature threshold — placed in extras/ |

## CD Manifest

```json
{
  "id": "...",
  "ama_version": "0.1.0",
  "ripped_at": "...",
  "status": "complete",

  "disc": {
    "type": "cd",
    "label": null,
    "device": "/dev/sr0"
  },

  "identification": {
    "method": "musicbrainz",
    "mb_release_id": "...",
    "mb_release_group_id": "...",
    "artist": "Jamestown Revival",
    "album": "The Education of a Wandering Man",
    "year": 2014,
    "confirmed": true,
    "confirmed_at": "..."
  },

  "tracks": [
    {
      "number": 1,
      "title": "Where I Need to Be",
      "duration_seconds": 207,
      "accurate_rip": true,
      "output_file": "01 - Where I Need to Be.flac"
    }
  ],

  "output": {
    "path": "/media/library/music/Jamestown Revival/The Education of a Wandering Man/"
  },

  "warnings": [],
  "errors": []
}
```

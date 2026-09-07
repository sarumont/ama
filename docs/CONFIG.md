# AMA Configuration Reference

AMA is configured via a YAML file. By default AMA looks for `ama.yaml` in the
working directory, or the path set by the `AMA_CONFIG` environment variable.

## Full Example

```yaml
# ama.yaml

makemkv:
  key: ""                          # MakeMKV license key (required for BD)
  min_track_duration: 60           # seconds; tracks shorter than this are skipped

tmdb:
  api_key: ""                      # TMDB API read access token, sent as a
                                    # bearer token (required)
  auto_confirm_threshold: null     # fuzzy-match confidence (0-1) at or above
                                    # which a candidate is auto-confirmed
                                    # without a manual step; unset or below
                                    # this floor always requires manual
                                    # confirmation in the web UI

output:
  movies: /media/library/movies    # root path for movie output
  music: /media/library/music      # root path for music output
  temp: /tmp/ama                   # temp dir for in-progress rips

radarr:
  enabled: true
  url: http://radarr:7878
  api_key: ""

sonarr:
  enabled: false                   # v2
  url: http://sonarr:8989
  api_key: ""

web:
  port: 8080
  host: "0.0.0.0"

subtitle:
  forced_ratio_threshold: 0.25    # smaller/larger size ratio below which a
                                   # track is flagged as forced candidate
  ocr_languages:
    - eng                          # tesseract language codes

disc:
  device: /dev/sr0                 # optical drive device
  poll_interval: 5                 # seconds between disc presence checks
  eject_on_complete: true
```

## Environment Variable Overrides

All config values can be overridden via environment variables using the prefix
`AMA_` and a single underscore for nesting (a double underscore is also
accepted, for disambiguating a field name that itself contains an underscore
— e.g. `AMA_SUBTITLE__FORCED_RATIO_THRESHOLD`; if both forms are set for the
same field, the single-underscore form wins):

```
AMA_MAKEMKV_KEY=...
AMA_TMDB_API_KEY=...
AMA_RADARR_API_KEY=...
AMA_OUTPUT_MOVIES=/media/movies
AMA_WEB_PORT=9090
```

## Docker Compose Example

The image is **built locally, never pulled**. MakeMKV's binary package is not
freely redistributable, so the Dockerfile downloads and compiles MakeMKV at
build time and no image with MakeMKV in it is published anywhere. The build
therefore requires you to accept the [MakeMKV EULA](https://www.makemkv.com/eula/)
explicitly via a build arg.

```yaml
services:
  ama:
    image: ama:latest
    build:
      context: .
      args:
        MAKEMKV_ACCEPT_EULA: "yes"
    devices:
      - /dev/sr0:/dev/sr0
    volumes:
      - /media/library:/media/library
      - ./ama.yaml:/config/ama.yaml
    environment:
      AMA_CONFIG: /config/ama.yaml
      AMA_MAKEMKV_KEY: "${MAKEMKV_KEY}"
      AMA_TMDB_API_KEY: "${TMDB_API_KEY}"
    ports:
      - "8080:8080"
```

Then `docker compose build && docker compose up -d`, or without compose:

```
docker build --build-arg MAKEMKV_ACCEPT_EULA=yes -t ama:latest .
```

### Build Arguments

| Arg | Default | Purpose |
|---|---|---|
| `MAKEMKV_ACCEPT_EULA` | `no` | Must be `yes`; the build fails otherwise |
| `MAKEMKV_VERSION` | `1.18.4` | MakeMKV beta builds expire ~60 days after release — bump this and rebuild when `makemkvcon` reports an expired version |
| `TESSERACT_LANGS` | `eng osd fra deu spa ita jpn` | Tesseract language packs bundled into the image |
| `PGSRIP_VERSION` | `0.1.12` | pgsrip release used for PGS → SRT |

`subtitle.ocr_languages` may only name languages present in `TESSERACT_LANGS` —
the set is fixed at build time and AMA fails fast at startup on an unknown
language rather than installing packs at runtime. To add one, rebuild with the
extra [Debian `tesseract-ocr-<code>`](https://packages.debian.org/trixie/tesseract-ocr)
packs appended:

```
docker build --build-arg MAKEMKV_ACCEPT_EULA=yes \
             --build-arg TESSERACT_LANGS="eng osd fra deu spa ita jpn nld swe" \
             -t ama:latest .
```

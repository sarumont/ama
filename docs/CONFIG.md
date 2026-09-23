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
  auto_confirm_threshold: null     # fuzzy-match confidence (0,1] at or above
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

The final image (`ama:latest`, with MakeMKV) is **always built locally, never
pulled**. MakeMKV's binary package is not freely redistributable, so the
top-level `Dockerfile` downloads and compiles MakeMKV at build time and no
image with MakeMKV in it is published anywhere. The build therefore requires
you to accept the [MakeMKV EULA](https://www.makemkv.com/eula/) explicitly
via a build arg.

Everything *except* MakeMKV — whipper, ffmpeg, mkvtoolnix, tesseract, pgsrip,
the compiled `ama` binary — lives in a separate, publishable `ama-base` image
that CI builds and pushes to `ghcr.io/sarumont/ama-base` on every merge to
`main` (see `.github/workflows/docker.yml` and `Dockerfile.base`'s header
comment). The top-level `Dockerfile` layers MakeMKV on top of it. Two ways to
build:

- **Pull `ama-base`, build MakeMKV locally (default, fast).** The plain
  `docker build` below pulls `ghcr.io/sarumont/ama-base:latest` for its base
  layer and only compiles MakeMKV itself.
- **Build everything from scratch, including `ama-base` (fully
  offline-auditable).** For anyone who doesn't want to trust the published
  base, build it yourself first and point the final build at that local tag:

  ```
  docker build -f Dockerfile.base -t ama-base:local .
  docker build --build-arg MAKEMKV_ACCEPT_EULA=yes \
               --build-arg AMA_BASE_IMAGE=ama-base:local \
               -t ama:latest .
  ```

```yaml
services:
  ama:
    image: ama:latest
    build:
      context: .
      args:
        MAKEMKV_ACCEPT_EULA: "yes"
    init: true          # let Docker inject tini as PID 1 for signal/zombie handling
    devices:
      - /dev/sr0:/dev/sr0
    group_add:
      # GID that owns /dev/sr0 on this host — run `stat -c %g /dev/sr0` to
      # find it. The image only bakes in the stable Debian/Ubuntu `cdrom`
      # GID (24); every other distro allocates this dynamically per install,
      # so it must be supplied here rather than guessed at build time.
      - "988"
    volumes:
      - /media/library:/media/library
      - /media/library/.ama-tmp:/tmp/ama   # keep in-progress rips on the
                                            # library filesystem, not the
                                            # container's writable layer
      - ./ama.yaml:/config/ama.yaml
    environment:
      AMA_CONFIG: /config/ama.yaml
      AMA_MAKEMKV_KEY: "${MAKEMKV_KEY}"
      AMA_TMDB_API_KEY: "${TMDB_API_KEY}"
      PUID: "1000"        # match the uid/gid that owns /media/library
      PGID: "1000"        # on the host
    ports:
      - "8080:8080"
```

The image's `HEALTHCHECK` curls `127.0.0.1:${AMA_WEB_PORT:-8080}` — it cannot
see a `web.port` set only in `ama.yaml`. If you change the listen port, set
`AMA_WEB_PORT` to match (as in the environment block above) or the
healthcheck will report `unhealthy` even though the server is up.

The container starts as root and `docker-entrypoint.sh` remaps the image's
`ama` user to `PUID`/`PGID` (default 1000:1000, the image's baked-in user)
before dropping privileges via `gosu` — no uid/gid baked into the image at
build time can match every host: a root-owned NAS mount, a second local
account, and so on. Only `ama`'s own directories (`/home/ama`, `/config`) are
re-owned on every container start; `/media/library` and `/tmp/ama` are not
recursively `chown`ed, since that is normally a large volume and would slow
every start down. Set `PUID`/`PGID` to match whatever already owns the
library on the host instead of relying on AMA to fix it up.

Then `docker compose build && docker compose up -d`, or without compose:

```
docker build --build-arg MAKEMKV_ACCEPT_EULA=yes -t ama:latest .
```

### Build Arguments

**Top-level `Dockerfile`:**

| Arg | Default | Purpose |
|---|---|---|
| `AMA_BASE_IMAGE` | `ghcr.io/sarumont/ama-base:latest` | Base image the final image is layered on. Point at a locally built `ama-base:local` to build fully from scratch |
| `MAKEMKV_ACCEPT_EULA` | `no` | Must be `yes`; the build fails otherwise |
| `MAKEMKV_VERSION` | `1.18.4` | MakeMKV beta builds expire ~60 days after release — bump this and rebuild when `makemkvcon` reports an expired version |

**`Dockerfile.base` (only needed if building `ama-base` yourself):**

| Arg | Default | Purpose |
|---|---|---|
| `TESSERACT_LANGS` | `eng osd fra deu spa ita jpn` | Tesseract language packs bundled into the image |
| `PGSRIP_VERSION` | `0.1.12` | pgsrip release used for PGS → SRT |
| `WHIPPER_VERSION` | `0.10.0-5` | Exact Debian package version of whipper. Pinned rather than a bare `apt-get install whipper` for reproducibility and to match the whipper 0.10.0 output shape `internal/testutil/testdata/examples/whipper/` fixtures assume (#9) |

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

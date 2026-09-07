# syntax=docker/dockerfile:1

# AMA — Automated Media Archiver
#
# Single container, single daemon process. This image bundles every external
# tool the pipeline shells out to: makemkvcon, whipper, ffmpeg/ffprobe,
# mkvmerge/mkvpropedit, tesseract and pgsrip.
#
# IMPORTANT — this image cannot be published to a public registry.
# MakeMKV's binary package (makemkv-bin) is not freely redistributable, so
# MakeMKV is downloaded and built *at image build time* and every user builds
# their own image locally. Building requires accepting the MakeMKV EULA:
#
#     docker build --build-arg MAKEMKV_ACCEPT_EULA=yes -t ama:latest .
#
# The MakeMKV license key is NEVER baked into the image; it is supplied at
# runtime via AMA_MAKEMKV_KEY (see docs/CONFIG.md).


###############################################################################
# Stage 1 — MakeMKV
#
# makemkv-oss (GPL, source) + makemkv-bin (proprietary, redistribution-
# restricted) are downloaded from makemkv.com, verified against the GPG-signed
# sha256sums file published alongside them, and compiled here. Everything is
# staged into /out via DESTDIR so the runtime image gets the artifacts without
# any of the build toolchain.
#
# Approach follows automatic-ripping-machine's install_makemkv.sh, which in
# turn derives from tianon/dockerfiles' makemkv image (MIT).
###############################################################################
FROM debian:trixie-slim AS makemkv-builder

# MakeMKV beta builds stop working ~60 days after release. Bump this (and
# rebuild) when makemkvcon starts reporting an expired version:
#   docker build --build-arg MAKEMKV_VERSION=x.y.z ...
ARG MAKEMKV_VERSION=1.18.4

# "MakeMKV (signature) <support@makemkv.com>"
ARG MAKEMKV_GPG_KEY=2ECF23305F1FC0B32001673394E3083A18042697

# Building makemkv-bin requires accepting the MakeMKV EULA. There is no way to
# do that on the builder's behalf, so the build refuses to proceed until it is
# passed explicitly.
ARG MAKEMKV_ACCEPT_EULA=no

RUN set -eux; \
    if [ "$MAKEMKV_ACCEPT_EULA" != "yes" ]; then \
        echo "ERROR: building MakeMKV requires accepting the MakeMKV EULA."; \
        echo "       See https://www.makemkv.com/eula/ then rebuild with:"; \
        echo "         docker build --build-arg MAKEMKV_ACCEPT_EULA=yes ..."; \
        exit 1; \
    fi

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        dirmngr \
        gnupg \
        libavcodec-dev \
        libavutil-dev \
        libexpat1-dev \
        libssl-dev \
        pkg-config \
        wget \
        zlib1g-dev; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Fetch and verify the signed checksum manifest before touching any tarball.
RUN set -eux; \
    wget -O sha256sums.txt.sig "https://www.makemkv.com/download/makemkv-sha-${MAKEMKV_VERSION}.txt"; \
    GNUPGHOME="$(mktemp -d)"; export GNUPGHOME; \
    gpg --batch --keyserver keyserver.ubuntu.com --recv-keys "$MAKEMKV_GPG_KEY"; \
    gpg --batch --decrypt --output sha256sums.txt sha256sums.txt.sig; \
    gpgconf --kill all; \
    rm -rf "$GNUPGHOME" sha256sums.txt.sig

# makemkv-oss ships an autotools build (--disable-gui keeps Qt out entirely —
# AMA only ever drives makemkvcon). makemkv-bin has no configure; its Makefile
# gates on an interactive EULA prompt which we satisfy with the marker file,
# having already required MAKEMKV_ACCEPT_EULA above.
RUN set -eux; \
    for ball in makemkv-oss makemkv-bin; do \
        tarball="${ball}-${MAKEMKV_VERSION}.tar.gz"; \
        wget -O "$tarball" "https://www.makemkv.com/download/${tarball}"; \
        sha256="$(grep "  ${ball}-${MAKEMKV_VERSION}[.]tar[.]gz$" sha256sums.txt | cut -d' ' -f1)"; \
        test -n "$sha256"; \
        echo "${sha256}  ${tarball}" | sha256sum -c -; \
        mkdir -p "$ball"; \
        tar -xf "$tarball" -C "$ball" --strip-components=1; \
        rm "$tarball"; \
        ( \
            cd "$ball"; \
            if [ -f configure ]; then \
                ./configure --prefix=/usr/local --disable-gui; \
            else \
                mkdir -p tmp; \
                echo accepted > tmp/eula_accepted; \
            fi; \
            make -j"$(nproc)" PREFIX=/usr/local; \
            make install PREFIX=/usr/local DESTDIR=/out; \
        ); \
        rm -rf "$ball"; \
    done; \
    rm -f sha256sums.txt; \
    test -x /out/usr/local/bin/makemkvcon; \
    test -f /out/usr/local/lib/libmakemkv.so.1


###############################################################################
# Stage 2 — pgsrip
#
# pgsrip is the PGS -> SRT driver used by internal/subtitle/ocr.go. It is a
# Python tool; it lives in its own venv so it cannot collide with whipper's
# system Python packages in the runtime image.
###############################################################################
FROM debian:trixie-slim AS pgsrip-builder

ARG PGSRIP_VERSION=0.1.12

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        python3 \
        python3-venv; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    python3 -m venv /opt/pgsrip; \
    /opt/pgsrip/bin/pip install --no-cache-dir --upgrade pip; \
    /opt/pgsrip/bin/pip install --no-cache-dir "pgsrip==${PGSRIP_VERSION}"; \
    /opt/pgsrip/bin/pgsrip --help >/dev/null


###############################################################################
# Stage 3 — ama binary
#
# Static (CGO_ENABLED=0) so the runtime stage only has to carry the binary.
###############################################################################
FROM golang:1.27-trixie AS go-builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/ama ./cmd/ama


###############################################################################
# Stage 4 — runtime
###############################################################################
FROM debian:trixie-slim AS runtime

# Tesseract language data bundled into the image. Per the resolution on #18 the
# set is fixed at build time and AMA fails fast if subtitle.ocr_languages names
# a language that is not present — nothing is installed at runtime.
#
# Default set (~35 MB): eng is mandatory; osd is tesseract's orientation/script
# detector; fra/spa ship on nearly every Region A retail Blu-ray; deu/ita cover
# the common Region B pressings; jpn covers anime releases. To bundle more,
# rebuild with e.g.
#   --build-arg TESSERACT_LANGS="eng osd fra deu spa ita jpn nld por swe"
# Package names are the Debian tesseract-ocr-<code> packages.
ARG TESSERACT_LANGS="eng osd fra deu spa ita jpn"

RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        eject \
        ffmpeg \
        mkvtoolnix \
        whipper \
        tesseract-ocr \
        $(for lang in $TESSERACT_LANGS; do echo "tesseract-ocr-$lang"; done) \
        python3 \
        default-jre-headless \
        libexpat1 \
        libssl3t64 \
        zlib1g \
        libgl1 \
        libglib2.0-0t64; \
    rm -rf /var/lib/apt/lists/*

# makemkvcon, mmgplsrv, mmccextr, libmakemkv/libdriveio/libmmbd and the
# MakeMKV appdata, built in stage 1.
COPY --from=makemkv-builder /out/usr/local/ /usr/local/
RUN ldconfig

COPY --from=pgsrip-builder /opt/pgsrip /opt/pgsrip
RUN ln -s /opt/pgsrip/bin/pgsrip /usr/local/bin/pgsrip

COPY --from=go-builder /out/ama /usr/local/bin/ama

# The container is started with `devices: - /dev/sr0:/dev/sr0`, and the device
# node keeps the host's owning GID. That GID differs per distro, so ama joins
# all of the common ones: cdrom (24, Debian/Ubuntu), optical (990, Arch) and
# GID 11 (Fedora's cdrom). On any other host, override with `group_add:` in
# compose.
RUN set -eux; \
    groupadd -g 1000 ama; \
    useradd -u 1000 -g ama -G cdrom,video -m -d /home/ama -s /usr/sbin/nologin ama; \
    groupadd -f -g 990 optical; usermod -aG optical ama; \
    groupadd -f -g 11 cdrom-fedora; usermod -aG cdrom-fedora ama; \
    mkdir -p /config /media/library /tmp/ama; \
    chown -R ama:ama /config /media/library /tmp/ama

ENV AMA_CONFIG=/config/ama.yaml \
    HOME=/home/ama \
    LANG=C.UTF-8 \
    TESSDATA_PREFIX=/usr/share/tesseract-ocr/5/tessdata

# /config      — ama.yaml (bind mount)
# /media/library — movie + music output roots (bind mount)
WORKDIR /home/ama
USER ama

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${AMA_WEB_PORT:-8080}/" >/dev/null || exit 1

# Exec form, no shell wrapper, so SIGTERM reaches the daemon directly.
ENTRYPOINT ["/usr/local/bin/ama"]

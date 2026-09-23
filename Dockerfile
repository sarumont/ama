# syntax=docker/dockerfile:1

# AMA — Automated Media Archiver — local image (adds MakeMKV)
#
# This image bundles every external tool the pipeline shells out to:
# makemkvcon, whipper, ffmpeg/ffprobe, mkvmerge/mkvpropedit, tesseract and
# pgsrip. Everything except MakeMKV lives in the publishable `ama-base`
# image (Dockerfile.base, published to ghcr.io/sarumont/ama-base by
# .github/workflows/docker.yml) — this file only adds MakeMKV on top of it.
#
# IMPORTANT — the *image this file produces* cannot be published to a public
# registry. MakeMKV's binary package (makemkv-bin) is not freely
# redistributable, so MakeMKV is downloaded and built *at image build time*
# and every user builds their own final image locally. Building requires
# accepting the MakeMKV EULA:
#
#     docker build --build-arg MAKEMKV_ACCEPT_EULA=yes -t ama:latest .
#
# By default this pulls `ghcr.io/sarumont/ama-base:latest` for the FROM in
# stage 2 below. To build fully offline / from scratch instead — e.g. if you
# don't want to trust the published base — build ama-base yourself first and
# point at it locally:
#
#     docker build -f Dockerfile.base -t ama-base:local .
#     docker build --build-arg MAKEMKV_ACCEPT_EULA=yes \
#                  --build-arg AMA_BASE_IMAGE=ama-base:local \
#                  -t ama:latest .
#
# The MakeMKV license key is NEVER baked into the image; it is supplied at
# runtime via AMA_MAKEMKV_KEY (see docs/CONFIG.md).

ARG AMA_BASE_IMAGE=ghcr.io/sarumont/ama-base:latest


###############################################################################
# Stage 1 — MakeMKV
#
# makemkv-oss (GPL, source) + makemkv-bin (proprietary, redistribution-
# restricted) are downloaded from makemkv.com, verified against the GPG-signed
# sha256sums file published alongside them, and compiled here. Everything is
# staged into /out via DESTDIR so the final image gets the artifacts without
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
    gpg --batch --keyserver hkps://keyserver.ubuntu.com --recv-keys "$MAKEMKV_GPG_KEY"; \
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
# Stage 2 — final image: ama-base + MakeMKV
###############################################################################
FROM ${AMA_BASE_IMAGE} AS runtime

# ama-base does not set USER (the container starts as root; see
# docker-entrypoint.sh), so no USER switch is needed here either — apt-get
# below and the entrypoint's own PUID/PGID remap both require root.

# MakeMKV-specific runtime dependencies. Everything else the final image
# needs (whipper, ffmpeg, mkvtoolnix, tesseract, pgsrip, the ama binary, the
# ama user, docker-entrypoint.sh, ENV/HEALTHCHECK/ENTRYPOINT) already comes
# from ama-base. These three are here, not in ama-base, because nothing but
# MakeMKV (built from source, outside apt) needs them — apt already pulls in
# whatever whipper/ffmpeg/mkvtoolnix/tesseract need on their own:
#   - libexpat1, libssl3t64, zlib1g: makemkvcon links against these (its
#     builder stage installs the matching -dev/headers packages).
#   - default-jre-headless: MakeMKV's BD-J support (blues.jar, installed by
#     makemkv-bin) needs a JRE. It's the single largest package pulled in by
#     this whole image; drop it here if BD-J discs turn out not to matter.
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        default-jre-headless \
        libexpat1 \
        libssl3t64 \
        zlib1g; \
    rm -rf /var/lib/apt/lists/*

# makemkvcon, mmgplsrv, mmccextr, libmakemkv/libdriveio/libmmbd and the
# MakeMKV appdata, built in stage 1.
COPY --from=makemkv-builder /out/usr/local/ /usr/local/
RUN ldconfig

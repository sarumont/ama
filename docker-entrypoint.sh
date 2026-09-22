#!/bin/sh
# The container starts as root (see the Dockerfile's USER comment) so this
# script can remap the `ama` user to the host's PUID/PGID before dropping
# privileges. No uid/gid baked in at build time can match every host — a
# root-owned NAS mount, a second local account, a distro whose id allocation
# differs — so the remap has to happen here instead. Defaults to 1000:1000,
# the image's baked-in `ama` user, when PUID/PGID are unset.
set -eu

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

if [ "$(id -u ama)" != "$PUID" ]; then
    usermod -o -u "$PUID" ama
fi
if [ "$(getent group ama | cut -d: -f3)" != "$PGID" ]; then
    groupmod -o -g "$PGID" ama
fi

# Only ama's own directories are re-owned here, not /media/library: that is
# normally a large bind-mounted library, and a recursive chown of it on every
# container start would be slow and is the operator's call, not this image's.
# Match PUID/PGID to whatever already owns the library on the host instead
# (see docs/CONFIG.md).
chown -R ama:ama /home/ama /config

exec gosu ama:ama "$@"

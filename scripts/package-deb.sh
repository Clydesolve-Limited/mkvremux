#!/usr/bin/env bash
#
# package-deb.sh - build the release .deb for mkvremux.
#
# Builds the release binary the RELEASING way (static, stripped), stages
# the package tree, and runs dpkg-deb. The result lands in
# dist/mkvremux_<version>_amd64.deb (dist/ is git-ignored).
#
# Usage:
#   scripts/package-deb.sh [--output-dir DIR]
#
# Options:
#   --output-dir DIR  Where the .deb is written (default: dist/,
#                     relative to the repository root).
#   -h, --help        This text.
#
# Design notes:
#   * Version is a SINGLE SOURCE: the built binary's own --version
#     output ("mkvremux version X.Y.Z"). Nothing else is consulted, so
#     the package version can never drift from the binary it wraps; the
#     parsed value must match ^[0-9]+\.[0-9]+\.[0-9]+$ or the script
#     fails loudly instead of producing a bogus filename.
#   * CGO_ENABLED=0 static build: no dynamic linker, therefore NO
#     Depends: field in the control file (no libc dependency).
#   * ffmpeg/ffprobe are runtime tools the binary locates on PATH, so
#     the control file declares Recommends: ffmpeg, not Depends (the
#     rationale sits next to the control file below).
#   * The packaged README is the repo README as checked out (README.md
#     IS the public document in this project; there is no transform
#     step).
#
set -euo pipefail

usage() {
  sed -n '2,31p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

OUTPUT_DIR=""
while [ $# -gt 0 ]; do
  case "$1" in
    --output-dir) [ $# -ge 2 ] || { echo "error: --output-dir needs a value" >&2; exit 2; }
                  OUTPUT_DIR="$2"; shift 2 ;;
    --output-dir=*) OUTPUT_DIR="${1#--output-dir=}"; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            echo "error: unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done

command -v dpkg-deb >/dev/null 2>&1 || {
  echo "error: dpkg-deb not found on PATH (install dpkg / dpkg-dev)" >&2; exit 1; }
command -v go >/dev/null 2>&1 || {
  echo "error: go not found on PATH" >&2; exit 1; }

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: not inside a git repository" >&2; exit 1; }
cd "$REPO_ROOT"

if [ -z "$OUTPUT_DIR" ]; then
  OUTPUT_DIR="$REPO_ROOT/dist"
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mkvremux-deb-XXXXXX")"
on_exit() {
  local rc=$?
  if [ "$rc" -eq 0 ]; then
    rm -rf "$WORK"
  else
    echo "note: work dir kept for inspection: $WORK" >&2
  fi
}
trap on_exit EXIT

# ---------------------------------------------------------------------------
# 1. Release binary (the RELEASING build: static + stripped)
# ---------------------------------------------------------------------------
echo "[1/5] build: CGO_ENABLED=0 go build -ldflags '-s -w' ."
CGO_ENABLED=0 go build -ldflags "-s -w" -o "$WORK/mkvremux" .

# ---------------------------------------------------------------------------
# 2. Version: SINGLE SOURCE = the built binary's own --version output
#    ("mkvremux version X", toolVersion via kong).
# ---------------------------------------------------------------------------
echo "[2/5] version from binary --version"
RAW_VERSION="$("$WORK/mkvremux" --version | sed -n 's/^mkvremux version //p')"
[ -n "$RAW_VERSION" ] || {
  echo "error: could not parse 'mkvremux version X' from: $("$WORK/mkvremux" --version)" >&2; exit 1; }

# Sanity: the control file and the output filename embed this value, so
# reject anything that is not a plain X.Y.Z (garbage in, garbage package
# version out) instead of letting dpkg-deb fail later with a cryptic
# changelog/version error.
if ! printf '%s\n' "$RAW_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "error: version '$RAW_VERSION' is not a plain X.Y.Z number" >&2; exit 1
fi

# Debian upstream-version sanitisation: Debian versions may not contain
# '-' (dpkg reads it as the end of the upstream version and the start of
# a Debian revision), so map '-' to '~'. '~' also sorts BEFORE the plain
# release, which is the correct Debian ordering for a pre-release: a
# future 0.2.0-dev becomes 0.2.0~dev and sorts before 0.2.0.
PKG_VERSION="${RAW_VERSION//-/~}"
echo "  upstream: $RAW_VERSION -> package: $PKG_VERSION"

# ---------------------------------------------------------------------------
# 3. Stage the package tree
# ---------------------------------------------------------------------------
echo "[3/5] stage package tree"
STAGE="$WORK/stage"
mkdir -p "$STAGE/DEBIAN" \
         "$STAGE/usr/bin" \
         "$STAGE/usr/share/doc/mkvremux"

install -m 0755 "$WORK/mkvremux" "$STAGE/usr/bin/mkvremux"
# README.md IS the public document in this project (no README-public.md,
# no transform step): ship it byte-identically as checked out.
install -m 0644 "$REPO_ROOT/README.md" "$STAGE/usr/share/doc/mkvremux/README.md"

# MIT copyright for the installed package. Unlike Apache-2.0 in the
# isopress package (whose ~11 KB licence text is referenced by URL
# instead), the MIT text is ~1 KB and Debian policy wants the licence
# terms present in the file, so it is reproduced in full here; the
# canonical source URL is carried for provenance. MIT has no NOTICE
# equivalent, so there is no separate notice file to ship alongside.
# Maintainer carries no email address on purpose (house rule: the site
# scans ban address patterns, and dpkg-deb does not require a valid one).
cat > "$STAGE/usr/share/doc/mkvremux/copyright" <<'EOF'
Package: mkvremux
Upstream name: mkvremux
Source: https://github.com/Clydesolve-Limited/mkvremux

Copyright: 2026 Martin Donnelly, ClydeSolve Limited

Licence: MIT
 MIT License

 Copyright (c) 2026 Martin Donnelly, ClydeSolve Limited

 Permission is hereby granted, free of charge, to any person obtaining a copy
 of this software and associated documentation files (the "Software"), to deal
 in the Software without restriction, including without limitation the rights
 to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 copies of the Software, and to permit persons to whom the Software is
 furnished to do so, subject to the following conditions:

 The above copyright notice and this permission notice shall be included in all
 copies or substantial portions of the Software.

 THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 SOFTWARE.
EOF

# Installed-Size = KiB of the files the package installs (usr/ only;
# DEBIAN/ is control data and is not counted).
INSTALLED_SIZE="$(du -sk "$STAGE/usr" | awk '{print $1}')"

# Depends is deliberately ABSENT: the binary is CGO_ENABLED=0 static, so
# there is no libc or any other shared-library dependency to declare.
cat > "$STAGE/DEBIAN/control" <<EOF
Package: mkvremux
Version: $PKG_VERSION
Section: video
Priority: optional
Architecture: amd64
Maintainer: Martin Donnelly
Installed-Size: $INSTALLED_SIZE
Recommends: ffmpeg
Description: opinionated MKV remuxer for DVR recording libraries
 mkvremux batch-remuxes a folder tree of DVR recordings into MKV
 containers: it walks a source directory, repackages every matching file
 into a .mkv under the destination, and organises the results into
 season folders that a media library can consume directly. It follows
 the DVR workflow it serves (HDHomeRun tuner captures managed by
 NextPVR), repairing broken transport streams with -fflags
 +genpts+igndts on the way. ffmpeg and ffprobe are required on PATH at
 runtime; see Recommends.
EOF

# Recommends: ffmpeg justification - the binary shells out to ffmpeg and
# ffprobe, looked up on PATH, and its preflight fails with exit 2 plus
# the clear install hint before any file is touched (README
# "Requirements"), so ffmpeg is a genuine runtime dependency; but a hard
# Depends would force apt's ffmpeg onto users who ship their own static
# build on PATH. The tool's own floor check probes PATH at startup and
# prints the install hint, which is a better error than a hard Depends
# that drags apt's ffmpeg into installs that already have a working
# ffmpeg.

# Explicit modes for everything the package ships, independent of umask.
# CI containers commonly run with `umask 000`, under which `mkdir -p`
# creates 0777 directories and the heredoc-created control/copyright
# files land as 0666: dpkg-deb rejects a control directory outside
# 0755..0775 ("control directory has bad permissions 777"), and even
# below that check the archive would ship world-writable contents.
# Setting every mode here makes the package identical no matter what
# umask the caller (or runner) has.
find "$STAGE" -type d -exec chmod 0755 {} +
chmod 0755 "$STAGE/usr/bin/mkvremux"
chmod 0644 "$STAGE/usr/share/doc/mkvremux/README.md" \
           "$STAGE/usr/share/doc/mkvremux/copyright"
chmod 0644 "$STAGE/DEBIAN/control"

# ---------------------------------------------------------------------------
# 4. Build the .deb
# ---------------------------------------------------------------------------
echo "[4/5] dpkg-deb --build"
mkdir -p "$OUTPUT_DIR"
DEB_PATH="$OUTPUT_DIR/mkvremux_${PKG_VERSION}_amd64.deb"
dpkg-deb --root-owner-group --build "$STAGE" "$DEB_PATH" >/dev/null
chmod 0644 "$DEB_PATH"

# ---------------------------------------------------------------------------
# 5. Report
# ---------------------------------------------------------------------------
echo "[5/5] done"
echo "  package : mkvremux $PKG_VERSION (amd64, static, Installed-Size: ${INSTALLED_SIZE}k)"
echo "  output  : $DEB_PATH"
ls -lh "$DEB_PATH" | awk '{print "  size    : " $5}'

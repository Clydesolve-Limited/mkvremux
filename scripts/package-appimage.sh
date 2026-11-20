#!/usr/bin/env bash
#
# package-appimage.sh - build the release .AppImage for mkvremux.
#
# Stages an AppDir (AppRun + static binary + desktop entry + icon),
# packs it with appimagetool (AppImage project continuous release),
# and writes dist/mkvremux-<version>-amd64.AppImage (dist/ is
# git-ignored).
#
# Usage:
#   scripts/package-appimage.sh [--output-dir DIR]
#
# Options:
#   --output-dir DIR  Where the .AppImage is written (default: dist/,
#                     relative to the repository root).
#   -h, --help        This text.
#
# Design notes:
#   * Version is a SINGLE SOURCE: the built binary's own --version
#     output ("mkvremux version X.Y.Z"), parsed with sed and sanity-
#     checked against ^[0-9]+\.[0-9]+\.[0-9]+$; an unparsable or
#     malformed version fails loudly instead of producing a bogus
#     filename.
#   * CGO_ENABLED=0 static build: the AppImage runs on any x86_64
#     Linux regardless of the host libc.
#   * appimagetool is cached under dist/.cache/ and run with
#     APPIMAGE_EXTRACT_AND_RUN=1 so no FUSE device is needed (CI
#     runners typically have no /dev/fuse).
#   * CI hardening for the pack step: stdin is closed (< /dev/null) so
#     an interactive prompt on an error path fails instead of hanging;
#     the pack runs under timeout(1) with a hard wall-clock bound
#     (default 300s, override with APPIMAGETOOL_TIMEOUT=<seconds>) so a
#     hang fails loudly with exit 124; and --no-appstream suppresses
#     appimagetool's AppStream warning, which is expected noise here
#     (this CLI tool ships no AppStream metadata by design).
#
set -euo pipefail

usage() {
  sed -n '2,36p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
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

for tool in go curl rsvg-convert desktop-file-validate timeout; do
  command -v "$tool" >/dev/null 2>&1 || {
    if [ "$tool" = timeout ]; then
      echo "error: timeout not found on PATH (coreutils; Debian/Ubuntu: apt install coreutils)" >&2
    else
      echo "error: $tool not found on PATH (Debian/Ubuntu: apt install librsvg2-bin desktop-file-utils curl)" >&2
    fi
    exit 1; }
done

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: not inside a git repository" >&2; exit 1; }
cd "$REPO_ROOT"

if [ -z "$OUTPUT_DIR" ]; then
  OUTPUT_DIR="$REPO_ROOT/dist"
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mkvremux-appimage-XXXXXX")"
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
# ---------------------------------------------------------------------------
echo "[2/5] version from binary --version"
VERSION="$("$WORK/mkvremux" --version | sed -n 's/^mkvremux version //p')"
[ -n "$VERSION" ] || {
  echo "error: could not parse 'mkvremux version X' from --version output" >&2; exit 1; }
# Sanity: the filename and metadata embed this value, so reject
# anything that is not a plain X.Y.Z (garbage in, garbage filename out).
if ! printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "error: version '$VERSION' is not a plain X.Y.Z number" >&2; exit 1
fi
echo "  version: $VERSION"

# ---------------------------------------------------------------------------
# 3. Stage the AppDir
# ---------------------------------------------------------------------------
echo "[3/5] stage AppDir"
APPDIR="$WORK/AppDir"
mkdir -p "$APPDIR"

# AppRun: fixed relative exec so the AppImage works from any cwd.
cat > "$APPDIR/AppRun" <<'EOF'
#!/bin/sh
exec "$(dirname "$0")/mkvremux" "$@"
EOF
chmod 0755 "$APPDIR/AppRun"

install -m 0755 "$WORK/mkvremux" "$APPDIR/mkvremux"

# Desktop entry (validated BEFORE packing; the CI job installs
# desktop-file-utils for this).
desktop-file-validate "$REPO_ROOT/packaging/mkvremux.desktop"
install -m 0644 "$REPO_ROOT/packaging/mkvremux.desktop" "$APPDIR/mkvremux.desktop"

# Icon: SVG source -> 256x256 PNG (appimagetool wants a raster icon
# named after the desktop Icon= key) + .DirIcon preview copy.
rsvg-convert -w 256 -h 256 "$REPO_ROOT/packaging/icon.svg" -o "$APPDIR/mkvremux.png"
cp "$APPDIR/mkvremux.png" "$APPDIR/.DirIcon"

# Explicit modes for everything the AppDir ships, independent of umask.
# CI containers commonly run with `umask 000`, under which `mkdir -p`
# creates 0777 directories and the staged files land as 0666 (mksquashfs
# records these modes in the squashfs, so the artifact would ship a
# world-writable AppRun and binary). Setting every mode here makes the
# AppImage contents identical no matter what umask the caller has.
find "$APPDIR" -type d -exec chmod 0755 {} +
chmod 0755 "$APPDIR/AppRun" "$APPDIR/mkvremux"
chmod 0644 "$APPDIR/mkvremux.desktop" "$APPDIR/mkvremux.png" "$APPDIR/.DirIcon"

# ---------------------------------------------------------------------------
# 4. appimagetool (cached under dist/.cache/) + pack
# ---------------------------------------------------------------------------
echo "[4/5] appimagetool"
TOOL="$REPO_ROOT/dist/.cache/appimagetool-x86_64.AppImage"
TOOL_URL="https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage"
mkdir -p "$REPO_ROOT/dist/.cache"
if [ ! -x "$TOOL" ]; then
  echo "  downloading $TOOL_URL"
  curl -fL --retry 3 --retry-delay 2 -o "$TOOL.part" "$TOOL_URL"
  mv "$TOOL.part" "$TOOL"
  chmod +x "$TOOL"
else
  echo "  cached: $TOOL"
fi

# appimagetool rewrites mkvremux.desktop itself (injects
# X-AppImage-Version) using default create mode 0666 & ~umask, so under
# a `umask 000` CI runner the file would land 0666 inside the squashfs
# AFTER the explicit AppDir chmods above. Pin the umask for the pack
# step so the tool's own file creation is deterministic too.
umask 022

mkdir -p "$OUTPUT_DIR"
APPIMAGE_PATH="$OUTPUT_DIR/mkvremux-$VERSION-amd64.AppImage"
rm -f "$APPIMAGE_PATH"

# Hard wall-clock bound for the pack step; override with
# APPIMAGETOOL_TIMEOUT=<seconds> (0 is not allowed: timeout(1) treats it
# as "disable the limit").
APPIMAGETOOL_TIMEOUT="${APPIMAGETOOL_TIMEOUT:-300}"
case "$APPIMAGETOOL_TIMEOUT" in
  ''|*[!0-9]*|0)
    echo "error: APPIMAGETOOL_TIMEOUT must be a positive integer of seconds (got: '$APPIMAGETOOL_TIMEOUT')" >&2
    exit 2 ;;
esac

# Packing hardening (see header):
#   * APPIMAGE_EXTRACT_AND_RUN=1: unpack and run without FUSE (no
#     /dev/fuse on CI runners); VERSION is embedded as metadata.
#   * --no-appstream: no AppStream metadata is shipped by design, so the
#     check's warning is expected noise (verified against the cached
#     appimagetool continuous build: it supports the flag).
#   * < /dev/null: CI has no TTY; an interactive prompt on an error path
#     must fail fast, not hang waiting for input.
#   * timeout: converts a silent appimagetool/mksquashfs hang into a
#     loud failure (exit 124) with the partial output above still
#     visible; --kill-after escalates to SIGKILL if the child ignores
#     SIGTERM.
rc=0
VERSION="$VERSION" APPIMAGE_EXTRACT_AND_RUN=1 \
  timeout --kill-after=30 "$APPIMAGETOOL_TIMEOUT" \
  "$TOOL" --no-appstream "$APPDIR" "$APPIMAGE_PATH" < /dev/null || rc=$?
if [ "$rc" -eq 124 ]; then
  echo "error: appimagetool timed out after ${APPIMAGETOOL_TIMEOUT}s - see output above" >&2
  exit 124
fi
if [ "$rc" -ne 0 ]; then
  echo "error: appimagetool exited with status $rc" >&2
  exit "$rc"
fi
chmod +x "$APPIMAGE_PATH"

# ---------------------------------------------------------------------------
# 5. Report
# ---------------------------------------------------------------------------
echo "[5/5] done"
echo "  output  : $APPIMAGE_PATH"
file "$APPIMAGE_PATH" | sed 's/^/  file    : /'
ls -lh "$APPIMAGE_PATH" | awk '{print "  size    : " $5}'

#!/usr/bin/env bash
#
# release.sh - cut an mkvremux release end to end.
#
# The automated form of the release checklist: it runs the full gate
# list (verify, lint, vulncheck, test), builds both dist/ artifacts,
# captures their sha256sum, creates the ANNOTATED tag vX.Y.Z (version
# parsed from the built binary's own --version output - the single
# version source, same as package-deb.sh), pushes the tag to the
# internal origin, and mirrors it to GitHub through scripts/publish.sh.
#
# The tag message IS the release note (no changelog file in this
# project). It is composed of exactly:
#   mkvremux vX.Y.Z
#   (blank line)
#   Gates: verify, lint, vulncheck, test green (YYYY-MM-DD)
#     - or "gates skipped per RELEASE_SKIP_GATES (YYYY-MM-DD)" when the
#       gates were skipped, so the record never lies -
#   (blank line)
#   <sha256>  dist/mkvremux_<version>_amd64.deb
#   <sha256>  dist/mkvremux-<version>-amd64.AppImage
#
# Usage:
#   scripts/release.sh [--url GITHUB_URL] [--dry-run]
#   scripts/release.sh --help
#   make release [RELEASE_FLAGS="--dry-run"]
#
# Options:
#   --url URL        Public GitHub remote (also settable via env
#                    GITHUB_URL), passed through to scripts/publish.sh.
#                    When unset this is NOT a failure: the internal tag
#                    and its push still happen (a complete, honest
#                    state on their own) and the script prints the exact
#                    follow-up command to mirror later.
#   --dry-run        Run the gates and the artifacts, print the composed
#                    tag message and the git commands that WOULD run;
#                    create no tag, push nothing. A dirty working tree
#                    warns and continues here; a real run still refuses.
#   -h, --help       This text.
#
# Environment:
#   GITHUB_URL       Same as --url (passed through to publish.sh).
#   RELEASE_SKIP_GATES=1
#                    Skip the gate run. ONLY for re-running immediately
#                    after a gate run you trust; CI re-runs gates
#                    anyway. The tag message then records the skip
#                    instead of claiming green gates.
#
# Ordering safety: EVERYTHING is validated before any git write - a
# clean working tree (a release tags committed HEAD), the tag absent
# locally AND on origin, a strict X.Y.Z version, and both artifacts
# present after the build - and the gates run before those writes too,
# so a failing gate aborts before a tag exists anywhere.
#
# The toolVersion bump for the next release is NEVER performed here;
# a reminder line pointing at const toolVersion in main.go (the single
# version source) is printed instead.
#
set -euo pipefail

usage() {
  sed -n '2,57p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

# Placeholder public remote, used ONLY to compose the follow-up mirror
# command under --dry-run when no --url/GITHUB_URL is given (the same
# URL publish.sh substitutes as its own placeholder).
PLACEHOLDER_URL="https://github.com/Clydesolve-Limited/mkvremux"

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
GITHUB_URL="${GITHUB_URL:-}"
DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --url)      [ $# -ge 2 ] || { echo "error: --url needs a value" >&2; exit 2; }
                GITHUB_URL="$2"; shift 2 ;;
    --url=*)    GITHUB_URL="${1#--url=}"; shift ;;
    --dry-run)  DRY_RUN=1; shift ;;
    -h|--help)  usage; exit 0 ;;
    *)          echo "error: unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# The gate list, in order. This project has exactly these four gates
# (the Makefile's documented set, README gate table): there are NO
# e2e/perf/race tiers here, so none are invented for the release record.
# ---------------------------------------------------------------------------
GATES=(verify lint vulncheck test)
GATES_CSV=""
for g in "${GATES[@]}"; do GATES_CSV+="${GATES_CSV:+, }$g"; done

for tool in git make go sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "error: $tool not found on PATH" >&2; exit 1; }
done

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: not inside a git repository" >&2; exit 1; }
cd "$REPO_ROOT"

# ---------------------------------------------------------------------------
# (a) Clean working tree: a release tags committed HEAD and hashes the
#     artifacts built from it, so nothing uncommitted may ride along.
#     A REAL run refuses outright. --dry-run warns and continues (the
#     publish.sh convention): a dry run writes nothing, so it may
#     inspect the tree it is actually given.
# ---------------------------------------------------------------------------
STATUS="$(git status --porcelain)"
if [ -n "$STATUS" ]; then
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "warning: working tree is dirty; --dry-run continues against committed HEAD." >&2
    printf '%s\n' "$STATUS" >&2
  else
    {
      echo "error: refusing to release from a dirty working tree."
      echo "A release tags committed HEAD and hashes artifacts built from it;"
      echo "commit (or stash) first, then re-run. Dirty entries:"
      printf '%s\n' "$STATUS"
    } >&2
    exit 1
  fi
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mkvremux-release-XXXXXX")"
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
# Version: SINGLE SOURCE = the built binary's own --version output
# ("mkvremux version X", const toolVersion in main.go via kong - the
# same parse as package-deb.sh). This is the release-artifact build
# (static + stripped), built up front so a bad version or an existing
# tag fails in seconds, not after the full gate run.
# ---------------------------------------------------------------------------
echo "== version (single source: the built binary's --version) =="
CGO_ENABLED=0 go build -ldflags "-s -w" -o "$WORK/mkvremux" .
RAW_VERSION="$("$WORK/mkvremux" --version | sed -n 's/^mkvremux version //p')"
[ -n "$RAW_VERSION" ] || {
  echo "error: could not parse 'mkvremux version X' from: "$("$WORK/mkvremux" --version)"" >&2; exit 1; }
if ! printf '%s\n' "$RAW_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  {
    echo "error: binary --version reports '$RAW_VERSION', not a release X.Y.Z."
    echo "A release tag must be vX.Y.Z: set const toolVersion in main.go (the"
    echo "single version source) to X.Y.Z, commit, then re-run."
  } >&2
  exit 1
fi
VERSION="$RAW_VERSION"
TAG="v$VERSION"
echo "  version: $VERSION -> tag $TAG"

# ---------------------------------------------------------------------------
# (b) The tag must not exist locally or on origin.
# ---------------------------------------------------------------------------
echo "== tag preconditions ($TAG) =="
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  {
    echo "error: tag $TAG already exists locally (points at $(git rev-parse --short "$TAG^{}"))."
    echo "Refusing: a release tags a new version. If this tag must be re-cut,"
    echo "delete it deliberately (git tag -d $TAG) after deciding what to do"
    echo "with the copy already on origin. Nothing was tagged or pushed."
  } >&2
  exit 1
fi
if ! git remote get-url origin >/dev/null 2>&1; then
  echo "error: no 'origin' remote configured; the internal push needs it." >&2
  exit 1
fi
if ! ORIGIN_REFS="$(git ls-remote --tags origin "refs/tags/$TAG")"; then
  {
    echo "error: could not query origin for existing tags (git ls-remote failed)."
    echo "Fix remote access and re-run; nothing was written."
  } >&2
  exit 1
fi
if [ -n "$ORIGIN_REFS" ]; then
  {
    echo "error: tag $TAG already exists on origin:"
    printf '%s\n' "$ORIGIN_REFS"
    echo "Refusing: nothing was tagged or pushed."
  } >&2
  exit 1
fi
echo "  absent locally and on origin: OK"

# ---------------------------------------------------------------------------
# (c) GITHUB_URL is optional: the internal tag alone is a complete,
#     honest state - the internal push and the mirror split, mirror
#     later. Not a failure.
# ---------------------------------------------------------------------------
if [ -z "$GITHUB_URL" ]; then
  cat <<EOF

NOTE: no GITHUB_URL set - the internal tag push and the GitHub mirror are SPLIT.
  This run creates and pushes the internal tag only (a complete,
  honest state on its own); mirror to GitHub later with:
    scripts/publish.sh --tag $TAG
EOF
fi
echo

# ---------------------------------------------------------------------------
# Gates: ordered, timed, PASS/FAIL per gate, abort on the first failure
# with that gate's log tail - always before any tag or push exists.
# ---------------------------------------------------------------------------
GATES_RAN=1
if [ "${RELEASE_SKIP_GATES:-0}" = "1" ]; then
  GATES_RAN=0
  echo "=============================================================="
  echo "!! WARNING: RELEASE_SKIP_GATES=1 - GATES SKIPPED:"
  echo "!!   $GATES_CSV"
  echo "!! Only for re-running immediately after a gate run you trust;"
  echo "!! CI re-runs gates anyway. The tag message records the skip."
  echo "=============================================================="
else
  echo "== gates (${#GATES[@]}) =="
  mkdir -p "$WORK/gates"
  i=0
  for gate in "${GATES[@]}"; do
    i=$((i + 1))
    echo
    echo "[$i/${#GATES[@]}] make $gate"
    t0="$(date +%s)"
    if make "$gate" 2>&1 | tee "$WORK/gates/$gate.log"; then
      echo "  PASS $gate ($(( $(date +%s) - t0 ))s)"
    else
      rc=$?
      echo "  FAIL $gate (exit $rc, $(( $(date +%s) - t0 ))s)" >&2
      echo "--- last 40 lines of the make $gate log ($WORK/gates/$gate.log) ---" >&2
      tail -n 40 "$WORK/gates/$gate.log" >&2
      echo "--- end of log ---" >&2
      echo "error: gate '$gate' failed - aborting before any tag or push." >&2
      exit 1
    fi
  done
  echo
  echo "all gates green: $GATES_CSV ($(date +%F))"
fi

# ---------------------------------------------------------------------------
# Artifacts: both packages, then both hashes. (d) the artifacts must
# EXIST after the build - their sha256s are the release note, so a build
# that produced nothing must never be tagged.
# ---------------------------------------------------------------------------
echo "== artifacts =="
make deb
make appimage
DEB="dist/mkvremux_${VERSION}_amd64.deb"
APPIMAGE="dist/mkvremux-${VERSION}-amd64.AppImage"
for f in "$DEB" "$APPIMAGE"; do
  if [ ! -f "$f" ]; then
    {
      echo "error: expected artifact missing after 'make deb'/'make appimage': $f"
      echo "Refusing to tag: both sha256s are the release note, so both files"
      echo "must exist. Nothing was tagged or pushed."
    } >&2
    exit 1
  fi
done
SHA256_LINES="$(sha256sum "$DEB" "$APPIMAGE")"
printf '%s\n' "$SHA256_LINES" | sed 's/^/  /'

# ---------------------------------------------------------------------------
# Tag message = the release note (no changelog file; ASCII only).
# ---------------------------------------------------------------------------
TODAY="$(date +%F)"
if [ "$GATES_RAN" -eq 1 ]; then
  GATE_LINE="Gates: ${GATES_CSV} green (${TODAY})"
else
  GATE_LINE="gates skipped per RELEASE_SKIP_GATES (${TODAY})"
fi
TAG_MSG="mkvremux ${TAG}

${GATE_LINE}

${SHA256_LINES}"

# ---------------------------------------------------------------------------
# Dry run: show the message and the commands; write nothing.
# ---------------------------------------------------------------------------
if [ "$DRY_RUN" -eq 1 ]; then
  echo
  echo "== dry-run: composed tag message (would be the release note) =="
  printf '%s\n' "$TAG_MSG"
  echo
  echo "== dry-run: git commands that WOULD run =="
  echo "  git tag -a $TAG -F <the message file above>"
  echo "  git push origin $TAG"
  if [ -n "$GITHUB_URL" ]; then
    echo "  GITHUB_URL=$GITHUB_URL ./scripts/publish.sh --tag $TAG"
  else
    echo "  (no GITHUB_URL - internal push only; mirror later with:)"
    echo "  GITHUB_URL=$PLACEHOLDER_URL ./scripts/publish.sh --tag $TAG"
    echo "    ^ PLACEHOLDER remote - no --url/GITHUB_URL given; substitute the"
    echo "      real one for a real mirror (or run publish.sh's own --dry-run)."
  fi
  echo
  echo "== dry-run summary =="
  echo "version : $VERSION"
  if [ "$GATES_RAN" -eq 1 ]; then
    echo "gates   : all green ($GATES_CSV, ${TODAY})"
  else
    echo "gates   : SKIPPED (RELEASE_SKIP_GATES=1)"
  fi
  echo "artifacts:"
  printf '%s\n' "$SHA256_LINES" | sed 's/^/  /'
  echo "tag     : NOT created (dry-run); nothing pushed."
  echo "reminder: after tagging, bump const toolVersion in main.go (the single"
  echo "          version source) for the next release - this script never edits it."
  exit 0
fi

# ---------------------------------------------------------------------------
# Tag + push to the internal origin - the first git writes, after every
# validation above.
# ---------------------------------------------------------------------------
echo "== tag =="
MSG_FILE="$WORK/tag-message.txt"
printf '%s\n' "$TAG_MSG" > "$MSG_FILE"
git tag -a "$TAG" -F "$MSG_FILE"
echo "  created: $TAG -> $(git rev-parse --short "$TAG^{}") (annotated)"

echo "== push to origin =="
if ! git push origin "$TAG"; then
  {
    echo "error: $TAG was created in this clone but the push to origin failed."
    echo "Nothing is on the remote. Push it when origin is reachable:"
    echo "  git push origin $TAG"
  } >&2
  exit 1
fi
ORIGIN_REFS="$(git ls-remote --tags origin "refs/tags/$TAG")"
if [ -z "$ORIGIN_REFS" ]; then
  echo "error: push reported success but $TAG is not visible on origin; investigate before mirroring." >&2
  exit 1
fi
echo "  pushed + verified: $TAG on origin ($(git remote get-url origin))"

# ---------------------------------------------------------------------------
# Mirror to GitHub via scripts/publish.sh (snapshot tag now; the GitHub
# Release appears when the workflow runs on that tag).
# ---------------------------------------------------------------------------
if [ -n "$GITHUB_URL" ]; then
  echo "== mirror to GitHub (scripts/publish.sh --tag $TAG) =="
  if ! GITHUB_URL="$GITHUB_URL" ./scripts/publish.sh --tag "$TAG"; then
    {
      echo "error: the internal release is complete (tag $TAG on origin), but the"
      echo "GitHub mirror failed. Re-run the mirror when ready:"
      echo "  GITHUB_URL=$GITHUB_URL ./scripts/publish.sh --tag $TAG"
    } >&2
    exit 1
  fi
else
  echo
  echo "== mirror to GitHub =="
  echo "not run (no GITHUB_URL). Mirror with:"
  echo "  scripts/publish.sh --tag $TAG"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo
echo "== release summary =="
echo "version   : $VERSION (tag $TAG)"
if [ "$GATES_RAN" -eq 1 ]; then
  echo "gates     : all green ($GATES_CSV, ${TODAY})"
else
  echo "gates     : SKIPPED (RELEASE_SKIP_GATES=1)"
fi
echo "artifacts :"
printf '%s\n' "$SHA256_LINES" | sed 's/^/            /'
echo "tag       : created + pushed to origin"
if [ -n "$GITHUB_URL" ]; then
  echo "github    : mirrored via scripts/publish.sh --tag $TAG"
else
  echo "github    : not mirrored yet - run: scripts/publish.sh --tag $TAG"
fi
echo "reminder  : bump const toolVersion in main.go (the single version source)"
echo "            for the next release - this script never edits it."

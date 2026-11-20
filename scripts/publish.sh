#!/usr/bin/env bash
#
# publish.sh - push a SANITIZED, single-commit snapshot of this repository
# to a public GitHub remote (https://github.com/Clydesolve-Limited/mkvremux).
#
# The snapshot is one squashed commit per run ("snapshot: YYYY-MM-DD (from
# <internal-sha8>)"). Internal history, internal docs and machine-local
# paths never cross the boundary; only the publish set does.
#
# Publish set = every tracked file EXCEPT these 13 internal docs:
#   README-INTERNAL.md, WORK_LOG.md, NOTES.md, TODO.md, notes.md,
#   REVIEW_LOG.md, REVIEW_NOTES.md, DEV_LOG.md, DEV_NOTES.md, RELEASING.md,
#   TEST_NOTES.md, TEST_WORK_LOG.md,
#   .idea/ (belt and braces - already git-ignored)
# i.e. README.md (the public document) + Go sources/tests + go.mod/go.sum +
# LICENSE + Makefile + .golangci.yml + .gitignore (+ this script once
# committed). notes.md is tracked but excluded with the rest of the
# internal notes. TEST_NOTES.md/TEST_WORK_LOG.md (test-gate artifacts,
# currently untracked) are excluded defensively so a later commit can
# never leak them into a snapshot.
#
# README handling (deliberately inverted vs the isopress script): there is
# NO README-public.md and NO install/rename step - README.md IS the public
# document. Stage [3/7] replaces the export's README.md with the checkout's
# copy byte for byte (no rewriting), so the bytes the gate verifies are
# exactly the bytes that ship; once README.md is committed and the tree is
# clean - which a real run already requires - that copy coincides with the
# archive's own copy.
#
# Usage:
#   scripts/publish.sh [--url GITHUB_URL] [--tag TAG] [-m|--message TEXT]
#                      [--dry-run]
#   scripts/publish.sh --help
#
# Options:
#   --url URL        Public GitHub remote (also settable via env GITHUB_URL),
#                    e.g. https://github.com/Clydesolve-Limited/mkvremux.git.
#                    Required for a real push; may be omitted with --dry-run
#                    (placeholders are substituted instead - the output says so).
#   --tag TAG        Annotated tag of this name on the snapshot commit, pushed
#                    with it. Internal tags stay on the internal remote;
#                    GitHub tags are mirrored onto snapshot commits here.
#   -m, --message T  Extra commit-message text appended to the snapshot message.
#   --dry-run        Do everything except push and tag.
#   -h, --help       This text.
#
# Environment: GITHUB_URL (same as --url), PUBLISH_DIR (override the publish
# clone location, default <repo>/../mkvremux-publish), PUBLISH_AUTHOR_NAME
# (snapshot commit author name, default "Clydesolve Limited"),
# PUBLISH_AUTHOR_EMAIL (snapshot commit author email, default
# "hello@clydesolve.com"). The author pair is written to the publish
# clone's OWN config only (git -C <clone> config user.name/user.email) -
# never global, never the source repo.
#
# Behaviour:
#   1. Refuses a dirty working tree (a snapshot publishes committed HEAD);
#      --dry-run warns and continues against HEAD.
#   2. git archive HEAD (tracked files only) into a fresh temp dir.
#   3. Deletes the 13 internal docs from the export.
#   4. Ships README.md byte-identically from the checkout (no transform;
#      no README-public.md exists in this project).
#   5. Runs the clean-room gate over the CURRENT export (excluded docs,
#      leak needles, README links, ELF binaries, README byte-identity);
#      any hit aborts before anything is committed or pushed.
#   6. Mirrors the export into ../mkvremux-publish (outside this repo) and
#      commits one snapshot, authored as PUBLISH_AUTHOR_NAME/
#      PUBLISH_AUTHOR_EMAIL via the clone's OWN config (never global, never
#      the source repo).
#   7. Before ANY push - dry-run included - runs history leak gate (h):
#      every commit in the publish clone (exactly what a push would ship)
#      is scanned for the assembled leak needles and for excluded-doc
#      filenames as paths. A hit aborts with
#      "clean-room gate FAILED (history)" before stage [7/7].
#   8. Pushes the snapshot (outside --dry-run), optionally tagging it
#      first and pushing the tag with it. The only remote ever pushed to
#      is "github" in the publish clone - the source repo's "origin" is
#      never read, written, or pushed to by this script.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Excluded internal docs (the 13) - never published.
# ---------------------------------------------------------------------------
EXCLUDED_DOCS=(
  README-INTERNAL.md
  WORK_LOG.md
  NOTES.md
  TODO.md
  notes.md
  REVIEW_LOG.md
  REVIEW_NOTES.md
  DEV_LOG.md
  DEV_NOTES.md
  RELEASING.md
  TEST_NOTES.md
  TEST_WORK_LOG.md
  .idea/
)

# Placeholder used for --dry-run without --url (reported as such).
PLACEHOLDER_URL="https://github.com/Clydesolve-Limited/mkvremux"

# Snapshot commit author identity (stakeholder decision, R2-12): the publish
# surface is public, so the machine default identity must never appear on
# GitHub. Env-overridable; applied as CLONE-LOCAL config only (see stage 5).
PUBLISH_AUTHOR_NAME="${PUBLISH_AUTHOR_NAME:-Clydesolve Limited}"
PUBLISH_AUTHOR_EMAIL="${PUBLISH_AUTHOR_EMAIL:-hello@clydesolve.com}"

usage() {
  sed -n '2,78p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
GITHUB_URL="${GITHUB_URL:-}"
TAG="${TAG:-}"
MESSAGE=""
DRY_RUN=0
URL_PROVIDED=0
if [ -n "$GITHUB_URL" ]; then URL_PROVIDED=1; fi

while [ $# -gt 0 ]; do
  case "$1" in
    --url)        [ $# -ge 2 ] || { echo "error: --url needs a value" >&2; exit 2; }
                  GITHUB_URL="$2"; URL_PROVIDED=1; shift 2 ;;
    --url=*)      GITHUB_URL="${1#--url=}"; URL_PROVIDED=1; shift ;;
    --tag)        [ $# -ge 2 ] || { echo "error: --tag needs a value" >&2; exit 2; }
                  TAG="$2"; shift 2 ;;
    --tag=*)      TAG="${1#--tag=}"; shift ;;
    -m|--message) [ $# -ge 2 ] || { echo "error: $1 needs a value" >&2; exit 2; }
                  MESSAGE="$2"; shift 2 ;;
    --message=*)  MESSAGE="${1#--message=}"; shift ;;
    --dry-run)    DRY_RUN=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            echo "error: unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# Repo + preconditions
# ---------------------------------------------------------------------------
command -v python3 >/dev/null 2>&1 || {
  echo "error: python3 is required for the clean-room gate's README link check but was not found on PATH" >&2; exit 1; }

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: not inside a git repository" >&2; exit 1; }
cd "$REPO_ROOT"

HEAD_SHA="$(git rev-parse HEAD)"
HEAD_SHA8="$(git rev-parse --short=8 HEAD)"

STATUS="$(git status --porcelain)"
if [ -n "$STATUS" ]; then
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "warning: working tree is dirty; --dry-run continues against committed HEAD ${HEAD_SHA8}" >&2
    printf '%s\n' "$STATUS" >&2
  else
    {
      echo "error: refusing to publish from a dirty working tree."
      echo "A snapshot publishes committed HEAD; commit (or stash) first, then re-run."
      echo "Dirty entries:"
      printf '%s\n' "$STATUS"
    } >&2
    exit 1
  fi
fi

# Placeholder URL substituted when none was given (reported as such).
if [ "$URL_PROVIDED" -eq 1 ]; then
  PLACEHOLDER=0
else
  GITHUB_URL="$PLACEHOLDER_URL"
  PLACEHOLDER=1
fi

EXPORT_DIR=""
on_exit() {
  local rc=$?
  if [ -n "$EXPORT_DIR" ]; then
    if [ "$rc" -ne 0 ]; then
      echo "note: export kept for inspection: $EXPORT_DIR" >&2
    else
      rm -rf "$EXPORT_DIR"
    fi
  fi
}
trap on_exit EXIT

PUBLISH_DIR="${PUBLISH_DIR:-$REPO_ROOT/../mkvremux-publish}"

# --- publish-dir safety (stage [5/7] wipes everything except .git there) ---
# The reference script trusts PUBLISH_DIR blindly; these three checks keep a
# mistyped override from wiping this repository or someone else's clone.
PUBLISH_NORM="$(realpath -m -- "$PUBLISH_DIR")"
REPO_NORM="$(realpath -m -- "$REPO_ROOT")"
if [ "$PUBLISH_NORM" = "$REPO_NORM" ]; then
  echo "error: PUBLISH_DIR must not be the source repository itself: $PUBLISH_DIR" >&2
  exit 1
fi
case "$PUBLISH_NORM/" in
  "$REPO_NORM/"*)
    echo "error: PUBLISH_DIR must not be inside the source repository: $PUBLISH_DIR" >&2
    exit 1 ;;
esac
case "$REPO_NORM/" in
  "$PUBLISH_NORM/"*)
    echo "error: PUBLISH_DIR must not contain the source repository: $PUBLISH_DIR" >&2
    exit 1 ;;
esac
if [ -d "$PUBLISH_DIR" ] && [ ! -d "$PUBLISH_DIR/.git" ] &&
   [ -n "$(ls -A -- "$PUBLISH_DIR" 2>/dev/null)" ]; then
  {
    echo "error: $PUBLISH_DIR exists, is not empty and is not a git repository."
    echo "Refusing to wipe it. Point PUBLISH_DIR at a dedicated publish clone."
  } >&2
  exit 1
fi
if [ -d "$PUBLISH_DIR/.git" ] && git -C "$PUBLISH_DIR" remote get-url origin >/dev/null 2>&1; then
  {
    echo "error: $PUBLISH_DIR has an 'origin' remote - it was not created by this script"
    echo "(publish clones only ever get a 'github' remote). Refusing to wipe it."
    echo "If it really is the publish clone: git -C $PUBLISH_DIR remote remove origin"
    echo "Or point PUBLISH_DIR at a different directory."
  } >&2
  exit 1
fi

echo "== mkvremux publish (sanitized snapshot) =="
echo "source repo : $REPO_ROOT"
echo "source HEAD : $HEAD_SHA8"
echo "mode        : $([ "$DRY_RUN" -eq 1 ] && echo 'dry-run (no push, no tag)' || echo 'push')"
echo "remote      : $GITHUB_URL $([ "$PLACEHOLDER" -eq 1 ] && echo '(PLACEHOLDER)')"
echo "publish dir : $PUBLISH_DIR"
echo "author      : $PUBLISH_AUTHOR_NAME <$PUBLISH_AUTHOR_EMAIL> (clone-local git config)"
echo

# ---------------------------------------------------------------------------
# [1/7] Export tracked files only
# ---------------------------------------------------------------------------
echo "[1/7] export: git archive HEAD"
EXPORT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/mkvremux-publish-XXXXXX")"
git archive HEAD | tar -x -C "$EXPORT_DIR"
echo "  -> $EXPORT_DIR"

# ---------------------------------------------------------------------------
# [2/7] Exclude the 13 internal docs
# ---------------------------------------------------------------------------
echo "[2/7] exclude internal docs"
REMOVED=()
ABSENT=()
for f in "${EXCLUDED_DOCS[@]}"; do
  if [ -e "$EXPORT_DIR/$f" ]; then
    # -rf (not -f): .idea/ is directory-shaped; the list is fixed literals.
    rm -rf -- "$EXPORT_DIR/$f"
    REMOVED+=("$f")
  else
    ABSENT+=("$f")
  fi
done
echo "  removed : ${REMOVED[*]:-<none>}"
if [ "${#ABSENT[@]}" -gt 0 ]; then echo "  absent  : ${ABSENT[*]}"; fi
echo "  excluded list (all ${#EXCLUDED_DOCS[@]}): ${EXCLUDED_DOCS[*]}"

# ---------------------------------------------------------------------------
# [3/7] Ship README.md byte-identically
#   README.md IS the public document in this project: no README-public.md
#   exists, so there is nothing to rename and nothing to rewrite - the
#   checkout's copy replaces the export's copy byte for byte. Sourced from
#   the checkout rather than the archive so --dry-run gates the document as
#   it currently stands (its first commit may not have landed yet) and so
#   the bytes that pass gate (g) are exactly the bytes that ship. A real
#   run has already refused a dirty tree here, so the copies then coincide
#   with HEAD's copy anyway.
# ---------------------------------------------------------------------------
echo "[3/7] ship README.md (byte-identical, no transform)"
[ -f "$REPO_ROOT/README.md" ] || {
  echo "error: README.md not found at $REPO_ROOT/README.md (the public document must exist)" >&2; exit 1; }
cp "$REPO_ROOT/README.md" "$EXPORT_DIR/README.md"
echo "  README.md <- checkout copy (byte for byte; README.md IS the public document)"

# ---------------------------------------------------------------------------
# [4/7] Clean-room gate (must pass or abort)
# ---------------------------------------------------------------------------
echo "[4/7] clean-room gate (leaks, links, ELF, README identity)"
GATE_FAIL=0

# The two needles are assembled at run time on purpose: this script is
# itself part of the publish set, so a literal needle here would make the
# gate flag its own source. Assembled, the script still scans EVERY
# published file (including this one) for the real strings.
NEEDLE_HOME="$(printf '%s%s' '/home/' 'martin')"
NEEDLE_LAN="$(printf '%s%s' '10.130.' '1.152')"

# (a) none of the 13 exists
for f in "${EXCLUDED_DOCS[@]}"; do
  if [ -e "$EXPORT_DIR/$f" ]; then
    echo "  (a) FAIL: excluded file still present: $f"; GATE_FAIL=1
  fi
done
if [ "$GATE_FAIL" -eq 0 ]; then echo "  (a) PASS: none of the ${#EXCLUDED_DOCS[@]} excluded docs is present"; fi

# (b) no internal home path (skip binaries)
HITS="$(grep -rI -n -F "$NEEDLE_HOME" "$EXPORT_DIR" 2>/dev/null || true)"
if [ -n "$HITS" ]; then
  echo "  (b) FAIL: leak - $NEEDLE_HOME found in the export:"; printf '%s\n' "$HITS" | sed "s|$EXPORT_DIR/|  |"
  GATE_FAIL=1
else
  echo "  (b) PASS: 0 hits of $NEEDLE_HOME (binaries skipped)"
fi

# (c) no private LAN address (documented only in the excluded internal
#     notes; proven absent from the publish set, assembled like (b))
HITS="$(grep -rI -n -F "$NEEDLE_LAN" "$EXPORT_DIR" 2>/dev/null || true)"
if [ -n "$HITS" ]; then
  echo "  (c) FAIL: leak - private LAN address found in the export:"; printf '%s\n' "$HITS" | sed "s|$EXPORT_DIR/|  |"
  GATE_FAIL=1
else
  echo "  (c) PASS: 0 hits of $NEEDLE_LAN (binaries skipped)"
fi

# (d) every relative README link target resolves inside the export, and no
#     link points at an excluded internal doc. Targets that resolve OUTSIDE
#     the export (leading "/" or "../") are counted and skipped: they cannot
#     address an excluded doc inside the snapshot (the README's legitimate
#     ../../releases/latest GitHub link is of this class).
LINK_OUT="$(python3 - "$EXPORT_DIR" <<'PYEOF'
import os
import re
import sys

export = sys.argv[1]
readme = os.path.join(export, "README.md")
EXCLUDED = {
    "README-INTERNAL.md", "WORK_LOG.md", "NOTES.md", "TODO.md", "notes.md",
    "REVIEW_LOG.md", "REVIEW_NOTES.md", "DEV_LOG.md", "DEV_NOTES.md",
    "RELEASING.md", "TEST_NOTES.md", "TEST_WORK_LOG.md", ".idea/",
}
EXCLUDED_NORM = {e.rstrip("/") for e in EXCLUDED}
with open(readme, encoding="utf-8") as fh:
    body = fh.read()
targets = [m.group(1) for m in re.finditer(r"\]\(([^()\s]+)\)", body)]
bad, excluded_left, relative, outside = [], [], 0, 0
for t in targets:
    if t.startswith("#") or re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*://", t):
        continue
    if t.startswith("/") or t.startswith("../"):
        outside += 1
        continue
    relative += 1
    base = t.split("#", 1)[0]
    if base.startswith("./"):
        base = base[2:]
    if base.rstrip("/") in EXCLUDED_NORM:
        excluded_left.append(t)
    elif not os.path.exists(os.path.join(export, base)):
        bad.append(t)
print(f"total={len(targets)} relative={relative} outside={outside}")
for t in excluded_left:
    print(f"EXCLUDED_LINK {t}")
for t in bad:
    print(f"MISSING {t}")
PYEOF
)"
echo "  (d) README links: $(echo "$LINK_OUT" | head -1)"
if echo "$LINK_OUT" | grep -q '^EXCLUDED_LINK'; then
  echo "  (d) FAIL: link(s) to an excluded doc remain:"; echo "$LINK_OUT" | grep '^EXCLUDED_LINK' | sed 's/^/    /'
  GATE_FAIL=1
fi
if echo "$LINK_OUT" | grep -q '^MISSING'; then
  echo "  (d) FAIL: README target(s) missing from the export:"; echo "$LINK_OUT" | grep '^MISSING' | sed 's/^/    /'
  GATE_FAIL=1
fi
if ! echo "$LINK_OUT" | grep -qE '^(EXCLUDED_LINK|MISSING)'; then
  echo "  (d) PASS: every relative README target resolves in the export; http(s)/#anchor/outside-tree links untouched"
fi

# (e) no ELF binary ships: the whole export is scanned (what actually
#     ships), plus the checkout's top level for any file git would still
#     track - a stray binary sitting in the tree before it can be
#     committed. Git-ignored build outputs (makeMKV, bin/) are skipped:
#     they can never be committed, so they can never ship.
ELF_HITS=()
while IFS= read -r f; do
  ELF_HITS+=("export:${f#"$EXPORT_DIR"/}")
done < <(grep -rl $'\x7fELF' "$EXPORT_DIR" 2>/dev/null || true)
while IFS= read -r -d '' f; do
  name="${f#./}"
  if git check-ignore -q -- "$name" 2>/dev/null; then continue; fi
  if head -c 4 -- "$f" | grep -q $'\x7fELF'; then
    ELF_HITS+=("checkout:$name")
  fi
done < <(find . -maxdepth 1 -type f -print0)
if [ "${#ELF_HITS[@]}" -gt 0 ]; then
  echo "  (e) FAIL: ELF check - binary detected, no binary ships:"
  printf '      %s\n' "${ELF_HITS[@]}"
  GATE_FAIL=1
else
  echo "  (e) PASS: ELF check - no binary in the export, none un-ignored at the checkout's top level"
fi

# (f) export size + top-level layout
FILE_COUNT="$(find "$EXPORT_DIR" -type f | wc -l | tr -d ' ')"
echo "  (f) export: $FILE_COUNT files; top-level layout:"
ls -1A "$EXPORT_DIR" | sed 's/^/      /'

# (g) the exported README is byte-identical to the checkout's README - a
#     transform creeping back into stage [3/7] would show up here
if cmp -s "$EXPORT_DIR/README.md" "$REPO_ROOT/README.md"; then
  echo "  (g) PASS: exported README.md is byte-identical to the checkout's README.md"
else
  echo "  (g) FAIL: exported README.md differs from the checkout's README.md:"
  cmp "$EXPORT_DIR/README.md" "$REPO_ROOT/README.md" 2>&1 | sed 's/^/    /' || true
  GATE_FAIL=1
fi

if [ "$GATE_FAIL" -ne 0 ]; then
  echo >&2
  echo "error: clean-room gate FAILED - nothing was committed or pushed." >&2
  exit 1
fi
echo "  gate: PASS"
echo

# ---------------------------------------------------------------------------
# [5/7] Publish clone (OUTSIDE the internal repo)
# ---------------------------------------------------------------------------
echo "[5/7] publish clone at $PUBLISH_DIR"
if [ ! -d "$PUBLISH_DIR/.git" ]; then
  git init -q "$PUBLISH_DIR"
  git -C "$PUBLISH_DIR" checkout -q -b main
  echo "  initialised fresh clone on branch main"
else
  echo "  reusing existing clone"
fi
# Snapshot author identity: CLONE-LOCAL config only. Never --global (would
# rewrite the operator's identity) and never the source repo's config (the
# internal repo keeps its own author/committer identity).
git -C "$PUBLISH_DIR" config user.name "$PUBLISH_AUTHOR_NAME"
git -C "$PUBLISH_DIR" config user.email "$PUBLISH_AUTHOR_EMAIL"
echo "  author  : $PUBLISH_AUTHOR_NAME <$PUBLISH_AUTHOR_EMAIL> (clone-local config)"
# Wipe working files, keep .git.
find "$PUBLISH_DIR" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
cp -a "$EXPORT_DIR"/. "$PUBLISH_DIR/"
echo "  working tree replaced with the export ($FILE_COUNT files)"

# ---------------------------------------------------------------------------
# [6/7] Snapshot commit
# ---------------------------------------------------------------------------
echo "[6/7] commit"
git -C "$PUBLISH_DIR" add -A
SNAP_MSG="snapshot: $(date +%F) (from $HEAD_SHA8)"
NEW_COMMIT=0
if ! git -C "$PUBLISH_DIR" rev-parse --verify -q HEAD >/dev/null; then
  NEW_COMMIT=1
elif ! git -C "$PUBLISH_DIR" diff --cached --quiet; then
  NEW_COMMIT=1
fi
if [ "$NEW_COMMIT" -eq 1 ]; then
  if [ -n "$MESSAGE" ]; then
    git -C "$PUBLISH_DIR" commit -q -m "$SNAP_MSG" -m "$MESSAGE"
  else
    git -C "$PUBLISH_DIR" commit -q -m "$SNAP_MSG"
  fi
  SNAP_SHA="$(git -C "$PUBLISH_DIR" rev-parse HEAD)"
  SNAP_SHA8="$(git -C "$PUBLISH_DIR" rev-parse --short=8 HEAD)"
  echo "  created: $SNAP_SHA8 ($SNAP_MSG)"
else
  SNAP_SHA="$(git -C "$PUBLISH_DIR" rev-parse HEAD)"
  SNAP_SHA8="$(git -C "$PUBLISH_DIR" rev-parse --short=8 HEAD)"
  echo "  no content change since the last snapshot; HEAD stays $SNAP_SHA8"
fi

# ---------------------------------------------------------------------------
# Pre-push history leak gate (h)
#
# Gates (a)-(g) scan the CURRENT export only, but the push ships the publish
# clone's WHOLE main history. A reused clone can therefore carry stale
# snapshots past the clean-room gate (proven 2026-11-20: 3 of 5 accumulated
# dry-run snapshots still held the scrubbed internal hostname). This gate
# scans EVERY commit that would be pushed - content needles assembled at
# runtime like gates (b)/(c), plus every excluded-doc filename seen as a
# path in any commit - and aborts BEFORE the push. Runs in dry-run and real
# mode alike. On a fresh clone (one snapshot commit) it is a single
# git grep over one tree; it earns its keep on a clone that accumulates.
# ---------------------------------------------------------------------------
echo "[6/7] pre-push history leak gate (h): scan every commit that would be pushed"
mapfile -t HIST_COMMITS < <(git -C "$PUBLISH_DIR" rev-list --all)
HIST_FAIL=0
if [ "${#HIST_COMMITS[@]}" -eq 0 ]; then
  echo "  (h) PASS: publish clone has no commits yet (nothing would be pushed)"
else
  # Content needles across every commit (assembled at runtime, like (b)/(c);
  # -I skips binaries). Output lines are <sha>:<path>:<line>.
  HIST_HITS="$(git -C "$PUBLISH_DIR" grep -I -n -F \
      -e "$NEEDLE_HOME" -e "$NEEDLE_LAN" "${HIST_COMMITS[@]}" 2>/dev/null || true)"
  if [ -n "$HIST_HITS" ]; then
    echo "  (h) FAIL: leak needle found in publish history (commit:file:line):"
    printf '%s\n' "$HIST_HITS" | head -20 | sed 's/^/    /'
    if [ "$(printf '%s\n' "$HIST_HITS" | wc -l)" -gt 20 ]; then echo "    ... (truncated)"; fi
    HIST_FAIL=1
  fi
  # Excluded-doc filenames as paths, per commit (names the commit).
  for c in "${HIST_COMMITS[@]}"; do
    while IFS= read -r p; do
      [ -n "$p" ] || continue
      for f in "${EXCLUDED_DOCS[@]}"; do
        base="${f%/}"
        if [ "$p" = "$base" ] || [ "${p#"$base"/}" != "$p" ]; then
          echo "  (h) FAIL: excluded doc '$f' appears as path '$p' in commit ${c:0:8}"
          HIST_FAIL=1
        fi
      done
    done < <(git -C "$PUBLISH_DIR" show --name-only --format= "$c")
  done
  if [ "$HIST_FAIL" -eq 0 ]; then
    echo "  (h) PASS: ${#HIST_COMMITS[@]} commit(s) scanned - no leak needles, no excluded-doc paths"
  fi
fi
if [ "$HIST_FAIL" -ne 0 ]; then
  echo >&2
  echo "error: clean-room gate FAILED (history) - nothing was pushed." >&2
  echo "The leak lives in $PUBLISH_DIR's accumulated history (a derived clone:" >&2
  echo "rebuildable from git archive HEAD). Delete the directory and re-run." >&2
  exit 1
fi
echo

# ---------------------------------------------------------------------------
# [7/7] Push (+ optional tag)
#
# Every `git push` in this script lives in the final else branch below:
# --dry-run returns above after printing what it WOULD do (no push runs),
# and a real run without a URL prints setup instructions instead. The only
# remote ever pushed to is named "github" and lives in the publish clone -
# the source repo and its "origin" are never pushed to or reconfigured.
# ---------------------------------------------------------------------------
echo "[7/7] push"
PUSHED=0
if [ "$DRY_RUN" -eq 1 ]; then
  echo "  dry-run: would push 'main' to remote 'github' ($GITHUB_URL)"
  if [ -n "$TAG" ]; then echo "  dry-run: would tag $SNAP_SHA8 with annotated tag '$TAG' and push it"; fi
elif [ "$URL_PROVIDED" -eq 0 ]; then
  cat <<EOF
  no GITHUB_URL given - nothing pushed. Set it up with:

    export GITHUB_URL=https://github.com/Clydesolve-Limited/mkvremux.git
    scripts/publish.sh

  (or pass --url https://github.com/Clydesolve-Limited/mkvremux.git), then re-run; the
  snapshot commit $SNAP_SHA8 is already waiting in $PUBLISH_DIR.
EOF
else
  if git -C "$PUBLISH_DIR" remote get-url github >/dev/null 2>&1; then
    EXISTING="$(git -C "$PUBLISH_DIR" remote get-url github)"
    if [ "$EXISTING" != "$GITHUB_URL" ]; then
      echo "error: remote 'github' points at $EXISTING, not $GITHUB_URL." >&2
      echo "Fix it deliberately: git -C $PUBLISH_DIR remote set-url github $GITHUB_URL" >&2
      exit 1
    fi
    git -C "$PUBLISH_DIR" push -q github main
  else
    git -C "$PUBLISH_DIR" remote add github "$GITHUB_URL"
    git -C "$PUBLISH_DIR" push -q -u github main
  fi
  PUSHED=1
  echo "  pushed: main ($SNAP_SHA8) -> $GITHUB_URL (remote 'github')"

  if [ -n "$TAG" ]; then
    if git -C "$PUBLISH_DIR" rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
      echo "error: tag '$TAG' already exists in $PUBLISH_DIR; pick another name or delete it deliberately." >&2
      exit 1
    fi
    git -C "$PUBLISH_DIR" tag -a "$TAG" -m "mkvremux $TAG - snapshot of internal $HEAD_SHA8"
    git -C "$PUBLISH_DIR" push -q github "refs/tags/$TAG"
    echo "  tagged : $TAG (annotated, on $SNAP_SHA8; source $HEAD_SHA8) + pushed"
  fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo
echo "== summary =="
echo "excluded   : ${EXCLUDED_DOCS[*]}"
echo "publish set: $FILE_COUNT files = README.md (public document) + Go sources/tests + go.mod/go.sum + LICENSE + Makefile + .golangci.yml + .gitignore (+ scripts/publish.sh once committed)"
echo "gate       : PASS (no excluded docs, no $NEEDLE_HOME, no $NEEDLE_LAN, README links resolve, README byte-identical, no ELF binary, history clean - gate (h) over ${#HIST_COMMITS[@]} commit(s))"
echo "snapshot   : $SNAP_SHA8 (source HEAD $HEAD_SHA8)"
if [ "$PUSHED" -eq 1 ]; then
  echo "pushed     : main -> $GITHUB_URL"
  if [ -n "$TAG" ]; then echo "             tag $TAG"; fi
elif [ "$DRY_RUN" -eq 1 ]; then
  echo "pushed     : nothing (dry-run)"
else
  echo "pushed     : nothing (no GITHUB_URL)"
fi

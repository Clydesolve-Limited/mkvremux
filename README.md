<p align="center">
  <img src=".github/logo.png" alt="mkvremux" width="256">
</p>

<h1 align="center">mkvremux</h1>

<p align="center">
  <b>An opinionated MKV remuxer to work beside NextPVR and an HDHomeRun.</b>
</p>

<!--
  Badges resolve against github.com/Clydesolve-Limited/mkvremux. The release and
  licence badges come from shields.io; the CI badge comes from this repository's
  own build.yml workflow. All three activate at first publish, once the public
  repository, the workflow file and the first v* tag exist.
-->
<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/Clydesolve-Limited/mkvremux" alt="MIT licence"></a>
  <a href="../../releases/latest"><img src="https://img.shields.io/github/v/release/Clydesolve-Limited/mkvremux" alt="latest release"></a>
  <a href="https://github.com/Clydesolve-Limited/mkvremux/actions/workflows/build.yml"><img src="https://github.com/Clydesolve-Limited/mkvremux/actions/workflows/build.yml/badge.svg" alt="CI status"></a>
</p>

## Introduction

`mkvremux` batch-remuxes a folder tree of recordings into MKV containers: it walks a source
directory, repackages every matching file into a `.mkv` under the destination, and organises the
results into season folders that a media library can consume directly. It is a command-line tool
for Linux, written in Go, created by <a href="https://www.clydesolve.com">ClydeSolve Limited</a>.

The design follows the DVR (digital video recorder) workflow it serves. An HDHomeRun tuner
captures broadcasts as transport-stream recordings, NextPVR is the PVR (personal video recorder)
that manages them, and mkvremux turns those captures into MKV containers and season folders for
Kodi/Plex-style players. The details follow from that workflow: `-fflags +genpts+igndts` repairs
the broken or truncated transport streams that DVR capture produces, `.ts` is a default extension
because that is what DVR capture writes, and the season folders mirror the organisation of a PVR
library.

## Install

### AppImage

Download `mkvremux-<version>-amd64.AppImage` from the [latest release](../../releases/latest),
make it executable and run it:

```sh
chmod +x mkvremux-<version>-amd64.AppImage
./mkvremux-<version>-amd64.AppImage --version
```

### Debian package

Download `mkvremux_<version>_amd64.deb` from the [latest release](../../releases/latest) and
install it:

```sh
sudo apt install ./mkvremux_<version>_amd64.deb
```

### From source

```sh
git clone https://github.com/Clydesolve-Limited/mkvremux.git && cd mkvremux && make build
```

`make build` writes the binary to `bin/makeMKV`. The packaged AppImage and `.deb` install that
same binary on your `PATH` as `mkvremux`, the name used throughout this README. `make help`
lists the remaining targets.

## Usage

Remux a tree of DVR recordings into a library directory:

```sh
mkvremux /media/recordings /media/library
```

The source tree is walked recursively. Every file matching the extension filter is probed with
`ffprobe` and written as `<name>.mkv` into an `sNN` season folder under the destination (a file
with no detectable season goes straight into the destination root). Files whose destination
already exists are skipped, so the command is safe to re-run and picks up only what is missing.

`mkvremux --version` prints the version. The complete `--help` output:

```text
Usage: makeMKV <source> <destination-directory> [flags]

Arguments:
  <source>                   Path to source video files
  <destination-directory>    Path to destination video files

Flags:
  -h, --help                 Show context-sensitive help.
      --series=STRING        Specify a custom series number
      --extensions=STRING    Comma-separated file extensions to process;
                             leading dot optional (e.g. mp4,ts or .mp4,.ts)
      --reencode             Use this to re-encode the video to HEVC
      --hwaccel="auto"       Hardware device policy for --reencode: auto
                             (default, fall back to copy) or vaapi (strict)
      --version              Print version information and quit
```

The usage line names the build binary, `makeMKV`; the packaged command is `mkvremux`.

### Why?

- **Remux by default, re-encode only when asked.** The default path is a stream copy (`-c copy`):
  the video and audio bitstreams move into the MKV container untouched, so the remux is fast and
  lossless and the broadcast quality survives. `--reencode` re-encodes video to HEVC (and audio to
  Opus) instead, which is much slower and introduces generational loss, but shrinks a library on a
  space-constrained server.
- **`--hwaccel` is a device policy, not a switch.** With `--reencode`, `--hwaccel=auto` (the
  default) probes the machine at startup; if no usable VAAPI (Video Acceleration API) device is
  found it warns loudly and stream-copies the whole run instead of failing. `--hwaccel=vaapi` is
  strict: no usable device stops the run with exit 2 before any file is touched. Choose strict
  mode in automation, where a silent downgrade is worse than a failed run.
- **Season folders mirror the PVR library.** Season detection reads the source path and filename
  for an `sNN` token (`Show.S01E03.ts`, or an `S01/` folder) and writes into the matching `s01`
  folder under the destination. When detection is wrong or the names carry no season token,
  `--series` forces one: `--series 3` writes into `s03`.
- **Every track survives.** Each file is probed with `ffprobe` and every audio and subtitle stream
  is mapped explicitly into the output, so a recording with several audio languages or subtitle
  tracks keeps all of them. Data streams (for example timecode tracks) and attached cover art are
  excluded from the mapping.

## Features

- Recursive source walk with an extension filter: `--extensions` accepts `mp4,ts` or `.mp4,.ts`
  (leading dot optional, any case), defaulting to `.mp4` and `.ts`.
- Dynamic stream mapping from `ffprobe`: all audio and subtitle tracks are preserved; data streams
  and attached cover art are excluded.
- Season detection from filenames and paths, with a `--series` override for names that carry no
  season token.
- VAAPI hardware-accelerated HEVC re-encoding (`--reencode`) with a startup probe and the
  `--hwaccel=auto|vaapi` device policy.
- Broken transport-stream repair with `-fflags +genpts+igndts`.
- Atomic output: `ffmpeg` writes a temporary file in the destination directory and it is renamed
  only on success, so a partial file never appears at the final path.
- Skips files whose destination `.mkv` already exists, so re-runs process only what is missing.
- Filenames sanitised to a filesystem-safe form (spaces become underscores, repeated separators
  collapsed, edges trimmed).
- Clean exit codes (see below) so scripts and cron jobs can act on the result.

## Requirements

- `ffmpeg` and `ffprobe` on `PATH`. Both are checked before any file is touched. On Debian and
  Ubuntu: `sudo apt install ffmpeg`.
- Optional: a VAAPI-capable GPU for `--reencode`, exposed as a render node under
  `/dev/dri/renderD*`. Without one, the default `--hwaccel=auto` policy falls back to stream copy
  with a warning; strict `--hwaccel=vaapi` fails with exit 2 instead.
- Go, only to build from source: `make build` compiles with the toolchain pinned in `go.mod`, and
  the default `GOTOOLCHAIN=auto` setting downloads it when the installed Go is older.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success: the run completed with no file failures. `--help` and `--version` also exit `0`. |
| `1` | The run completed but at least one file failed. Per-file errors are printed as they happen and repeated in the end-of-run summary. |
| `2` | Usage or preflight failure: missing `ffmpeg`/`ffprobe`, an invalid `--series` or `--hwaccel` value, an unreadable source root, or a strict `--hwaccel=vaapi` refusal. Nothing is written. |

A missing positional argument is reported by the argument parser itself and exits `80`, which is
outside this contract.

## Not MakeMKV

`mkvremux` is an independent open-source tool with a different purpose. It is **not** the
commercial MakeMKV product published at [makemkv.com](https://www.makemkv.com), and it has no
affiliation with or endorsement from that product's authors. The two names share only their
subject matter: both deal with MKV, the Matroska container, and that is where the resemblance
ends.

## Related projects

- [NextPVR](https://nextpvr.com): the PVR that mkvremux is designed to work beside.
- [HDHomeRun](https://www.silicondust.com/hdhomerun/): the network tuners whose transport-stream
  recordings are mkvremux's input.
- [Kodi](https://kodi.tv) and [Plex](https://www.plex.tv): the kind of player the resulting
  season-organised MKV library is written for.

This README is the project's complete documentation.

## License

`mkvremux` is released under the MIT licence. See [LICENSE](LICENSE) for the full text.

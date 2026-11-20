// hwaccel.go — --hwaccel flag surface, startup hardware preflight and the
// encode-plan matrix: a port of mkv-fixer's internal/fixer/hwaccel.go (the
// proven §7.4 / ADR-010 selection matrix), adapted to makeMKV per the
// hardware-discovery requirement and Amendment 1 / A2 in WORK_LOG.md.
//
// makeMKV differs from mkv-fixer in two ways that shape this file:
//
//   - NO software re-encode: makeMKV either re-encodes with hevc_vaapi or
//     stream-copies. --hwaccel therefore accepts only "auto" (default —
//     loud warning + copy when no device) and "vaapi" (strict — exit 2
//     before any file is touched); "software" is rejected with an explicit
//     note.
//
//   - The probe encode is 192×192 with the PRODUCTION flag set (measured:
//     hevc_vaapi rejects 128-wide sources — minimum width 130 — so
//     mkv-fixer's 128×128 h264_vaapi probe does not transfer here).
//     -seekable must NOT appear: lavfi inputs reject it. See probeArgs.
//
// Probe scope (Q-A2-1, approved): the startup probe catches MACHINE-class
// faults (no render node, driver down, encoder broken) before any file is
// touched; CONTENT-class failures (e.g. yuv444p sources failing hwupload
// with exit 218) can only be known per file and are caught in moveVideo
// from the captured ffmpeg stderr tail → recorded failure → exit 1.
//
// All discovery runs behind seams (renderNodesFn, probeEncodeFn,
// ffmpegEncodersTimeout, probeTimeout) so the suite stays GPU-free and
// ffmpeg-free, exactly as in the mkv-fixer original.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// hwaccelMode is the --hwaccel value. Only two modes exist — makeMKV has no
// software re-encode — so the zero value (hwaccelAuto) is the default.
type hwaccelMode int

const (
	hwaccelAuto  hwaccelMode = iota // default: probe, fall back to copy
	hwaccelVAAPI                    // strict: no usable device ⇒ exit 2
)

func (m hwaccelMode) String() string {
	switch m {
	case hwaccelVAAPI:
		return "vaapi"
	default:
		return "auto"
	}
}

// parseHwaccel validates a --hwaccel value: exact match, only "auto" and
// "vaapi". "software" gets its own note because it is a valid mkv-fixer
// mode that makeMKV deliberately does not have; anything else (including
// wrong case or a trailing space — the parser is exact-match) is a usage
// error, surfaced by main as message + usage + exit 2.
func parseHwaccel(s string) (hwaccelMode, error) {
	switch s {
	case "auto":
		return hwaccelAuto, nil
	case "vaapi":
		return hwaccelVAAPI, nil
	case "software":
		return 0, errors.New(`"software" is invalid: makeMKV has no software re-encode mode — use auto (fall back to stream copy) or vaapi (strict)`)
	default:
		return 0, errors.New("must be auto or vaapi")
	}
}

// errVaapiStrict is the exact approved strict-mode refusal text; it is the
// sentinel's own message so main's print is verbatim.
const errVaapiStrict = "--hwaccel=vaapi: no usable VAAPI device (probed /dev/dri/renderD*); --reencode refused. Use --hwaccel=auto to fall back to stream copy."

// errExit2 is the strict --hwaccel=vaapi refusal returned by
// selectEncodePlan: main prints it verbatim and exits 2 (checked with
// errors.Is). The message IS the sentinel text — not a wrapped generic
// error — so the exact approved wording reaches the user unchanged; the
// underlying cause (encoder scan vs no device) is logged separately by
// selectEncodePlan before this is returned. (Declared via the named
// constant above: ST1005 dislikes trailing periods in error literals, but
// the approved wording ends in one.)
//
//lint:ignore ST1005 approved A2 matrix wording ends with a period (WORK_LOG.md); the verbatim text is contractual
//nolint:staticcheck // golangci-lint honours nolint, not lint:ignore, for this same ST1005 exception
var errExit2 = errors.New(errVaapiStrict)

// Exact warning texts (approved A2 matrix; asserted verbatim).
const (
	// errAutoFallback is the auto-downgrade warning — never a failure:
	// the run continues with stream copy (Q-A2-2).
	errAutoFallback = "warning: no usable VAAPI device (probed /dev/dri/renderD*) — --reencode downgraded to stream copy; files will be remuxed but NOT re-encoded"
	// errHwaccelNoReencode is printed once when --hwaccel=vaapi is given
	// without --reencode: stream copy uses no hardware at all (Q-A2-5).
	errHwaccelNoReencode = "warning: --hwaccel=%s has no effect without --reencode (stream copy does not use hardware acceleration)"
)

// Timeout seams: production value 30s each; tests override them so timeout
// coverage stays fast and a wedged ffmpeg can never hang startup.
var (
	// ffmpegEncodersTimeout bounds the "ffmpeg -hide_banner -encoders"
	// capability scan.
	ffmpegEncodersTimeout = 30 * time.Second
	// probeTimeout bounds one probe encode (~100 ms measured; the bound
	// only guards a wedged ffmpeg).
	probeTimeout = 30 * time.Second
)

// encoderListed matches an encoder NAME token in "ffmpeg -encoders" output.
// The token must be an exact field (so "libx264rgb" never satisfies
// "libx264", "hevc_nvenc" never satisfies "hevc_vaapi").
func encoderListed(output, name string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			return true
		}
	}
	return false
}

// --- device discovery -------------------------------------------------------

// renderNodesFn is the device-discovery seam: the sorted /dev/dri/renderD*
// list (filepath.Glob is sorted by contract — the matrix iterates in sorted
// order). Tests return fake node lists and never touch /dev.
var renderNodesFn = defaultRenderNodes

// defaultRenderNodes globs the render nodes. This is the ONLY place in the
// codebase that names a render-node path pattern (the old hardcoded
// single-node literal was deleted in R1-7).
func defaultRenderNodes() []string {
	nodes, err := filepath.Glob("/dev/dri/renderD*")
	if err != nil {
		return nil
	}
	return nodes
}

// probeEncodeFn is the probe seam: nil error = the device encodes.
var probeEncodeFn = probeVAAPIEncode

// probeArgs builds the startup probe argv (one argv slice, no shell).
// The 192×192 source size is MANDATORY (measured: hevc_vaapi rejects
// 128-wide input — minimum width 130 — so a smaller probe would fail for
// the wrong reason; never shrink it). -seekable must NOT appear: lavfi
// inputs reject it. The -fflags/-err_detect/-hwaccel* flags are the
// production flags kept in the probe argv (A2-3/4/5) so the probe parses
// the same option set a real re-encode will.
func probeArgs(device string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-vaapi_device", device,
		"-fflags", "+genpts+igndts",
		"-hwaccel", "vaapi", "-hwaccel_device", device, "-hwaccel_output_format", "vaapi",
		"-err_detect", "ignore_err",
		"-f", "lavfi", "-i", "color=black:s=192x192:d=0.1",
		"-frames:v", "1",
		"-vf", "format=nv12,hwupload",
		"-c:v", "hevc_vaapi", "-qp", "23",
		"-pix_fmt", "yuv420p",
		"-max_muxing_queue_size", "9999",
		"-f", "null", "-",
	}
}

// probeVAAPIEncode runs one probe encode against one render node — node
// existence alone is insufficient (a node can exist with a broken driver).
// argv-only, bounded by probeTimeout, diagnostics via firstDiagLine.
func probeVAAPIEncode(ctx context.Context, device string) error {
	rctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, "ffmpeg", probeArgs(device)...) // ONE argv slice, no shell
	setChildPdeathsig(cmd)                                           // child dies with makeMKV (no orphaned probe)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if rctx.Err() != nil {
			return fmt.Errorf("probe timed out after %s", probeTimeout)
		}
		return fmt.Errorf("probe failed: %s", firstDiagLine("", string(out)))
	}
	return nil
}

// deviceCache memoizes discovery for the PROCESS lifetime: the first
// resolution probes, every later call reuses it. Negative results cache
// too — a node appearing mid-process is picked up on the next run
// (documented trade of the mkv-fixer matrix).
var deviceCache struct {
	mu       sync.Mutex
	resolved bool
	device   string // "" = no usable device found
}

// resetDeviceCache clears the process-lifetime cache (tests only).
//
//lint:ignore U1000 GPU-free test seam for R1-9's ported hwaccel_test.go; no production caller by design (process-lifetime cache)
func resetDeviceCache() {
	deviceCache.mu.Lock()
	defer deviceCache.mu.Unlock()
	deviceCache.resolved, deviceCache.device = false, ""
}

// resolveDevice returns the first render node that PASSES the probe encode,
// probing /dev/dri/renderD* in sorted order. Cached after the first call of
// the process (including a negative result).
func resolveDevice(ctx context.Context) (string, bool) {
	deviceCache.mu.Lock()
	defer deviceCache.mu.Unlock()
	if deviceCache.resolved {
		return deviceCache.device, deviceCache.device != ""
	}
	for _, node := range renderNodesFn() {
		if probeEncodeFn(ctx, node) == nil {
			deviceCache.resolved, deviceCache.device = true, node
			return node, true
		}
	}
	deviceCache.resolved, deviceCache.device = true, ""
	return "", false
}

// --- startup preflight and selection matrix ---------------------------------

// preflightHardware is the startup capability scan. With --reencode,
// "ffmpeg -hide_banner -encoders" must list field-exact "hevc_vaapi" — the
// only encoder makeMKV can re-encode with (there is no software re-encode
// mode). Without --reencode nothing executes (stream copy needs no
// encoder). ffmpeg's PRESENCE is already guaranteed by requireTools (R1-1);
// this checks capability, not presence, is bounded by ffmpegEncodersTimeout
// so a wedged ffmpeg cannot hang startup, and uses an argv slice (no shell).
func preflightHardware(mode hwaccelMode, reencode bool) error {
	if !reencode {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegEncodersTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders")
	setChildPdeathsig(cmd) // child dies with makeMKV (no orphaned scan)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("'ffmpeg -encoders' timed out after %s (hwaccel=%s)", ffmpegEncodersTimeout, mode)
		}
		return fmt.Errorf("'ffmpeg -encoders' failed: %w", err)
	}
	if !encoderListed(string(out), "hevc_vaapi") {
		return fmt.Errorf("ffmpeg does not list the 'hevc_vaapi' encoder (hwaccel=%s) — install an ffmpeg build with VAAPI support", mode)
	}
	return nil
}

// encoderPlan is the startup decision outcome for ONE run: which encoder
// path to use and any warnings that must render before processing.
type encoderPlan struct {
	Vaapi    bool     // encode with hevc_vaapi (false → stream-copy fallback)
	Device   string   // resolved /dev/dri/renderD* node when Vaapi
	Warnings []string // auto-fallback warning (rendered before processing)
}

// selectEncodePlan implements the approved A2 matrix. It runs BEFORE the
// walk, so the strict refusal exits 2 before any file is touched:
//
//	reencode=false → {Vaapi:false}: no scan, no probe, no warnings — the
//	                 hwaccel policy is inert (main prints its own warning
//	                 when the mode was explicitly vaapi).
//	reencode+auto  → scan + resolveDevice; any failure ⇒ {Vaapi:false,
//	                 Warnings:[errAutoFallback]} — never an error.
//	reencode+vaapi → scan or resolveDevice failure ⇒ errExit2 (exit 2).
//
// The underlying cause of a refusal is logged (log → stderr) before the
// fixed errExit2 text so the user still sees WHY (encoder missing vs no
// usable device) without weakening the pinned message.
func selectEncodePlan(ctx context.Context, mode hwaccelMode, reencode bool) (encoderPlan, error) {
	if !reencode {
		return encoderPlan{}, nil // stream copy: never scans, never probes
	}

	if err := preflightHardware(mode, reencode); err != nil {
		log.Printf("Hardware preflight: %v", err)
		if mode == hwaccelVAAPI {
			return encoderPlan{}, errExit2
		}
		return encoderPlan{Warnings: []string{errAutoFallback}}, nil
	}

	device, ok := resolveDevice(ctx)
	if ok {
		return encoderPlan{Vaapi: true, Device: device}, nil
	}
	if mode == hwaccelVAAPI {
		return encoderPlan{}, errExit2
	}
	return encoderPlan{Warnings: []string{errAutoFallback}}, nil
}

// --- diagnostics ------------------------------------------------------------

// firstDiagLine picks the most informative diagnostic line for a failure
// message: stderr first (design-faithful), then stdout, falling back to
// "no diagnostic output" so the caller's message is never empty.
func firstDiagLine(stderr, stdout string) string {
	for _, out := range []string{stderr, stdout} {
		if line := firstUsefulLine(out); line != "" {
			return line
		}
	}
	return "no diagnostic output"
}

// firstUsefulLine returns the first line of an output stream that carries a
// diagnostic. The needle is "Error" (mkv-fixer uses "Error:"): ffmpeg emits
// "Error while opening encoder…" with no colon. Lines named "Progress:" are
// skipped so a bare status update is never reported as the cause.
func firstUsefulLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "Error") {
			return strings.TrimSpace(l)
		}
	}
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "Progress:") {
			continue
		}
		return t
	}
	return ""
}

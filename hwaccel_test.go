// hwaccel_test.go — R1-9: GPU-free, ffmpeg-free tests for hwaccel.go,
// ported and adapted from mkv-fixer's internal/fixer/hwaccel_test.go (the
// proven §7.4 / ADR-010 matrix) per the approved A2 matrix in WORK_LOG.md.
//
// Hermeticity (Gate A): device discovery and the probe encode run entirely
// behind the renderNodesFn/probeEncodeFn seams — no test touches /dev/dri.
// preflightHardware has NO seam (DEV_NOTES R1-7), so every path that reaches
// the real ffmpeg binary installs an executable stub first via
// installFakeFFmpeg/installStubs (t.Setenv("PATH", ...)); with no ffmpeg on
// PATH those tests still pass because the stub shadows the real binary.
// No t.Parallel anywhere: the seams, the process-lifetime device cache,
// PATH and the timeout vars are all process-global.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// withSeams installs fake discovery/probe seams for one case and restores
// them (plus the process-lifetime device cache) afterwards. probeOK maps a
// node to "this probe passes"; any other node fails. Returns a pointer to
// the probe-call counter.
func withSeams(t *testing.T, nodes []string, probeOK map[string]bool) *int {
	t.Helper()
	prevNodes, prevProbe := renderNodesFn, probeEncodeFn
	calls := 0
	renderNodesFn = func() []string { return nodes }
	probeEncodeFn = func(_ context.Context, device string) error {
		calls++
		if probeOK[device] {
			return nil
		}
		return errors.New("probe failed: injected")
	}
	resetDeviceCache()
	t.Cleanup(func() {
		renderNodesFn, probeEncodeFn = prevNodes, prevProbe
		resetDeviceCache()
	})
	return &calls
}

// Encoder listings for the fake ffmpeg -encoders scan. Field-exact match
// matters: hevc_nvenc/libx264 must NEVER satisfy hevc_vaapi.
const (
	encodersWithVAAPI = `
Encoders:
 V..... = Video
 A..... = Audio
 V..... libx264              H.264 / AVC / MPEG-4 AVC (codec h264)
 V..... hevc_vaapi           HEVC (VAAPI) (codec hevc)
 V..... hevc_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 A..... libopus              Opus
`
	encodersNoVAAPI = `
Encoders:
 V..... libx264              H.264 / AVC / MPEG-4 AVC (codec h264)
 V..... hevc_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 A..... libopus              Opus
`
)

// TestSelectEncodePlanMatrix pins every row of the approved A2 matrix:
// copy mode never scans and never probes (either --hwaccel value); reencode
// + auto falls back to stream copy with the exact errAutoFallback warning
// (never an error) and picks the first passing node in sorted order;
// reencode + vaapi is strict — any failure returns errExit2 with the pinned
// refusal text, before any file is touched.
func TestSelectEncodePlanMatrix(t *testing.T) {
	const (
		d128 = "/dev/dri/renderD128"
		d129 = "/dev/dri/renderD129"
	)
	cases := []struct {
		name         string
		mode         hwaccelMode
		reencode     bool
		nodes        []string
		probeOK      map[string]bool
		encoders     string // fake ffmpeg -encoders output
		encodersExit int    // fake ffmpeg exit code
		wantVaapi    bool
		wantDevice   string
		wantWarnings []string
		wantErrExit2 bool
		wantProbes   int
	}{
		// reencode=false: inert policy — no scan, no probe, no warnings.
		{name: "copy-auto-never-scans-or-probes", mode: hwaccelAuto, wantProbes: 0},
		{name: "copy-vaapi-never-scans-or-probes", mode: hwaccelVAAPI, wantProbes: 0},

		// reencode + auto: probe-driven selection with copy fallback.
		{name: "reencode-auto-device-found", mode: hwaccelAuto, reencode: true,
			nodes: []string{d128}, probeOK: map[string]bool{d128: true},
			encoders: encodersWithVAAPI, wantVaapi: true, wantDevice: d128, wantProbes: 1},
		{name: "reencode-auto-no-nodes-fallback", mode: hwaccelAuto, reencode: true,
			encoders: encodersWithVAAPI, wantWarnings: []string{errAutoFallback}, wantProbes: 0},
		{name: "reencode-auto-all-probes-fail-fallback", mode: hwaccelAuto, reencode: true,
			nodes: []string{d128}, probeOK: map[string]bool{},
			encoders: encodersWithVAAPI, wantWarnings: []string{errAutoFallback}, wantProbes: 1},
		{name: "reencode-auto-first-passing-in-sorted-order", mode: hwaccelAuto, reencode: true,
			nodes: []string{d128, d129}, probeOK: map[string]bool{d129: true},
			encoders: encodersWithVAAPI, wantVaapi: true, wantDevice: d129, wantProbes: 2},
		{name: "reencode-auto-scan-missing-vaapi-encoder", mode: hwaccelAuto, reencode: true,
			nodes: []string{d128}, probeOK: map[string]bool{d128: true},
			encoders: encodersNoVAAPI, wantWarnings: []string{errAutoFallback}, wantProbes: 0},

		// reencode + vaapi: strict — errExit2, exit 2, no file ever touched.
		{name: "reencode-vaapi-device-found", mode: hwaccelVAAPI, reencode: true,
			nodes: []string{d128}, probeOK: map[string]bool{d128: true},
			encoders: encodersWithVAAPI, wantVaapi: true, wantDevice: d128, wantProbes: 1},
		{name: "reencode-vaapi-no-device-strict", mode: hwaccelVAAPI, reencode: true,
			encoders: encodersWithVAAPI, wantErrExit2: true, wantProbes: 0},
		{name: "reencode-vaapi-scan-fails-strict", mode: hwaccelVAAPI, reencode: true,
			encoders: encodersWithVAAPI, encodersExit: 1, wantErrExit2: true, wantProbes: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probes := withSeams(t, tc.nodes, tc.probeOK)
			_, marker := installFakeFFmpeg(t, tc.encoders, tc.encodersExit)

			plan, err := selectEncodePlan(context.Background(), tc.mode, tc.reencode)

			if tc.wantErrExit2 {
				if !errors.Is(err, errExit2) {
					t.Fatalf("err = %v, want errors.Is(err, errExit2)", err)
				}
				// The sentinel's message IS the pinned A2 refusal text.
				if err.Error() != errVaapiStrict {
					t.Errorf("err message =\n%q\nwant verbatim\n%q", err.Error(), errVaapiStrict)
				}
				if !strings.Contains(err.Error(), "--hwaccel=auto") {
					t.Errorf("err = %q, want the --hwaccel=auto remedy", err)
				}
				if !reflect.DeepEqual(plan, encoderPlan{}) {
					t.Errorf("plan = %+v, want the zero plan on refusal", plan)
				}
			} else {
				if err != nil {
					t.Fatalf("selectEncodePlan: %v", err)
				}
				if plan.Vaapi != tc.wantVaapi || plan.Device != tc.wantDevice {
					t.Errorf("plan = %+v, want vaapi=%v device=%q",
						plan, tc.wantVaapi, tc.wantDevice)
				}
				// Exact text: the auto fallback is errAutoFallback verbatim.
				if !reflect.DeepEqual(plan.Warnings, tc.wantWarnings) {
					t.Errorf("warnings = %q, want %q", plan.Warnings, tc.wantWarnings)
				}
			}
			if *probes != tc.wantProbes {
				t.Errorf("probe calls = %d, want %d", *probes, tc.wantProbes)
			}

			// The encoder scan runs only with --reencode: copy rows must not
			// have touched the fake ffmpeg at all; reencode rows must have
			// run exactly the "-encoders" scan.
			markerExists := true
			if _, statErr := os.Stat(marker); statErr != nil {
				if !os.IsNotExist(statErr) {
					t.Fatalf("stat marker: %v", statErr)
				}
				markerExists = false
			}
			if tc.reencode && !markerExists {
				t.Errorf("--reencode must run the ffmpeg -encoders scan, but the fake ffmpeg was never invoked")
			}
			if !tc.reencode && markerExists {
				b, readErr := os.ReadFile(marker)
				if readErr != nil {
					t.Fatalf("read marker: %v", readErr)
				}
				t.Errorf("copy mode must never scan encoders, but fake ffmpeg ran: %s", b)
			}
			if tc.reencode && markerExists {
				b, readErr := os.ReadFile(marker)
				if readErr != nil {
					t.Fatalf("read marker: %v", readErr)
				}
				if !strings.Contains(string(b), "-encoders") {
					t.Errorf("scan argv = %q, want -encoders", b)
				}
			}
		})
	}
}

// TestProbeArgsShape pins the approved probe argv EXACTLY: 192×192 source
// (hevc_vaapi rejects 128-wide input — minimum width 130 — so the probe
// must never shrink), production flags kept in the probe, -pix_fmt yuv420p,
// the device fed to both device flags, and NO -seekable (lavfi inputs
// reject it — deliberately different from the production reencode argv).
func TestProbeArgsShape(t *testing.T) {
	const dev = "/dev/dri/renderD128"
	want := []string{
		"-hide_banner", "-loglevel", "error",
		"-vaapi_device", dev,
		"-fflags", "+genpts+igndts",
		"-hwaccel", "vaapi", "-hwaccel_device", dev, "-hwaccel_output_format", "vaapi",
		"-err_detect", "ignore_err",
		"-f", "lavfi", "-i", "color=black:s=192x192:d=0.1",
		"-frames:v", "1",
		"-vf", "format=nv12,hwupload",
		"-c:v", "hevc_vaapi", "-qp", "23",
		"-pix_fmt", "yuv420p",
		"-max_muxing_queue_size", "9999",
		"-f", "null", "-",
	}
	got := probeArgs(dev)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("probeArgs =\n%q\nwant\n%q", got, want)
	}

	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "s=192x192") {
		t.Error("probe source must be 192x192 (hevc_vaapi rejects 128-wide input)")
	}
	if strings.Contains(joined, "128x128") {
		t.Error("a 128x128 probe must never be constructed")
	}
	if !strings.Contains(joined, "-pix_fmt yuv420p") {
		t.Error("probe must carry the production -pix_fmt yuv420p flag")
	}
	if strings.Contains(joined, "-seekable") {
		t.Error("-seekable must never appear in the probe argv (lavfi rejects it)")
	}
	if !strings.Contains(joined, "-vaapi_device "+dev) {
		t.Errorf("probe must pass the device to -vaapi_device, got %q", joined)
	}
}

// TestDeviceCache pins the process-lifetime cache: the first resolution
// probes, every later call reuses the result — both for a positive result
// and for a negative one (a node appearing mid-process is picked up on the
// next RUN, not the next call).
func TestDeviceCache(t *testing.T) {
	const d128 = "/dev/dri/renderD128"

	t.Run("positive-caching", func(t *testing.T) {
		probes := withSeams(t, []string{d128}, map[string]bool{d128: true})
		for i := 0; i < 3; i++ {
			dev, ok := resolveDevice(context.Background())
			if !ok || dev != d128 {
				t.Fatalf("call %d: device = %q, ok = %v; want %q/true", i, dev, ok, d128)
			}
		}
		if *probes != 1 {
			t.Errorf("probe calls = %d, want 1 (cached for the process lifetime)", *probes)
		}
	})

	t.Run("negative-caching", func(t *testing.T) {
		probes := withSeams(t, []string{d128}, map[string]bool{})
		for i := 0; i < 3; i++ {
			dev, ok := resolveDevice(context.Background())
			if ok || dev != "" {
				t.Fatalf("call %d: device = %q, ok = %v; want \"\"/false", i, dev, ok)
			}
		}
		if *probes != 1 {
			t.Errorf("probe calls = %d, want 1 (negative results cache too)", *probes)
		}
	})
}

// TestParseHwaccelTable pins parseHwaccel: "auto"/"vaapi" accepted;
// "software" rejected with makeMKV's explicit no-software-mode note (it is
// a valid mkv-fixer mode that makeMKV deliberately lacks); everything else
// rejected — the parser is exact-match, so wrong case and trailing spaces
// fail too.
func TestParseHwaccelTable(t *testing.T) {
	cases := []struct {
		in      string
		want    hwaccelMode
		wantErr string // "" = no error
	}{
		{"auto", hwaccelAuto, ""},
		{"vaapi", hwaccelVAAPI, ""},
		{"software", 0, `no software re-encode mode`},
		{"bogus", 0, "must be auto or vaapi"},
		{"", 0, "must be auto or vaapi"},
		{"Auto", 0, "must be auto or vaapi"}, // exact match only
		{"vaapi ", 0, "must be auto or vaapi"},
	}
	for _, tc := range cases {
		t.Run("in="+tc.in, func(t *testing.T) {
			got, err := parseHwaccel(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseHwaccel(%q) = %v, want an error", tc.in, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHwaccel(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseHwaccel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestHwaccelModeString pins all String() arms: the two real modes plus the
// default arm for unknown values (the zero value is hwaccelAuto, so an
// out-of-range value must never print as something else).
func TestHwaccelModeString(t *testing.T) {
	for _, tc := range []struct {
		mode hwaccelMode
		want string
	}{
		{hwaccelAuto, "auto"},
		{hwaccelVAAPI, "vaapi"},
		{hwaccelMode(99), "auto"}, // unknown value: default arm
	} {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("hwaccelMode(%d).String() = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// TestEncoderListed pins the field-exact encoder matcher: the NAME must be
// an exact token (field 2), so a longer name sharing the prefix
// (hevc_vaapi_xyz) never satisfies hevc_vaapi, and a name appearing only
// inside a description never satisfies anything.
func TestEncoderListed(t *testing.T) {
	listing := `
Encoders:
 V..... libx264              H.264 / AVC / MPEG-4 AVC (codec h264)
 V..... hevc_vaapi           HEVC (VAAPI) (codec hevc)
 A..... libopus              Opus
`
	for _, tc := range []struct {
		listing string
		name    string
		want    bool
	}{
		{listing, "hevc_vaapi", true},
		{listing, "libx264", true},
		{listing, "libopus", true},
		{listing, "libx264rgb", false}, // not listed at all
		{listing, "h264", false},       // appears only inside a description
		{listing, "hevc", false},       // ditto
		{listing, "", false},
		// The brief's exact requirement: a line naming hevc_vaapi_xyz must
		// NOT satisfy a search for hevc_vaapi.
		{" V..... hevc_vaapi_xyz       fake encoder\n", "hevc_vaapi", false},
		{" V..... hevc_vaapi_xyz       fake encoder\n", "hevc_vaapi_xyz", true},
	} {
		if got := encoderListed(tc.listing, tc.name); got != tc.want {
			t.Errorf("encoderListed(%q, %q) = %v, want %v", tc.listing, tc.name, got, tc.want)
		}
	}
}

// TestPreflightHardware drives the un-seamed capability scan (DEV_NOTES
// R1-7: no seam exists — PATH-stub is the way) through all outcomes:
// missing binary, a listing without hevc_vaapi, a healthy listing, a
// non-zero scan exit, and a wedged ffmpeg bounded by the timeout seam.
func TestPreflightHardware(t *testing.T) {
	t.Run("copy-mode-needs-no-encoder", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // no ffmpeg at all
		if err := preflightHardware(hwaccelAuto, false); err != nil {
			t.Errorf("stream copy must not scan (auto): %v", err)
		}
		if err := preflightHardware(hwaccelVAAPI, false); err != nil {
			t.Errorf("stream copy must not scan (vaapi): %v", err)
		}
	})

	t.Run("missing-binary", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // no ffmpeg on PATH
		err := preflightHardware(hwaccelAuto, true)
		if err == nil {
			t.Fatal("want an error when ffmpeg is missing")
		}
		if !strings.Contains(err.Error(), "'ffmpeg -encoders' failed") {
			t.Errorf("err = %v, want the scan-failure wrap", err)
		}
	})

	t.Run("vaapi-encoder-listed", func(t *testing.T) {
		_, marker := installFakeFFmpeg(t, encodersWithVAAPI, 0)
		if err := preflightHardware(hwaccelAuto, true); err != nil {
			t.Errorf("healthy scan (auto): %v, want nil", err)
		}
		if err := preflightHardware(hwaccelVAAPI, true); err != nil {
			t.Errorf("healthy scan (vaapi): %v, want nil", err)
		}
		if lines := markerLines(t, marker); len(lines) != 2 {
			t.Errorf("scan invocations = %d, want 2", len(lines))
		}
	})

	t.Run("vaapi-encoder-missing", func(t *testing.T) {
		installFakeFFmpeg(t, encodersNoVAAPI, 0)
		err := preflightHardware(hwaccelAuto, true)
		if err == nil {
			t.Fatal("want an error when hevc_vaapi is not listed")
		}
		if !strings.Contains(err.Error(), "does not list the 'hevc_vaapi' encoder") {
			t.Errorf("err = %v, want the missing-encoder diagnostic", err)
		}
		if !strings.Contains(err.Error(), "hwaccel=auto") {
			t.Errorf("err = %v, want the mode named in the diagnostic", err)
		}
	})

	t.Run("scan-exit-nonzero", func(t *testing.T) {
		installFakeFFmpeg(t, encodersWithVAAPI, 1)
		err := preflightHardware(hwaccelVAAPI, true)
		if err == nil {
			t.Fatal("want an error from a failing scan")
		}
		if !strings.Contains(err.Error(), "'ffmpeg -encoders' failed") {
			t.Errorf("err = %v, want the scan-failure wrap", err)
		}
	})

	t.Run("timeout-bounded", func(t *testing.T) {
		// A wedged ffmpeg: the stub sleeps past the seam. Builtins only.
		installStubs(t, map[string]string{"ffmpeg": "exec /bin/sleep 5\n"})
		prev := ffmpegEncodersTimeout
		ffmpegEncodersTimeout = 50 * time.Millisecond
		t.Cleanup(func() { ffmpegEncodersTimeout = prev })

		start := time.Now()
		err := preflightHardware(hwaccelAuto, true)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("want a timeout error")
		}
		if !strings.Contains(err.Error(), "timed out after") {
			t.Errorf("err = %v, want the timeout named", err)
		}
		if elapsed > 3*time.Second {
			t.Errorf("preflight not bounded by the seam: took %s", elapsed)
		}
	})
}

// TestProbeVAAPIEncodeFakeFFmpeg drives the real probe exec against stub
// ffmpeg binaries: success, a failure carrying ffmpeg's diagnostic (via
// firstDiagLine), and the probeTimeout seam bounding a wedged probe.
func TestProbeVAAPIEncodeFakeFFmpeg(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		installStubs(t, map[string]string{"ffmpeg": "exit 0\n"})
		if err := probeVAAPIEncode(context.Background(), "/dev/dri/renderD128"); err != nil {
			t.Errorf("probe: %v, want nil", err)
		}
	})
	t.Run("failure-diagnostic", func(t *testing.T) {
		installStubs(t, map[string]string{"ffmpeg": "printf '%s\\n' 'Error while opening encoder for output stream #0:0' >&2\nexit 1\n"})
		err := probeVAAPIEncode(context.Background(), "/dev/dri/renderD128")
		if err == nil {
			t.Fatal("want an error from a failing probe")
		}
		if !strings.Contains(err.Error(), "Error while opening encoder") {
			t.Errorf("err = %v, want ffmpeg's diagnostic surfaced", err)
		}
	})
	t.Run("timeout-bounded", func(t *testing.T) {
		installStubs(t, map[string]string{"ffmpeg": "exec /bin/sleep 5\n"})
		prev := probeTimeout
		probeTimeout = 50 * time.Millisecond
		t.Cleanup(func() { probeTimeout = prev })

		start := time.Now()
		err := probeVAAPIEncode(context.Background(), "/dev/dri/renderD128")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("want a timeout error")
		}
		if !strings.Contains(err.Error(), "timed out after") {
			t.Errorf("err = %v, want the timeout named", err)
		}
		if elapsed > 3*time.Second {
			t.Errorf("probe not bounded by the seam: took %s", elapsed)
		}
	})
}

// TestExactHwaccelTexts asserts the contractual A2 strings VERBATIM: the
// auto-fallback warning, the strict refusal (errExit2 IS its own message so
// main prints it unchanged), the vaapi-without-reencode warning format, and
// the dual lint directives on errExit2's declaration — all pinned as
// intentional, not drift (DEV_NOTES R1-7).
func TestExactHwaccelTexts(t *testing.T) {
	const wantAutoFallback = "warning: no usable VAAPI device (probed /dev/dri/renderD*) — --reencode downgraded to stream copy; files will be remuxed but NOT re-encoded"
	if errAutoFallback != wantAutoFallback {
		t.Errorf("errAutoFallback =\n%q\nwant\n%q", errAutoFallback, wantAutoFallback)
	}

	const wantStrict = `--hwaccel=vaapi: no usable VAAPI device (probed /dev/dri/renderD*); --reencode refused. Use --hwaccel=auto to fall back to stream copy.`
	if errVaapiStrict != wantStrict {
		t.Errorf("errVaapiStrict =\n%q\nwant\n%q", errVaapiStrict, wantStrict)
	}
	if errExit2.Error() != errVaapiStrict {
		t.Errorf("errExit2.Error() =\n%q\nwant it to BE the pinned refusal verbatim", errExit2.Error())
	}
	if !strings.Contains(errExit2.Error(), "--hwaccel=auto") {
		t.Error("errExit2 must carry the --hwaccel=auto remedy")
	}

	const wantNoReencode = "warning: --hwaccel=%s has no effect without --reencode (stream copy does not use hardware acceleration)"
	if errHwaccelNoReencode != wantNoReencode {
		t.Errorf("errHwaccelNoReencode =\n%q\nwant\n%q", errHwaccelNoReencode, wantNoReencode)
	}
	rendered := fmt.Sprintf(errHwaccelNoReencode, hwaccelVAAPI)
	if want := "warning: --hwaccel=vaapi has no effect without --reencode (stream copy does not use hardware acceleration)"; rendered != want {
		t.Errorf("rendered = %q, want %q", rendered, want)
	}

	// The message ends in a period (approved wording) while ST1005 dislikes
	// that; BOTH lint directives on the declaration are deliberate. Pin
	// them so a future "cleanup" cannot silently reintroduce a lint finding
	// or weaken the verbatim contract.
	src, err := os.ReadFile("hwaccel.go")
	if err != nil {
		t.Fatalf("read hwaccel.go: %v", err)
	}
	for _, needle := range []string{"//lint:ignore ST1005", "//nolint:staticcheck"} {
		if !strings.Contains(string(src), needle) {
			t.Errorf("hwaccel.go missing the intentional lint directive %q (DEV_NOTES R1-7)", needle)
		}
	}
}

// TestFirstDiagLine pins the diagnostics picker: stderr first
// (design-faithful), a line carrying "Error" preferred within a stream,
// "Progress:" status lines skipped, and a non-empty fallback so the
// caller's message is never blank.
func TestFirstDiagLine(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		stdout string
		want   string
	}{
		{"no-output", "", "", "no diagnostic output"},
		{"stderr-error-line", "noise\nError while opening encoder\nmore", "", "Error while opening encoder"},
		{"stderr-preferred", "plain stderr line", "Error from stdout", "plain stderr line"},
		{"stdout-fallback", "", "some output", "some output"},
		{"stdout-error-line", "", "Error: bad option", "Error: bad option"},
		{"progress-skipped", "", "Progress: 50%\nreal cause", "real cause"},
		// A stream with NOTHING but progress/status lines is never reported
		// as the cause: the caller falls back to its fixed text.
		{"progress-only", "\nProgress: 10%\n", "", "no diagnostic output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstDiagLine(tc.stderr, tc.stdout); got != tc.want {
				t.Errorf("firstDiagLine(%q, %q) = %q, want %q",
					tc.stderr, tc.stdout, got, tc.want)
			}
		})
	}
}

// TestDefaultRenderNodes exercises the only real discovery entry point: the
// fixed glob must never error, and whatever it returns (empty on machines
// without a GPU) must be sorted render-node paths — the matrix iterates in
// sorted order by contract.
func TestDefaultRenderNodes(t *testing.T) {
	nodes := defaultRenderNodes()
	if !sort.StringsAreSorted(nodes) {
		t.Errorf("nodes = %v, want sorted order", nodes)
	}
	for _, n := range nodes {
		if !strings.HasPrefix(n, "/dev/dri/renderD") {
			t.Errorf("node %q is not a render node", n)
		}
	}
}

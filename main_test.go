// main_test.go — R1-9: hermetic unit and regression tests for main.go.
//
// Gate A: the suite must pass with NO GPU and NO ffmpeg/ffprobe on PATH.
// Every test that would reach an external tool either (a) runs behind an
// exec-free seam (prepareFile/getVideoInfo/destinationFor are pure), or
// (b) installs executable stubs via installStubs (t.Setenv("PATH", ...)),
// or (c) — exactly ONE test, TestH1.../TestAtomicRemuxFailure... below —
// skips when the real ffmpeg is absent. No network, no t.Parallel (PATH,
// the log writer and the hwaccel seams are process-global).
//
// Regression coverage: H1 (nil-deref on bad filename), H2 (destination
// escaping destRoot), H3 (data/attached-pic streams mapped), H4 (-map
// flags dropped from the copy branch).
package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// --- fake external tools (shared with hwaccel_test.go) ----------------------

// installStubs writes the named executable shell stubs into one fresh temp
// dir, points PATH at it (shadowing any real binaries of the same names) and
// returns the shared invocation marker: every stub appends its argv to the
// marker file FIRST, so "marker absent" proves the tool was never executed.
// Bodies must use shell builtins only — the PATH deliberately contains
// nothing else (mkv-fixer's makeFakeFFmpeg precedent).
func installStubs(t *testing.T, stubs map[string]string) (binDir, marker string) {
	t.Helper()
	binDir = t.TempDir()
	marker = filepath.Join(binDir, "invocations.log")
	for name, body := range stubs {
		script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> '%s'\n%s\n", marker, body)
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write stub %q: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir)
	return binDir, marker
}

// installFakeFFmpeg installs an ffmpeg stub whose stdout is the given
// "-encoders" listing, exiting with the given code, and returns the marker.
func installFakeFFmpeg(t *testing.T, encodersOutput string, exitCode int) (binDir, marker string) {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(encodersOutput, "\n") {
		fmt.Fprintf(&b, "printf '%%s\\n' '%s'\n", strings.ReplaceAll(line, "'", `'\''`))
	}
	fmt.Fprintf(&b, "exit %d\n", exitCode)
	return installStubs(t, map[string]string{"ffmpeg": b.String()})
}

// installFakeFFprobe installs an ffprobe stub that prints stdoutJSON on
// stdout (when non-empty) and stderrText on stderr (when non-empty), then
// exits with exitCode.
func installFakeFFprobe(t *testing.T, stdoutJSON, stderrText string, exitCode int) (binDir, marker string) {
	t.Helper()
	var b strings.Builder
	if stdoutJSON != "" {
		fmt.Fprintf(&b, "printf '%%s' '%s'\n", strings.ReplaceAll(stdoutJSON, "'", `'\''`))
	}
	if stderrText != "" {
		fmt.Fprintf(&b, "printf '%%s\\n' '%s' >&2\n", strings.ReplaceAll(stderrText, "'", `'\''`))
	}
	fmt.Fprintf(&b, "exit %d\n", exitCode)
	return installStubs(t, map[string]string{"ffprobe": b.String()})
}

// markerLines returns the non-empty invocation lines recorded by installStubs.
func markerLines(t *testing.T, marker string) []string {
	t.Helper()
	b, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// argvHasPair reports whether argv contains flag immediately followed by value.
func argvHasPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// --- pure helpers -----------------------------------------------------------

// TestNormaliseExtensions pins the M4 fix: user specs are canonicalised to
// dotted-lowercase keys matching filepath.Ext (which always returns a dotted
// extension), stray dots/spaces/empty entries are tolerated, and a spec with
// nothing usable falls back to the default {.mp4, .ts} set — never to an
// empty set (which would silently process nothing).
func TestNormaliseExtensions(t *testing.T) {
	defaults := map[string]bool{".mp4": true, ".ts": true}
	cases := []struct {
		spec string
		want map[string]bool
	}{
		{"mp4,ts", map[string]bool{".mp4": true, ".ts": true}},
		{".MP4 , .Ts", map[string]bool{".mp4": true, ".ts": true}},
		{"..mp4", map[string]bool{".mp4": true}}, // TrimLeft cutset strips EVERY leading dot
		{"mp4,,ts", map[string]bool{".mp4": true, ".ts": true}},
		{"", defaults},
		{",,", defaults},
		{"  ,  ", defaults},
		{"flv", map[string]bool{".flv": true}},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			got := normaliseExtensions(tc.spec)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normaliseExtensions(%q) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

// TestH2DestinationFor pins the H2 regression: the destination directory is
// built with filepath.Join, so the result can never escape destRoot (no
// sibling "dests01" directories, no traversal), and --series is validated
// against ^[Ss]?[0-9]{1,2}$ and normalised to zero-padded "sNN".
func TestH2DestinationFor(t *testing.T) {
	cases := []struct {
		name           string
		destRoot       string
		detectedSeason string
		customSeason   string
		want           string
		wantErr        bool
	}{
		{"no-trailing-slash-root", "/out", "s01", "", "/out/s01", false},
		{"empty-both-return-root", "/out", "", "", "/out", false},
		{"detected-season-joins", "/out/", "s02", "", "/out/s02", false},
		{"series-single-digit-pads", "/out", "", "3", "/out/s03", false},
		{"series-upper-s-normalises", "/out", "", "S1", "/out/s01", false},
		{"series-lower-s-two-digits", "/out", "", "s12", "/out/s12", false},
		{"series-upper-s-two-digits", "/out", "", "S12", "/out/s12", false},
		{"reject-path-traversal", "/out", "", "../../etc", "", true},
		{"reject-absolute-path", "/out", "", "/etc", "", true},
		{"reject-alpha", "/out", "", "abc", "", true},
		{"reject-three-digits", "/out", "", "s012", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := destinationFor(tc.destRoot, tc.detectedSeason, tc.customSeason)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("destinationFor(%q, %q, %q) = %q, want an error",
						tc.destRoot, tc.detectedSeason, tc.customSeason, got)
				}
				if !strings.Contains(err.Error(), "invalid season") {
					t.Errorf("err = %v, want the \"invalid season\" diagnostic", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("destinationFor(%q, %q, %q): %v",
					tc.destRoot, tc.detectedSeason, tc.customSeason, err)
			}
			if got != tc.want {
				t.Errorf("destinationFor(...) = %q, want %q", got, tc.want)
			}
			// H2 invariant: the result always stays inside destRoot.
			rel, err := filepath.Rel(tc.destRoot, got)
			if err != nil {
				t.Fatalf("filepath.Rel(%q, %q): %v", tc.destRoot, got, err)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("result %q escapes destRoot %q (rel=%q) — H2 regression",
					got, tc.destRoot, rel)
			}
		})
	}
}

// TestGetSeason pins the R1-6 season-token contract by BEHAVIOUR (the
// pattern itself is an implementation detail): "sNN" only at a word start,
// exactly two digits, first match wins, lowercased; embedded false positives
// (Eps01, vs12) and over-long tokens (s123) yield "".
func TestGetSeason(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"S01E02.mkv", "s01"},
		{"show s02e05", "s02"},
		{"/lib/show/S03/ep01.ts", "s03"},
		{"Eps01-special", ""}, // leading \b rejects the embedded "s" of "Eps"
		{"vs12", ""},          // leading \b rejects the embedded "s" of "vs"
		{"s123", ""},          // trailing check rejects a third digit
		{"movie.mp4", ""},
		{"show s1e02.mp4", ""}, // single-digit tokens are not detected (use --series)
		{"s02 s01.mkv", "s02"}, // first match wins, lowercased
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := getSeason(tc.in); got != tc.want {
				t.Errorf("getSeason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeFilename pins existing behaviour: Unicode letters/numbers are
// kept, spaces become underscores, emoji/control/symbols are dropped, runs
// of '_'/'-' collapse to one separator, and edges are trimmed of "_-.".
func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"My Show S01", "My_Show_S01"},
		{"  padded  ", "padded"},           // edge trim of collapsed separator runs
		{"Café 日本語", "Café_日本語"},           // Unicode letters kept
		{"Show2024", "Show2024"},           // numbers kept
		{"emoji😀here", "emojihere"},        // emoji dropped
		{"ctrl\x00chars", "ctrlchars"},     // control chars dropped
		{"tab\there", "tab_here"},          // tab is a space
		{"hello___world", "hello_world"},   // runs collapse
		{"a - b", "a_b"},                   // mixed '_'/'-' run collapses to one
		{"..hidden..", "hidden"},           // leading/trailing dots trimmed
		{"Show.Name.S01", "Show.Name.S01"}, // interior dots preserved
		{"100% legit", "100_legit"},        // symbols dropped, spaces collapsed
		{"!!!", ""},                        // all-symbol name -> empty
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeFilename(tc.in); got != tc.want {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestGetFileNameWithoutExtension pins the extension-stripping contract that
// feeds FileMKV: exactly one interior dot is required, so leading-dot
// (".mp4"), trailing-dot ("a.") and extension-less ("noext") names are
// rejected with an error instead of producing a bogus output name.
func TestGetFileNameWithoutExtension(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{".mp4", "", true}, // leading dot: lastDotIndex == 0
		{"a.", "", true},   // trailing dot: lastDotIndex == len-1
		{"noext", "", true},
		{"show.s01.mkv", "show.s01", false},
		{"/tmp/show.s01.mkv", "show.s01", false}, // operates on filepath.Base
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := getFileNameWithoutExtension(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("getFileNameWithoutExtension(%q) = %q, want error", tc.in, got)
				}
				if !strings.Contains(err.Error(), "invalid file name") {
					t.Errorf("err = %v, want the \"invalid file name\" diagnostic", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("getFileNameWithoutExtension(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("getFileNameWithoutExtension(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestH3SummariseStreams pins the H3 regression with an R1-3-faithful
// fixture: codec_type "data" (tmcd timecode, which real ffprobe reports with
// an EMPTY codec_name — the filter must key on CodecType, never codec name)
// and attached_pic cover art of ANY codec (mjpeg AND png) are excluded; all
// mappable streams become alternating "-map", "0:N" pairs.
func TestH3SummariseStreams(t *testing.T) {
	fixture := &ProbeResult{Streams: []Stream{
		{Index: 0, CodecName: "h264", CodecType: "video"},
		{Index: 1, CodecName: "mjpeg", CodecType: "video",
			Disposition: StreamDisposition{AttachedPic: 1}},
		{Index: 2, CodecName: "", CodecType: "data"}, // tmcd: real ffprobe emits an empty codec_name
		{Index: 3, CodecName: "aac", CodecType: "audio"},
		{Index: 4, CodecName: "aac", CodecType: "audio"},
		{Index: 5, CodecName: "mov_text", CodecType: "subtitle"},
		{Index: 6, CodecName: "png", CodecType: "video",
			Disposition: StreamDisposition{AttachedPic: 1}},
	}}
	want := []string{"-map", "0:0", "-map", "0:3", "-map", "0:4", "-map", "0:5"}
	got := summariseStreams(fixture)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summariseStreams = %q, want %q (no data, no cover art)", got, want)
	}

	if empty := summariseStreams(&ProbeResult{}); len(empty) != 0 {
		t.Errorf("no streams = %q, want an empty result", empty)
	}
}

// TestH4BuildFFmpegArgs pins the H4 regression (the copy branch must USE
// the generated -map flags) and the reencode argv contract from R1-3/R1-7:
// -err_detect ignore_err is a decoder-side input option and must precede
// -i, the device comes from the parameter (the old renderD128 literal is
// gone), and both variants end -y <output>.
func TestH4BuildFFmpegArgs(t *testing.T) {
	maps := []string{"-map", "0:0", "-map", "0:3"}

	t.Run("copy-mode", func(t *testing.T) {
		got := buildFFmpegArgs(false, "", "in.ts", "out.mkv", maps)
		want := []string{
			"-fflags", "+genpts+igndts",
			"-i", "in.ts",
			"-map", "0:0", "-map", "0:3",
			"-c", "copy",
			"-max_muxing_queue_size", "9999",
			"-y", "out.mkv",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("copy argv =\n%q\nwant\n%q", got, want)
		}
		// H4: every -map must land strictly BEFORE "-c copy" or the flags
		// are silently unused (the original defect).
		copyIdx := slices.Index(got, "copy")
		for i, a := range got {
			if a == "-map" && i > copyIdx {
				t.Errorf("-map at index %d is after -c copy at %d — H4 regression", i, copyIdx)
			}
		}
		// Shared invariants: genpts/igndts repair flags and -y at the tail.
		if !argvHasPair(got, "-fflags", "+genpts+igndts") {
			t.Error("copy argv missing -fflags +genpts+igndts")
		}
		if !slices.Contains(got, "-y") || got[len(got)-2] != "-y" || got[len(got)-1] != "out.mkv" {
			t.Errorf("copy argv must end -y out.mkv, got tail %q", got[len(got)-2:])
		}
	})

	t.Run("reencode-mode", func(t *testing.T) {
		const device = "/dev/dri/renderD129"
		got := buildFFmpegArgs(true, device, "in.ts", "out.mkv", maps)
		// -err_detect is a decoder-side input option: before -i it works,
		// after -i it does not (R1-3 review finding).
		errIdx, inIdx := slices.Index(got, "-err_detect"), slices.Index(got, "-i")
		if errIdx == -1 || inIdx == -1 || errIdx >= inIdx {
			t.Errorf("-err_detect (idx %d) must precede -i (idx %d)", errIdx, inIdx)
		}
		// The device is the PASSED one — never a hardcoded render node.
		if !argvHasPair(got, "-hwaccel_device", device) {
			t.Errorf("-hwaccel_device must be %q, got %q", device, got)
		}
		if slices.Contains(got, "renderD128") {
			t.Error("argv contains the deleted renderD128 literal — device must come from the parameter")
		}
		if !argvHasPair(got, "-c:v", "hevc_vaapi") {
			t.Error("reencode argv missing -c:v hevc_vaapi")
		}
		if !argvHasPair(got, "-pix_fmt", "yuv420p") {
			t.Error("reencode argv missing -pix_fmt yuv420p")
		}
		// Production reencode argv keeps -seekable 0 (the PROBE argv must
		// not — lavfi rejects it; pinned in hwaccel_test.go).
		if !argvHasPair(got, "-seekable", "0") {
			t.Error("reencode argv missing -seekable 0 (production flag set)")
		}
		if !argvHasPair(got, "-fflags", "+genpts+igndts") {
			t.Error("reencode argv missing -fflags +genpts+igndts")
		}
		// Maps are inserted after -i, and the argv ends -y <output>.
		if i := slices.Index(got, "-map"); i < inIdx {
			t.Errorf("-map (idx %d) must follow -i (idx %d)", i, inIdx)
		}
		if got[len(got)-2] != "-y" || got[len(got)-1] != "out.mkv" {
			t.Errorf("reencode argv must end -y out.mkv, got tail %q", got[len(got)-2:])
		}
	})
}

// --- handler / report regressions ------------------------------------------

// TestH1HandlerBadFilenameDoesNotPanic pins the H1 nil-deref regression: a
// file literally named ".mp4" fails filename parsing in prepareFile; the
// handler must record that failure, CONTINUE the batch (the valid file still
// succeeds) and return nil so the walk does not abort — no panic, no ffmpeg
// or ffprobe on PATH (PATH is stripped, and the valid file's destination is
// pre-created so moveVideo returns at its skip-if-exists check before any
// probe can run).
func TestH1HandlerBadFilenameDoesNotPanic(t *testing.T) {
	// No tools on PATH: if anything tried to exec, it would fail and the
	// failure-count assertions below would catch it.
	t.Setenv("PATH", t.TempDir())

	srcDir := t.TempDir()
	badPath := filepath.Join(srcDir, ".mp4")
	if err := os.WriteFile(badPath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(srcDir, "show s01e01.mp4")
	if err := os.WriteFile(validPath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-create the valid file's destination so moveVideo takes its
	// skip-if-exists branch (exec-free) instead of probing.
	destRoot := t.TempDir()
	destDir, err := destinationFor(destRoot, getSeason(validPath), "")
	if err != nil {
		t.Fatalf("destinationFor: %v", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(destDir, "show_s01e01.mkv")
	if err := os.WriteFile(destPath, []byte("already remuxed"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		sourceRoot:  srcDir,
		destination: destRoot,
		extensions:  normaliseExtensions(""),
	}
	rep := &runReport{}

	// Must not panic (H1 was a nil deref) and must not abort the walk.
	if walkErr := filepath.Walk(srcDir, newHandler(cfg, rep)); walkErr != nil {
		t.Fatalf("walk returned %v, want nil (per-file failures never abort the batch)", walkErr)
	}

	if rep.attempted != 2 {
		t.Errorf("attempted = %d, want 2 (both matching files handled)", rep.attempted)
	}
	if len(rep.failures) != 1 {
		t.Fatalf("failures = %d, want exactly 1 (the bad filename); failures: %v",
			len(rep.failures), rep.failures)
	}
	if rep.failures[0].path != badPath {
		t.Errorf("failed path = %q, want %q", rep.failures[0].path, badPath)
	}
	if rep.failures[0].err == nil || !strings.Contains(rep.failures[0].err.Error(), "invalid file name") {
		t.Errorf("failure err = %v, want the \"invalid file name\" diagnostic", rep.failures[0].err)
	}
}

// TestHandlerRootWalkErrorAborts pins the R1-1 walk contract: only an error
// on the source root itself is returned by the handler (main then exits 2);
// everything else is recorded per file.
func TestHandlerRootWalkErrorAborts(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	cfg := config{
		sourceRoot:  missing,
		destination: t.TempDir(),
		extensions:  normaliseExtensions(""),
	}
	rep := &runReport{}

	err := filepath.Walk(missing, newHandler(cfg, rep))
	if err == nil {
		t.Fatal("root walk error must be returned (main turns it into exit 2)")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
	if rep.attempted != 0 {
		t.Errorf("attempted = %d, want 0 (nothing was processed)", rep.attempted)
	}
}

// TestHandlerRecordsNestedWalkErrors pins the other half of the R1-1 walk
// contract: an error on a NESTED entry (unreadable subdir, vanishing file)
// is recorded as a per-file failure and the walk CONTINUES — only the
// source root itself aborts (covered by TestHandlerRootWalkErrorAborts).
// Driven by invoking the WalkFunc directly with a synthetic error so the
// branch is deterministic without depending on filesystem permissions
// (chmod 000 behaves differently when tests run as root).
func TestHandlerRecordsNestedWalkErrors(t *testing.T) {
	srcDir := t.TempDir()
	cfg := config{
		sourceRoot:  srcDir,
		destination: t.TempDir(),
		extensions:  normaliseExtensions(""),
	}
	rep := &runReport{}
	handler := newHandler(cfg, rep)

	// Nested entry error: recorded, handler returns nil (batch continues).
	nested := filepath.Join(srcDir, "sub", "clip.ts")
	if err := handler(nested, nil, errors.New("permission denied")); err != nil {
		t.Fatalf("nested walk error returned %v, want nil (batch continues)", err)
	}
	if rep.attempted != 1 || len(rep.failures) != 1 {
		t.Fatalf("attempted = %d, failures = %d, want 1/1", rep.attempted, len(rep.failures))
	}
	if rep.failures[0].path != nested || rep.failures[0].err == nil {
		t.Errorf("failure = %+v, want the nested path carrying its error", rep.failures[0])
	}

	// Root error: returned unchanged so main can turn it into exit 2.
	rootErr := errors.New("root unreadable")
	if err := handler(srcDir, nil, rootErr); !errors.Is(err, rootErr) {
		t.Errorf("root walk error = %v, want it returned unchanged", err)
	}
}

// TestPrepareFileGraftsConfig pins getVideoInfo (exec-free since R1-6) plus
// the config grafting that replaced H1's pre-error nil writes: a value
// VideoInfo comes back on success, a zero value + error on a bad filename —
// the caller structurally cannot dereference a failed probe.
func TestPrepareFileGraftsConfig(t *testing.T) {
	cfg := config{
		sourceRoot:   "/src",
		destination:  "/out",
		extensions:   normaliseExtensions(""),
		customSeason: "3",
		reencode:     true,
		plan:         encoderPlan{Vaapi: true, Device: "/dev/dri/renderD129"},
	}

	info, err := cfg.prepareFile("/src/show s01e01.mp4")
	if err != nil {
		t.Fatalf("prepareFile: %v", err)
	}
	if info.FilePath != "/src/show s01e01.mp4" {
		t.Errorf("FilePath = %q", info.FilePath)
	}
	if info.Format != "show s01e01.mp4" {
		t.Errorf("Format = %q", info.Format)
	}
	if info.FileMKV != "show_s01e01.mkv" {
		t.Errorf("FileMKV = %q, want %q (sanitised, .mkv)", info.FileMKV, "show_s01e01.mkv")
	}
	if info.Season != "s01" {
		t.Errorf("Season = %q, want %q", info.Season, "s01")
	}
	if info.DestinationPath != "/out" || info.CustomSeason != "3" || !info.Reencode {
		t.Errorf("config grafts missing: %+v", info)
	}
	if !reflect.DeepEqual(info.EncodePlan, cfg.plan) {
		t.Errorf("EncodePlan = %+v, want %+v", info.EncodePlan, cfg.plan)
	}

	// Bad filename: zero value + error, never a partially-filled struct.
	bad, err := cfg.prepareFile("/src/.mp4")
	if err == nil {
		t.Fatal("leading-dot filename must be rejected")
	}
	if !strings.Contains(err.Error(), "invalid file name") {
		t.Errorf("err = %v, want the \"invalid file name\" diagnostic", err)
	}
	if !reflect.DeepEqual(bad, VideoInfo{}) {
		t.Errorf("bad-file result = %+v, want the zero value", bad)
	}
}

// TestRunReportAggregation pins the R1-1 end-of-run summary: failures are
// counted against attempted, the downgrade line renders when --reencode was
// auto-downgraded (the reencodeDowngraded flag is carried into the summary),
// and the failure list collapses after summaryErrorLimit entries.
func TestRunReportAggregation(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	t.Run("all-success", func(t *testing.T) {
		buf.Reset()
		var rep runReport
		rep.record("a.mkv", nil)
		rep.record("b.mkv", nil)
		rep.printSummary()
		if rep.attempted != 2 || len(rep.failures) != 0 {
			t.Errorf("attempted = %d, failures = %d, want 2/0", rep.attempted, len(rep.failures))
		}
		if want := "Finished: 2 file(s) processed, 0 failed."; !strings.Contains(buf.String(), want) {
			t.Errorf("summary = %q, want it to contain %q", buf.String(), want)
		}
	})

	t.Run("failures-counted-and-downgrade-carried", func(t *testing.T) {
		buf.Reset()
		var rep runReport
		rep.record("ok1.mkv", nil)
		rep.record("bad1.mkv", errors.New("boom"))
		rep.record("ok2.mkv", nil)
		rep.record("bad2.mkv", errors.New("bang"))
		rep.reencodeDowngraded = true // as set by main: reencode requested, no device
		rep.printSummary()

		out := buf.String()
		if rep.attempted != 4 || len(rep.failures) != 2 {
			t.Errorf("attempted = %d, failures = %d, want 4/2", rep.attempted, len(rep.failures))
		}
		if want := "--reencode skipped: no usable VAAPI device — 2 files stream-copied"; !strings.Contains(out, want) {
			t.Errorf("summary = %q, want it to contain %q", out, want)
		}
		if want := "2 of 4 files failed:"; !strings.Contains(out, want) {
			t.Errorf("summary = %q, want it to contain %q", out, want)
		}
		for _, want := range []string{"bad1.mkv: boom", "bad2.mkv: bang"} {
			if !strings.Contains(out, want) {
				t.Errorf("summary = %q, want it to contain %q", out, want)
			}
		}
	})

	t.Run("downgrade-line-with-zero-failures", func(t *testing.T) {
		buf.Reset()
		rep := runReport{reencodeDowngraded: true}
		rep.record("only.mkv", nil)
		rep.printSummary()
		if want := "--reencode skipped: no usable VAAPI device — 1 files stream-copied"; !strings.Contains(buf.String(), want) {
			t.Errorf("summary = %q, want it to contain %q", buf.String(), want)
		}
	})

	t.Run("failure-list-truncated", func(t *testing.T) {
		buf.Reset()
		var rep runReport
		for i := 0; i < summaryErrorLimit+2; i++ {
			rep.record(fmt.Sprintf("f%d.mkv", i), errors.New("err"))
		}
		rep.printSummary()
		out := buf.String()
		if want := fmt.Sprintf("%d of %d files failed:", summaryErrorLimit+2, summaryErrorLimit+2); !strings.Contains(out, want) {
			t.Errorf("summary = %q, want it to contain %q", out, want)
		}
		if want := "... and 2 more"; !strings.Contains(out, want) {
			t.Errorf("summary = %q, want it to contain %q", out, want)
		}
	})
}

// --- filesystem helpers -----------------------------------------------------

func TestEnsureDirectoryExists(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := ensureDirectoryExists(nested); err != nil {
		t.Fatalf("create nested: %v", err)
	}
	if st, err := os.Stat(nested); err != nil || !st.IsDir() {
		t.Fatalf("nested dir missing: %v", err)
	}
	// Idempotent on an existing directory.
	if err := ensureDirectoryExists(nested); err != nil {
		t.Errorf("existing dir: %v, want nil", err)
	}
	// Error path: a FILE occupies the parent path.
	f := filepath.Join(root, "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureDirectoryExists(filepath.Join(f, "child")); err == nil {
		t.Error("want an error when the parent path is a file")
	} else if !strings.Contains(err.Error(), "failed to create directory") {
		t.Errorf("err = %v, want the \"failed to create directory\" wrap", err)
	}
}

func TestFileExists(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x")

	exists, err := fileExists(p)
	if err != nil || exists {
		t.Errorf("missing file: exists=%v err=%v, want false/nil", exists, err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exists, err = fileExists(p)
	if err != nil || !exists {
		t.Errorf("present file: exists=%v err=%v, want true/nil", exists, err)
	}
	// Error path: ENOTDIR is neither "exists" nor "does not exist".
	exists, err = fileExists(filepath.Join(p, "child"))
	if err == nil {
		t.Fatal("want an error when a path component is a file")
	}
	if exists {
		t.Error("exists must be false on the error path")
	}
	if !strings.Contains(err.Error(), "failed to check if file") {
		t.Errorf("err = %v, want the \"failed to check if file\" wrap", err)
	}
}

// TestTailBuffer pins the bounded stderr capture: oldest bytes are dropped
// as new ones arrive, an oversized single write keeps only its tail, and the
// buffer never exceeds its limit.
func TestTailBuffer(t *testing.T) {
	t.Run("small-writes-accumulate", func(t *testing.T) {
		var b tailBuffer
		b.limit = 10
		if n, err := b.Write([]byte("abc")); err != nil || n != 3 {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if _, err := b.Write([]byte("def")); err != nil {
			t.Fatal(err)
		}
		if got := b.String(); got != "abcdef" {
			t.Errorf("String = %q, want %q", got, "abcdef")
		}
	})
	t.Run("overflow-drops-oldest", func(t *testing.T) {
		var b tailBuffer
		b.limit = 5
		if _, err := b.Write([]byte("abc")); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Write([]byte("defg")); err != nil {
			t.Fatal(err)
		}
		if got := b.String(); got != "cdefg" {
			t.Errorf("String = %q, want %q", got, "cdefg")
		}
	})
	t.Run("oversized-write-keeps-tail", func(t *testing.T) {
		var b tailBuffer
		b.limit = 4
		if _, err := b.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
		if got := b.String(); got != "6789" {
			t.Errorf("String = %q, want %q", got, "6789")
		}
	})
}

// --- fake-tool exec paths (hermetic: stubs on PATH) -------------------------

// TestMoveVideoProbeFailureLeavesNoDestination pins the R1-4 probe-failure
// recipe hermetically: ffprobe fails on unreadable content, the wrapped
// error surfaces ffprobe's diagnostic, and — because the probe runs BEFORE
// the temp file is created — the destination stays empty with no .part
// residue. PATH holds ONLY the ffprobe stub, so any attempt to reach ffmpeg
// would fail loudly and break the assertions.
func TestMoveVideoProbeFailureLeavesNoDestination(t *testing.T) {
	installFakeFFprobe(t, "", "Invalid data found when processing input", 1)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "junk.ts")
	if err := os.WriteFile(src, []byte("not a transport stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	destRoot := t.TempDir()

	info, err := (config{destination: destRoot}).prepareFile(src)
	if err != nil {
		t.Fatalf("prepareFile: %v", err)
	}
	err = moveVideo(info)
	if err == nil {
		t.Fatal("want a probe failure on unreadable content")
	}
	if !strings.Contains(err.Error(), "probe failed for") {
		t.Errorf("err = %v, want the probe wrap", err)
	}
	if !strings.Contains(err.Error(), "Invalid data found when processing input") {
		t.Errorf("err = %v, want ffprobe's diagnostic surfaced", err)
	}

	destDir, err := destinationFor(destRoot, info.Season, info.CustomSeason)
	if err != nil {
		t.Fatalf("destinationFor: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destDir, info.FileMKV)); !os.IsNotExist(statErr) {
		t.Errorf("destination must not exist after a failed probe (stat err: %v)", statErr)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read dest dir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("residue after failed probe: %s", e.Name())
	}
}

// TestMoveVideoRejectsBadSeason pins the defence-in-depth guard: moveVideo
// builds the destination with destinationFor FIRST, so an invalid season
// (main validates --series at startup, but the value is re-checked here)
// fails the file before any directory or temp file is touched.
func TestMoveVideoRejectsBadSeason(t *testing.T) {
	info := VideoInfo{
		FilePath:        "x.ts",
		FileMKV:         "x.mkv",
		DestinationPath: t.TempDir(),
		CustomSeason:    "../../etc",
	}
	err := moveVideo(info)
	if err == nil {
		t.Fatal("want an invalid-season error")
	}
	if !strings.Contains(err.Error(), "invalid season") {
		t.Errorf("err = %v, want the \"invalid season\" diagnostic", err)
	}
	// Nothing may have been created under the destination root.
	entries, readErr := os.ReadDir(info.DestinationPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("destination root not empty after rejection: %v", entries)
	}
}

// TestProbeFileWithFakeFFprobe drives probeFile's three outcomes against an
// ffprobe stub — JSON parse, exit failure with surfaced stderr, malformed
// JSON — so probeFile is covered without a real ffprobe on PATH.
func TestProbeFileWithFakeFFprobe(t *testing.T) {
	t.Run("parses-stream-json", func(t *testing.T) {
		const js = `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":1920},` +
			`{"index":1,"codec_name":"","codec_type":"data"}]}`
		_, marker := installFakeFFprobe(t, js, "", 0)

		res, err := probeFile("/src/any.mp4")
		if err != nil {
			t.Fatalf("probeFile: %v", err)
		}
		if len(res.Streams) != 2 {
			t.Fatalf("streams = %d, want 2", len(res.Streams))
		}
		if res.Streams[0].CodecName != "h264" || res.Streams[0].Width != 1920 {
			t.Errorf("stream 0 = %+v", res.Streams[0])
		}
		// Empty codec_name must survive the round-trip (real tmcd shape).
		if res.Streams[1].CodecType != "data" || res.Streams[1].CodecName != "" {
			t.Errorf("stream 1 = %+v, want the empty-codec_name data stream", res.Streams[1])
		}
		lines := markerLines(t, marker)
		if len(lines) != 1 || !strings.Contains(lines[0], "-show_streams") {
			t.Errorf("ffprobe invocations = %q, want exactly one -show_streams call", lines)
		}
	})

	t.Run("exit-error-surfaces-stderr", func(t *testing.T) {
		installFakeFFprobe(t, "", "Invalid data found when processing input", 1)
		_, err := probeFile("/src/junk.ts")
		if err == nil {
			t.Fatal("want an error from a failing ffprobe")
		}
		if !strings.Contains(err.Error(), "ffprobe execution failed") {
			t.Errorf("err = %v, want the probe wrap", err)
		}
		if !strings.Contains(err.Error(), "Invalid data found when processing input") {
			t.Errorf("err = %v, want ffprobe's stderr surfaced", err)
		}
	})

	t.Run("malformed-json", func(t *testing.T) {
		installFakeFFprobe(t, "not-json", "", 0)
		_, err := probeFile("/src/any.mp4")
		if err == nil {
			t.Fatal("want an error from unparseable output")
		}
		if !strings.Contains(err.Error(), "failed to parse ffprobe JSON output") {
			t.Errorf("err = %v, want the parse wrap", err)
		}
	})
}

// TestMoveVideoSuccessPublishesAtomically drives the full success path with
// stub tools: probe -> remux into the temp .part.mkv -> chmod 0644 -> atomic
// rename onto the destination, no .part residue, and a second run skips via
// exists-check without touching either tool again.
func TestMoveVideoSuccessPublishesAtomically(t *testing.T) {
	const probeJSON = `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"}]}`
	_, marker := installStubs(t, map[string]string{
		"ffprobe": fmt.Sprintf("printf '%%s' '%s'", probeJSON),
		// ffmpeg: write our payload to its LAST argument (the temp file).
		"ffmpeg": "for a in \"$@\"; do out=\"$a\"; done\nprintf 'remuxed' > \"$out\"\nexit 0",
	})

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "show s01e01.mp4")
	if err := os.WriteFile(src, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	destRoot := t.TempDir()

	info, err := (config{destination: destRoot}).prepareFile(src)
	if err != nil {
		t.Fatalf("prepareFile: %v", err)
	}
	if err := moveVideo(info); err != nil {
		t.Fatalf("moveVideo: %v", err)
	}

	destDir, err := destinationFor(destRoot, info.Season, info.CustomSeason)
	if err != nil {
		t.Fatalf("destinationFor: %v", err)
	}
	destPath := filepath.Join(destDir, info.FileMKV)
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("destination missing after success: %v", err)
	}
	if string(got) != "remuxed" {
		t.Errorf("destination content = %q, want the ffmpeg payload", got)
	}
	st, err := os.Stat(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o644 {
		t.Errorf("destination mode = %o, want 644 (temp file is created 0600)", perm)
	}
	// Atomic publish leaves no temp/part residue behind.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "part") {
			t.Errorf("temp residue left in destination: %s", e.Name())
		}
	}

	// Both tools ran exactly once: one ffprobe probe, one ffmpeg remux.
	var probed, remuxed int
	for _, line := range markerLines(t, marker) {
		switch {
		case strings.Contains(line, "-show_streams"):
			probed++
		case strings.Contains(line, "-fflags"):
			remuxed++
		}
	}
	if probed != 1 || remuxed != 1 {
		t.Errorf("invocations: probed=%d remuxed=%d, want 1/1 (lines: %v)", probed, remuxed, markerLines(t, marker))
	}

	// Skip-if-exists: a repeat run must not touch either tool.
	before := len(markerLines(t, marker))
	if err := moveVideo(info); err != nil {
		t.Fatalf("second moveVideo (exists): %v", err)
	}
	if after := len(markerLines(t, marker)); after != before {
		t.Errorf("skip path invoked a tool: %d -> %d marker lines", before, after)
	}
}

// TestAtomicRemuxFailureLeavesNoDestination is the suite's ONE optional
// test (R1-4): it needs the real ffmpeg + ffprobe and skips when they are
// absent, so the Gate A hermetic run stays green with no ffmpeg on PATH.
// Fixture recipe from DEV_NOTES: an mp4 carrying a mov_text subtitle probes
// fine but the -c copy remux into Matroska fails (exit 218) — the failure
// must surface ffmpeg's stderr tail and leave neither the destination file
// nor any .part residue behind.
func TestAtomicRemuxFailureLeavesNoDestination(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH (optional real-tool test)")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH (optional real-tool test; ships with ffmpeg)")
	}

	dir := t.TempDir()
	sub := filepath.Join(dir, "sub.srt")
	if err := os.WriteFile(sub, []byte("1\n00:00:00,000 --> 00:00:01,000\nhello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "mov_text.mp4")
	build := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-i", sub,
		"-c:v", "mpeg4", "-c:a", "aac", "-c:s", "mov_text",
		"-map", "0:v", "-map", "1:a", "-map", "2:s",
		src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build failed: %v\n%s", err, out)
	}

	destRoot := t.TempDir()
	info, err := (config{destination: destRoot}).prepareFile(src)
	if err != nil {
		t.Fatalf("prepareFile: %v", err)
	}

	err = moveVideo(info)
	if err == nil {
		t.Fatal("remux of mov_text into Matroska must fail")
	}
	if !strings.Contains(err.Error(), "ffmpeg stderr tail") {
		t.Errorf("error must carry ffmpeg's stderr tail, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Subtitle codec 94213 is not supported") {
		t.Errorf("stderr tail must carry the muxer diagnostic, got: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status") {
		t.Errorf("error must carry the child exit status, got: %v", err)
	}

	destDir, err := destinationFor(destRoot, info.Season, info.CustomSeason)
	if err != nil {
		t.Fatalf("destinationFor: %v", err)
	}
	destPath := filepath.Join(destDir, info.FileMKV)
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Errorf("destination %s must not exist after a failed remux (stat err: %v)", destPath, err)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "part") {
			t.Errorf(".part residue left behind: %s", e.Name())
		}
	}
}

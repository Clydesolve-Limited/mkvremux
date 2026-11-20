// exitcode_test.go — Gate A criterion 6: the exit-code contract, pinned by
// subprocess tests.
//
// main() terminates with os.Exit on every path, so these tests use the
// standard Go idiom for exercising os.Exit from a test binary:
// TestHelperProcess re-execs the test binary (os.Args[0]) with
// -test.run=^TestHelperProcess$ and the application argv after "--"; the
// child rebuilds os.Args and calls the real main(), and the parent asserts
// the child's exit code TOGETHER WITH the key message fragment — an
// unrelated failure carrying the same code must not pass.
//
// Contract (WORK_LOG.md "Exit-code contract", in force since R1-1):
//
//	0  success, incl. --help and the empty-source/destination usage prints
//	1  run completed with at least one recorded file failure
//	2  usage/preflight: bad --series, bad --hwaccel (incl. "software"),
//	   missing ffmpeg/ffprobe on PATH, unreadable source root,
//	   strict --hwaccel=vaapi refusal
//	80 kong's own missing-positional-args usage error (kong's default —
//	   pinned so a kong upgrade cannot silently change it)
//
// Hermeticity (Gate A): every case runs with PATH pointed at a fresh temp
// dir — EMPTY for the fail-fast preflight cases (proving they exit before
// any tool is needed) or holding shell-builtin-only stubs from installStubs.
// No network, no GPU, no t.Parallel (PATH, the log writer and the hwaccel
// seams are process-global).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelperProcess is the subprocess entry point for the exit-code tests
// below. It is a no-op in the parent test process (the helper env var is
// only ever set on spawned children); in the child it rebuilds os.Args from
// the argv after "--" and calls the real main(), which exits with the
// contract code. Reaching the os.Exit(0) below means main() returned — the
// success path.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MAKE_MKV_EXITCODE_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"makeMKV"}, flag.Args()...)
	// Sentinel printed by the HELPER, not by main(): since R2-1 the startup
	// banner prints only after a successful kong.Parse, so --help, --version
	// and parse-error paths (exit 80) exit inside Parse and never reach it.
	// This line proves the -test.run match worked and main() is next; without
	// it a broken match would yield a spurious exit 0 with no app output.
	fmt.Fprintln(os.Stderr, "makeMKV test helper: entering main")
	main()
	os.Exit(0) // main only returns on the exit-0 success path
}

// runCLI drives the real main() in a subprocess with the given application
// argv and returns its exit code plus combined stdout+stderr. The child
// inherits the caller's environment, so a t.Setenv("PATH", ...) done before
// this call is exactly the PATH the CLI runs under; the GOCOVERDIR set by
// "go test -cover" is inherited too, so the child's coverage of main()
// merges into the reported profile.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "GO_WANT_MAKE_MKV_EXITCODE_HELPER=1")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("helper subprocess did not run: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	// The helper prints its sentinel before calling main(); without it the
	// -test.run match was broken and a spurious clean exit-0 assertion
	// could pass without any application code having run.
	if !strings.Contains(string(out), "makeMKV test helper: entering main") {
		t.Fatalf("helper output lacks the entering-main sentinel — main() never ran:\n%s", out)
	}
	return code, string(out)
}

// wantExit asserts the exit-code contract for one invocation: the exact code
// AND every key message fragment, so a wrong-but-same-code failure cannot
// pass.
func wantExit(t *testing.T, gotCode, wantCode int, out string, wantFragments ...string) {
	t.Helper()
	if gotCode != wantCode {
		t.Errorf("exit code = %d, want %d\noutput:\n%s", gotCode, wantCode, out)
	}
	for _, want := range wantFragments {
		if !strings.Contains(out, want) {
			t.Errorf("output must contain %q\noutput:\n%s", want, out)
		}
	}
}

// TestExitZeroHelpPrintsUsage pins "--help" → exit 0 with the usage printed
// (kong exits inside its help handler, before any preflight runs — hence the
// empty PATH: no tool may be needed).
func TestExitZeroHelpPrintsUsage(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	code, out := runCLI(t, "--help")
	wantExit(t, code, 0, out,
		"Usage: makeMKV <source> <destination-directory>",
		"--series", // full help (Arguments + Flags), not just the summary line
	)
}

// TestExitZeroVersionFlag pins the R2-1 --version contract: one parseable
// line ("mkvremux version X.Y.Z") from the single-source toolVersion, exit
// 0, and NO startup banner — kong's VersionFlag exits inside Parse, before
// the banner (which prints only after a successful parse) and before any
// tool preflight (hence the empty PATH).
func TestExitZeroVersionFlag(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	code, out := runCLI(t, "--version")
	wantExit(t, code, 0, out, "mkvremux version "+toolVersion)
	// The banner text is "mkvremux v<toolVersion>"; it must not appear.
	// (Note: "mkvremux version 0.1.0" does NOT contain "mkvremux v0.1.0".)
	if strings.Contains(out, "mkvremux v"+toolVersion) {
		t.Errorf("startup banner must not print for --version\noutput:\n%s", out)
	}
}

// TestExitZeroEmptySourceOrDestinationUsage pins the documented usage
// prints: an empty <source>/<destination-directory> positional prints the
// summary usage and exits 0 — success per the contract table (WORK_LOG.md).
func TestExitZeroEmptySourceOrDestinationUsage(t *testing.T) {
	t.Run("empty-source", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		code, out := runCLI(t, "", t.TempDir())
		wantExit(t, code, 0, out, "Usage: makeMKV <source> <destination-directory>")
	})
	t.Run("empty-destination", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		code, out := runCLI(t, t.TempDir(), "")
		wantExit(t, code, 0, out, "Usage: makeMKV <source> <destination-directory>")
	})
}

// TestExitZeroCleanRun pins the happy path end to end with stub tools: one
// file is probed and remuxed, published atomically, the startup banner
// ("mkvremux v<toolVersion>", R2-1 — printed only after a successful
// kong.Parse) is present, the summary reads "Finished ... 0 failed" and the
// run exits 0. The marker proves each tool ran exactly once; the remuxed
// payload is verified from the parent after the child exits.
func TestExitZeroCleanRun(t *testing.T) {
	const probeJSON = `{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"}]}`
	_, marker := installStubs(t, map[string]string{
		"ffprobe": fmt.Sprintf("printf '%%s' '%s'", probeJSON),
		// ffmpeg: write the payload to its LAST argument (the temp file).
		"ffmpeg": "for a in \"$@\"; do out=\"$a\"; done\nprintf 'remuxed' > \"$out\"\nexit 0",
	})

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "show s01e01.mp4")
	if err := os.WriteFile(src, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	destRoot := t.TempDir()

	code, out := runCLI(t, src, destRoot)
	// Positive banner assertion (the mutation-drill gap): a normal run must
	// print the startup banner built from the single-source toolVersion —
	// never a hardcoded version literal.
	wantExit(t, code, 0, out,
		"mkvremux v"+toolVersion,
		"Finished: 1 file(s) processed, 0 failed.")

	destDir, err := destinationFor(destRoot, getSeason(src), "")
	if err != nil {
		t.Fatalf("destinationFor: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destDir, "show_s01e01.mkv"))
	if err != nil {
		t.Fatalf("destination missing after clean run: %v\noutput:\n%s", err, out)
	}
	if string(got) != "remuxed" {
		t.Errorf("destination content = %q, want the ffmpeg payload", got)
	}

	// Exactly one probe and one remux — nothing else touched the tools.
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
		t.Errorf("invocations: probed=%d remuxed=%d, want 1/1 (lines: %v)",
			probed, remuxed, markerLines(t, marker))
	}
}

// TestExitZeroValidSeriesAccepted pins the valid --series branch: the value
// is accepted and logged and the run proceeds to exit 0 (the rejection half
// is pinned in TestExitTwoPreflightFailures).
func TestExitZeroValidSeriesAccepted(t *testing.T) {
	installStubs(t, map[string]string{"ffmpeg": "exit 0\n", "ffprobe": "exit 0\n"})
	code, out := runCLI(t, "--series", "3", t.TempDir(), t.TempDir())
	wantExit(t, code, 0, out,
		"Using custom series number: 3",
		"Finished: 0 file(s) processed, 0 failed.")
}

// TestExitZeroHardwareWarningsDoNotFail pins the two approved warning paths
// that must NOT change the exit code: --reencode+auto downgrading to stream
// copy (startup warning + end-of-run summary line) and --hwaccel=vaapi
// without --reencode (one-line warning, run proceeds as plain copy).
func TestExitZeroHardwareWarningsDoNotFail(t *testing.T) {
	t.Run("reencode-auto-downgrades-to-copy", func(t *testing.T) {
		// ffmpeg lists NO hevc_vaapi: the capability scan fails, auto mode
		// falls back to copy — and never reaches device discovery, so the
		// case is GPU-free by construction.
		installStubs(t, map[string]string{
			"ffmpeg":  "printf '%s\\n' ' V..... libx264  H.264 / AVC / MPEG-4 AVC (codec h264)'\nexit 0",
			"ffprobe": "exit 0\n",
		})
		code, out := runCLI(t, "--reencode", t.TempDir(), t.TempDir())
		wantExit(t, code, 0, out,
			errAutoFallback,
			"--reencode skipped: no usable VAAPI device — 0 files stream-copied",
			"Finished: 0 file(s) processed, 0 failed.")
	})
	t.Run("vaapi-without-reencode-warns-and-proceeds", func(t *testing.T) {
		installStubs(t, map[string]string{"ffmpeg": "exit 0\n", "ffprobe": "exit 0\n"})
		code, out := runCLI(t, "--hwaccel", "vaapi", t.TempDir(), t.TempDir())
		wantExit(t, code, 0, out,
			fmt.Sprintf(errHwaccelNoReencode, hwaccelVAAPI),
			"Finished: 0 file(s) processed, 0 failed.")
	})
}

// TestExitOneFileFailureSummary pins exit 1 for a batch that COMPLETED with
// failures: the badly named ".mp4" fails exec-free in prepareFile, the good
// file succeeds via its pre-created destination (skip-if-exists — also
// exec-free), so the summary must read "1 of 2 files failed". The PATH
// stubs exist only to satisfy requireTools' LookPath — the marker proves
// neither tool was ever executed.
func TestExitOneFileFailureSummary(t *testing.T) {
	_, marker := installStubs(t, map[string]string{"ffmpeg": "exit 0\n", "ffprobe": "exit 0\n"})

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, ".mp4"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	goodPath := filepath.Join(srcDir, "show s01e01.mp4")
	if err := os.WriteFile(goodPath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	destRoot := t.TempDir()
	destDir, err := destinationFor(destRoot, getSeason(goodPath), "")
	if err != nil {
		t.Fatalf("destinationFor: %v", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "show_s01e01.mkv"),
		[]byte("already remuxed"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := runCLI(t, srcDir, destRoot)
	wantExit(t, code, 1, out,
		"1 of 2 files failed",
		"invalid file name", // the .mp4 failure's diagnostic, repeated in the summary
	)

	// LookPath passed preflight, but this batch must not EXEC anything.
	if lines := markerLines(t, marker); len(lines) != 0 {
		t.Errorf("tools must not run for this batch, but invocations recorded: %v", lines)
	}
}

// TestExitTwoPreflightFailures pins exit 2 for every usage/preflight
// failure, each with its key message so an unrelated error carrying the
// same code cannot pass. The first five cases run with an EMPTY or
// ffmpeg-only PATH on purpose: --series/--hwaccel are validated BEFORE
// requireTools, so they must fail fast with no tools installed at all.
func TestExitTwoPreflightFailures(t *testing.T) {
	t.Run("bad-series", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		code, out := runCLI(t, "--series", "abc", t.TempDir(), t.TempDir())
		wantExit(t, code, 2, out,
			"Invalid --series value",
			`invalid season "abc"`)
	})

	t.Run("hwaccel-software", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		code, out := runCLI(t, "--reencode", "--hwaccel", "software", t.TempDir(), t.TempDir())
		wantExit(t, code, 2, out,
			`Invalid --hwaccel value "software"`,
			"no software re-encode mode")
	})

	t.Run("hwaccel-bogus", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		code, out := runCLI(t, "--hwaccel", "bogus", t.TempDir(), t.TempDir())
		wantExit(t, code, 2, out,
			`Invalid --hwaccel value "bogus"`,
			"must be auto or vaapi")
	})

	t.Run("missing-ffmpeg", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // no tools at all
		code, out := runCLI(t, t.TempDir(), t.TempDir())
		wantExit(t, code, 2, out,
			`Required tool "ffmpeg" not found on PATH`,
			`sudo apt install ffmpeg`)
	})

	t.Run("missing-ffprobe", func(t *testing.T) {
		// PATH holds an ffmpeg stub only: ffmpeg passes LookPath, ffprobe must not.
		installStubs(t, map[string]string{"ffmpeg": "exit 0\n"})
		code, out := runCLI(t, t.TempDir(), t.TempDir())
		wantExit(t, code, 2, out, `Required tool "ffprobe" not found on PATH`)
	})

	t.Run("nonexistent-source-root", func(t *testing.T) {
		installStubs(t, map[string]string{"ffmpeg": "exit 0\n", "ffprobe": "exit 0\n"})
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		code, out := runCLI(t, missing, t.TempDir())
		wantExit(t, code, 2, out, "Failed to walk through directory")
	})

	t.Run("strict-vaapi-refusal", func(t *testing.T) {
		// ffmpeg's -encoders scan lists no hevc_vaapi → the strict
		// --hwaccel=vaapi refusal (errExit2) must exit 2 before any file
		// is touched — deterministic with or without a GPU.
		installStubs(t, map[string]string{
			"ffmpeg":  "printf '%s\\n' ' V..... libx264  H.264 / AVC / MPEG-4 AVC (codec h264)'\nexit 0",
			"ffprobe": "exit 0\n",
		})
		code, out := runCLI(t, "--reencode", "--hwaccel", "vaapi", t.TempDir(), t.TempDir())
		wantExit(t, code, 2, out, errVaapiStrict)
	})
}

// TestExitEightyKongMissingArgs pins kong's own missing-positional-args
// usage error to exit 80 — deliberately, so a kong upgrade cannot silently
// change the code — and asserts kong's diagnostic alongside it.
func TestExitEightyKongMissingArgs(t *testing.T) {
	t.Run("no-args", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		code, out := runCLI(t)
		wantExit(t, code, 80, out, `expected "<source> <destination-directory>"`)
	})
	t.Run("one-arg", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		code, out := runCLI(t, t.TempDir())
		wantExit(t, code, 80, out, `expected "<destination-directory>"`)
	})
}

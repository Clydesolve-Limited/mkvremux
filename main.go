package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/alecthomas/kong"
)

// toolVersion is the single source of truth for the tool's version (R2-1).
// It is bumped manually per RELEASING.md (R2-11); nothing else carries a
// version value — no ldflags -X injection, no counter-file reads.
const toolVersion = "0.1.0"

// VideoInfo carries the per-file state assembled by getVideoInfo and grafted
// on by prepareFile, then consumed by moveVideo to build the destination
// directory and output file name.
type VideoInfo struct {
	FilePath        string
	Format          string
	DestinationPath string

	Season       string
	FileMKV      string
	CustomSeason string
	Reencode     bool
	// EncodePlan is the startup hardware plan grafted on by prepareFile:
	// Vaapi+Device are set only when --reencode resolved a usable device.
	EncodePlan encoderPlan
}

// CLI is the kong-parsed command-line surface: two positional arguments
// (source, destination) plus optional flags.
var CLI struct {
	Source      string           `arg:"" name:"source" help:"Path to source video files"`
	Destination string           `arg:"" name:"destination-directory" help:"Path to destination video files"`
	Series      string           `help:"Specify a custom series number"`
	Extensions  string           `help:"Comma-separated file extensions to process; leading dot optional (e.g. mp4,ts or .mp4,.ts)"`
	Reencode    bool             `help:"Use this to re-encode the video to HEVC"`
	Hwaccel     string           `default:"auto" help:"Hardware device policy for --reencode: auto (default, fall back to copy) or vaapi (strict)"`
	Version     kong.VersionFlag `name:"version" help:"Print version information and quit"`
}

type StreamDisposition struct {
	Default         int `json:"default"`
	Dub             int `json:"dub"`
	Original        int `json:"original"`
	Comment         int `json:"comment"`
	Lyrics          int `json:"lyrics"`
	Karaoke         int `json:"karaoke"`
	Forced          int `json:"forced"`
	HearingImpaired int `json:"hearing_impaired"`
	VisualImpaired  int `json:"visual_impaired"`
	CleanEffects    int `json:"clean_effects"`
	AttachedPic     int `json:"attached_pic"`
	TimedThumbnails int `json:"timed_thumbnails"`
}

type Stream struct {
	// Index is the zero-based position of this stream within the container.
	Index int `json:"index"`

	// CodecName is the short name of the codec, e.g. "h264", "aac", "hevc".
	CodecName string `json:"codec_name"`

	// CodecLongName is the human-readable full name of the codec.
	CodecLongName string `json:"codec_long_name"`

	// CodecType indicates the broad category of the stream: "video", "audio",
	// "subtitle", or "data".
	CodecType string `json:"codec_type"`

	// CodecTagString is the four-character code (FourCC) tag for the codec.
	CodecTagString string `json:"codec_tag_string"`

	// --- Video-specific fields ---

	// Width and Height describe the frame dimensions in pixels (video streams only).
	Width  int `json:"width"`
	Height int `json:"height"`

	// PixFmt is the pixel format used by the video stream, e.g. "yuv420p".
	PixFmt string `json:"pix_fmt"`

	// RFrameRate is the real base frame rate of the stream as a ratio string, e.g. "30/1".
	RFrameRate string `json:"r_frame_rate"`

	// AvgFrameRate is the average frame rate of the stream as a ratio string.
	AvgFrameRate string `json:"avg_frame_rate"`

	// --- Audio-specific fields ---

	// SampleRate is the number of audio samples per second, e.g. "44100".
	SampleRate string `json:"sample_rate"`

	// Channels is the number of audio channels, e.g. 2 for stereo.
	Channels int `json:"channels"`

	// ChannelLayout describes the audio channel layout, e.g. "stereo", "5.1".
	ChannelLayout string `json:"channel_layout"`

	// SampleFmt is the audio sample format, e.g. "fltp", "s16".
	SampleFmt string `json:"sample_fmt"`

	// --- Common fields ---

	// Duration is the length of the stream in seconds as a string.
	Duration string `json:"duration"`

	// BitRate is the average bitrate of the stream in bits per second.
	BitRate string `json:"bit_rate"`

	// Tags holds arbitrary metadata key-value pairs attached to the stream,
	// such as language, title, or handler name.
	Tags map[string]string `json:"tags"`

	// Disposition holds a set of boolean flags describing stream properties.
	Disposition StreamDisposition `json:"disposition"`
}

// ProbeResult is the top-level structure returned by ffprobe when invoked
// with the -show_streams flag. It encapsulates the stream-level information
// for a given file; container-level format data is not requested because
// nothing reads it.
type ProbeResult struct {
	Streams []Stream `json:"streams"`
}

func probeFile(filePath string) (*ProbeResult, error) {
	// Build the ffprobe argument list. We use -v quiet to suppress log output,
	// -print_format json to get machine-readable output, and -show_streams
	// to capture the stream data we need.
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		filePath,
	}

	// Execute ffprobe and capture its combined stdout output. If ffprobe is
	// not installed or not on the PATH, exec.Command will return an error here.
	// On a non-zero exit, exec attaches the child's stderr (up to 32 KB) to
	// the ExitError; surface it so failures carry ffprobe's diagnostic rather
	// than just "exit status 1".
	cmd := exec.Command("ffprobe", args...)
	setChildPdeathsig(cmd) // child dies with makeMKV (no orphaned ffprobe)
	output, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			if tail := strings.TrimSpace(string(exitErr.Stderr)); tail != "" {
				stderr = "; stderr: " + tail
			}
		}
		return nil, fmt.Errorf("ffprobe execution failed for file %q: %w%s", filePath, err, stderr)
	}

	// Unmarshal the JSON output into our ProbeResult struct. Any fields that
	// ffprobe does not include in its output will simply remain zero-valued.
	var result ProbeResult
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe JSON output: %w", err)
	}

	return &result, nil
}

// summariseStreams generates the ffmpeg -map arguments needed to carry every
// mappable stream of the probed file into the MKV output. Streams that cannot
// or must not be mapped are skipped outright:
//   - codec_type "data" (e.g. tmcd timecode) is unsupported by the Matroska
//     muxer and makes ffmpeg fail outright if mapped;
//   - cover art, identified by disposition attached_pic == 1 regardless of
//     codec (png, mjpeg, ...), so it is not re-encoded as a video track.
//
// It returns a flat []string of alternating "-map", "0:N" entries suitable
// for direct append into an ffmpeg argv.
func summariseStreams(result *ProbeResult) []string {
	mappings := []string{}

	for _, stream := range result.Streams {
		if stream.CodecType == "data" || stream.Disposition.AttachedPic == 1 {
			continue
		}

		mappings = append(mappings, "-map", fmt.Sprintf("0:%d", stream.Index))
	}

	return mappings
}

func sanitizeFilename(input string) string {
	var builder strings.Builder

	for _, r := range input {
		switch {
		// Keep all unicode letters and numbers (handles accented chars, CJK, etc.)
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			builder.WriteRune(r)

		// Convert spaces to underscores for a more readable filename.
		case unicode.IsSpace(r):
			builder.WriteRune('_')

		// Preserve a small set of punctuation characters that are safe on Linux.
		case r == '-' || r == '_' || r == '.':
			builder.WriteRune(r)

		// All other characters (emoji, symbols, control chars, slashes, etc.) are dropped.
		default:
			continue
		}
	}

	sanitized := builder.String()

	// Collapse any sequence of underscores or dashes into a single underscore.
	// This prevents filenames like "hello___world" from appearing when multiple
	// unsafe characters appeared consecutively in the original string.
	prevWasSeparator := false
	var collapsed strings.Builder
	for _, r := range sanitized {
		isSeparator := r == '_' || r == '-'
		if isSeparator && prevWasSeparator {
			continue
		}
		collapsed.WriteRune(r)
		prevWasSeparator = isSeparator
	}

	// Trim leading and trailing separators and dots to avoid hidden files or
	// filenames that start/end with awkward punctuation.
	result := strings.Trim(collapsed.String(), "_-.")

	return result
}

// config holds the run options grafted onto every file processed in a batch:
// source/destination roots, extension filter, season override, the re-encode
// request and the startup encode plan resolved from it (R1-7).
type config struct {
	sourceRoot   string
	destination  string
	extensions   map[string]bool
	customSeason string
	reencode     bool
	plan         encoderPlan
}

// fileFailure pairs the path that failed with the error that caused it.
type fileFailure struct {
	path string
	err  error
}

// runReport collects per-file outcomes so main can print an end-of-run
// summary and pick the exit code: 0 on full success, 1 if any file failed.
type runReport struct {
	attempted int // paths handled (matched files + recorded walk errors)
	failures  []fileFailure
	// reencodeDowngraded records that --reencode was requested but the
	// startup plan downgraded it to stream copy (auto + no usable VAAPI
	// device); the summary then states how many files were copied instead
	// of re-encoded.
	reencodeDowngraded bool
}

// summaryErrorLimit is how many individual errors the end-of-run summary
// prints before collapsing the rest into an "and N more" line.
const summaryErrorLimit = 5

// record notes the outcome of handling one path. err == nil records a
// success; non-nil records a failure. Every handled path increments
// attempted, so failures can never exceed the total shown in the summary.
func (r *runReport) record(path string, err error) {
	r.attempted++
	if err != nil {
		r.failures = append(r.failures, fileFailure{path: path, err: err})
	}
}

// printSummary logs the end-of-run totals: on failure, "N of M files failed"
// plus the first few individual errors. When --reencode was auto-downgraded
// to stream copy, the summary first states how many files were copied
// instead of re-encoded (approved A2 matrix).
func (r *runReport) printSummary() {
	if r.reencodeDowngraded {
		// Files that finished without a failure went through the
		// stream-copy branch (the approved auto downgrade).
		log.Printf("--reencode skipped: no usable VAAPI device — %d files stream-copied", r.attempted-len(r.failures))
	}
	if len(r.failures) == 0 {
		log.Printf("Finished: %d file(s) processed, 0 failed.", r.attempted)
		return
	}

	log.Printf("%d of %d files failed:", len(r.failures), r.attempted)
	for i, f := range r.failures {
		if i == summaryErrorLimit {
			log.Printf("  ... and %d more", len(r.failures)-summaryErrorLimit)
			break
		}
		log.Printf("  %s: %v", f.path, f.err)
	}
}

// prepareFile runs the cheap, exec-free half of per-file processing: it reads
// the file's metadata (getVideoInfo) and grafts this run's configuration onto
// the resulting plan. The exec-heavy half is moveVideo. It returns a value
// plan (never a nil pointer), so callers structurally cannot dereference a
// failed probe (the H1 nil-deref bug).
func (cfg config) prepareFile(path string) (VideoInfo, error) {
	info, err := getVideoInfo(path)
	if err != nil {
		return VideoInfo{}, err
	}

	info.DestinationPath = cfg.destination
	info.CustomSeason = cfg.customSeason
	info.Reencode = cfg.reencode
	info.EncodePlan = cfg.plan

	return *info, nil
}

// newHandler builds the filepath.WalkFunc for one run. Per-file problems are
// recorded in rep and never abort the batch; only an error on the source root
// itself is returned, which makes main exit 2.
func newHandler(cfg config, rep *runReport) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		// Walk-level error: unreadable entry or a bad source root.
		if err != nil {
			if path == cfg.sourceRoot {
				return err // root failure aborts the run (main exits 2)
			}
			log.Printf("Skipping unreadable entry %s: %v", path, err)
			rep.record(path, err)
			return nil
		}

		if !cfg.extensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}

		plan, err := cfg.prepareFile(path)
		if err == nil {
			err = moveVideo(plan)
		}
		if err != nil {
			log.Printf("Failed to process %s: %v", path, err)
		}
		rep.record(path, err)

		return nil
	}
}

// requireTools verifies the external tools the batch depends on are on PATH
// before any file is touched. A missing tool is a preflight failure (exit 2),
// not a per-file failure.
func requireTools() {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			log.Printf("Required tool %q not found on PATH. Install ffmpeg (it provides both ffmpeg and ffprobe), e.g. \"sudo apt install ffmpeg\"", tool)
			os.Exit(2)
		}
	}
}

// normaliseExtensions converts an --extensions spec into the canonical set
// the handler matches filepath.Ext against: one lowercased key per entry with
// exactly one leading dot. Entries are accepted with or without the dot
// ("mp4,ts" and ".mp4,.ts" are equivalent), in any case, and tolerate
// surrounding spaces plus stray dots ("..mp4" -> ".mp4"); empty entries are
// skipped. A spec with nothing usable (empty, or only separators/spaces)
// falls back to the default set {.mp4, .ts}. The function is pure so it is
// trivially table-testable.
func normaliseExtensions(spec string) map[string]bool {
	exts := make(map[string]bool)
	for _, ext := range strings.Split(spec, ",") {
		ext = strings.TrimLeft(strings.ToLower(strings.TrimSpace(ext)), ".")
		if ext == "" {
			continue
		}
		exts["."+ext] = true
	}
	if len(exts) == 0 {
		return map[string]bool{".mp4": true, ".ts": true}
	}
	return exts
}

func main() {
	// Parse first: kong's --version/--help handlers exit inside Parse, so
	// both stay clean one-liners with no startup banner ahead of them (R2-1).
	ctx := kong.Parse(&CLI, kong.Vars{"version": "mkvremux version " + toolVersion})

	// Startup banner: only reached on a successful parse (R2-1).
	log.Printf("mkvremux v%s\n", toolVersion)

	var customSeason string
	reencode := false

	if CLI.Source == "" {
		_ = ctx.PrintUsage(true)
		os.Exit(0)
	}

	if CLI.Destination == "" {
		_ = ctx.PrintUsage(false)
		os.Exit(0)
	}

	if CLI.Series != "" {
		// Validate --series once at startup (preflight-class, exit 2) so a
		// bad value fails before any file is touched rather than mid-walk.
		if _, err := destinationFor(CLI.Destination, "", CLI.Series); err != nil {
			log.Printf("Invalid --series value: %v", err)
			os.Exit(2)
		}

		customSeason = CLI.Series

		log.Printf("Using custom series number: %s", customSeason)

	}

	// Validate --hwaccel once at startup (usage error → exit 2), in the
	// same style as the --series check above and BEFORE the tool preflight,
	// so a bad value fails fast regardless of what is installed. Deliberately
	// not kong's enum tag: that exits 1 on parse and would break the 0/1/2
	// exit contract.
	mode, hwErr := parseHwaccel(CLI.Hwaccel)
	if hwErr != nil {
		log.Printf("Invalid --hwaccel value %q: %v", CLI.Hwaccel, hwErr)
		ctx.Stdout = os.Stderr // usage belongs on stderr, with the error
		_ = ctx.PrintUsage(false)
		os.Exit(2)
	}

	if CLI.Reencode {
		reencode = true
	}

	rootDir := CLI.Source
	destDir := CLI.Destination

	extensions := normaliseExtensions(CLI.Extensions)

	log.Printf("Processing extensions: %v", extensions)

	// Preflight: fail fast (exit 2) if ffmpeg/ffprobe are unavailable,
	// before any file in the batch is touched.
	requireTools()

	// Resolve the encode plan BEFORE the walk: a strict --hwaccel=vaapi
	// refusal (errExit2) exits 2 here, before any file is touched, and the
	// auto-downgrade warning renders before processing starts.
	plan, err := selectEncodePlan(context.Background(), mode, CLI.Reencode)
	if err != nil {
		if errors.Is(err, errExit2) {
			log.Printf("%v", err)
		} else {
			log.Printf("Hardware preflight failed: %v", err)
		}
		os.Exit(2)
	}
	for _, w := range plan.Warnings {
		log.Printf("%s", w)
	}
	if !CLI.Reencode && mode == hwaccelVAAPI {
		log.Printf(errHwaccelNoReencode, mode)
	}

	cfg := config{
		sourceRoot:   rootDir,
		destination:  destDir,
		extensions:   extensions,
		customSeason: customSeason,
		reencode:     reencode,
		plan:         plan,
	}
	// Carry the auto downgrade (reencode requested, no usable device) into
	// the end-of-run summary.
	rep := &runReport{reencodeDowngraded: CLI.Reencode && !plan.Vaapi}

	// Walk through the directory and process video files.
	err = filepath.Walk(rootDir, newHandler(cfg, rep))
	if err != nil {
		// Only the handler's root-path errors reach here; a bad source
		// root is a preflight-class failure (exit 2), not a file failure.
		log.Printf("Failed to walk through directory: %v", err)
		os.Exit(2)
	}

	rep.printSummary()
	if len(rep.failures) > 0 {
		os.Exit(1)
	}
}

// seasonRe matches a season token "sNN" (e.g. s01, S02) as a whole word,
// case-insensitively. The leading \b rejects embedded false positives such
// as "Eps01" or "vs12" (the "s" there is not at a word start). The trailing
// check rejects a third digit ("s123") but, unlike a literal \b, still
// accepts the episode suffix of "S01E02"/"s02e05" — Go's RE2 has no
// lookahead, so the token is captured in a group and the lookahead is
// emulated with (?:[^0-9]|$).
var seasonRe = regexp.MustCompile(`(?i)\b(s\d{2})(?:[^0-9]|$)`)

// getSeason returns the first season token found in input, lowercased
// (e.g. "s02"), or "" when there is none. The pattern is a compile-time
// constant, so there is no error to report.
func getSeason(input string) string {
	if match := seasonRe.FindStringSubmatch(input); match != nil {
		return strings.ToLower(match[1])
	}
	return ""
}

func getFileNameWithoutExtension(path string) (string, error) {
	// Extract the base name of the path
	baseName := filepath.Base(path)

	// Find the last period in the base name
	lastDotIndex := strings.LastIndex(baseName, ".")
	if lastDotIndex == -1 || lastDotIndex == 0 || lastDotIndex == len(baseName)-1 {
		return "", fmt.Errorf("invalid file name: %s", baseName)
	}

	// Extract the file name without the extension
	fileNameWithoutExt := baseName[:lastDotIndex]

	return fileNameWithoutExt, nil
}

func getVideoInfo(path string) (*VideoInfo, error) {
	var videoInfo VideoInfo
	videoInfo.FilePath = path
	videoInfo.Format = filepath.Base(path)

	fileBasename, err := getFileNameWithoutExtension(videoInfo.Format)
	if err != nil {
		return nil, err
	}

	fileBasename = sanitizeFilename(fileBasename)

	videoInfo.FileMKV = fileBasename + ".mkv"
	videoInfo.Season = getSeason(videoInfo.FilePath)

	return &videoInfo, nil
}

func ensureDirectoryExists(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // Directory already exists
	}

	err := os.MkdirAll(path, 0755)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %w", path, err)
	}

	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil // File exists
	}
	if os.IsNotExist(err) {
		return false, nil // File does not exist
	}
	return false, fmt.Errorf("failed to check if file %s exists: %w", path, err)
}

// customSeasonRe matches a season component supplied via --series: an optional
// "s" followed by one or two digits (e.g. "3", "S1", "s12"). Anything else,
// including path traversal or absolute paths, is rejected.
var customSeasonRe = regexp.MustCompile(`^[Ss]?[0-9]{1,2}$`)

// destinationFor builds the destination directory for one file. The season
// component is customSeason when set (validated and normalised to "sNN"),
// otherwise the season detected from the file path; when neither is present
// the destination root itself is returned. filepath.Join cleans the result,
// so raw concatenation can never produce a sibling directory outside
// destRoot (e.g. "/out" + "s01" -> "/outs01"). The function is pure so it
// is trivially table-testable.
func destinationFor(destRoot, detectedSeason, customSeason string) (string, error) {
	component := detectedSeason

	if customSeason != "" {
		if !customSeasonRe.MatchString(customSeason) {
			return "", fmt.Errorf("invalid season %q: expected an optional \"s\" followed by 1 or 2 digits (e.g. s01, 3, S12)", customSeason)
		}

		digits := strings.TrimPrefix(strings.ToLower(customSeason), "s")
		if len(digits) == 1 {
			digits = "0" + digits
		}
		component = "s" + digits
	}

	return filepath.Join(destRoot, component), nil
}

// buildFFmpegArgs constructs the complete argv for one ffmpeg invocation.
//
// reencode selects the variant:
//   - false: stream copy — all mapped streams are passed through untouched
//     (the default remux path; -map flags preserve extra audio/subtitle
//     tracks that ffmpeg's default selection would otherwise drop).
//   - true: VAAPI hardware re-encode to HEVC + Opus with the production flag
//     set. -err_detect ignore_err is placed BEFORE -i because it is a
//     decoder-side input option: after -i it has no effect on input errors.
//
// device is the render node resolved by selectEncodePlan (a
// /dev/dri/renderD* path); it feeds -hwaccel_device in the reencode
// variant and is ignored by stream copy (pass "").
// maps are the "-map", "0:N" pairs from summariseStreams, inserted after -i.
// Both variants end with -y so ffmpeg never prompts on stdin when the output
// already exists (R1-4 pre-creates the output file).
func buildFFmpegArgs(reencode bool, device, input, output string, maps []string) []string {
	// Common input-side flags: repair timestamps on broken transport streams.
	args := []string{"-fflags", "+genpts+igndts"}

	if reencode {
		args = append(args,
			"-seekable", "0",
			"-hwaccel", "vaapi",
			"-hwaccel_device", device,
			"-hwaccel_output_format", "vaapi",
			"-err_detect", "ignore_err",
			"-i", input,
		)
		args = append(args, maps...)
		args = append(args,
			"-c:v", "hevc_vaapi",
			"-qp", "23",
			"-pix_fmt", "yuv420p",
			"-c:a", "libopus",
			"-b:a", "128K",
			"-ac", "2",
			"-c:s", "copy",
			"-max_muxing_queue_size", "9999",
			"-y", output,
		)
		return args
	}

	args = append(args, "-i", input)
	args = append(args, maps...)
	args = append(args,
		"-c", "copy",
		"-max_muxing_queue_size", "9999",
		"-y", output,
	)
	return args
}

// stderrTailLimit caps how much of a child process's stderr is retained for
// error reporting. ffmpeg writes its progress/status output to stderr
// continuously, so an unbounded buffer could grow to megabytes on a long
// encode; the useful diagnostic lines are at the end, so only the tail is kept.
const stderrTailLimit = 4096

// tailBuffer is an io.Writer that keeps only the last limit bytes written to
// it. Writes never block or fail, so it is safe to hand straight to exec.Cmd.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.limit {
		// Incoming write alone exceeds the cap: keep only its tail.
		t.buf = append(t.buf[:0], p[n-t.limit:]...)
		return n, nil
	}
	// Drop the oldest bytes so buf+new data stays within the cap.
	if over := len(t.buf) + n - t.limit; over > 0 {
		t.buf = t.buf[over:]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	return string(t.buf)
}

// moveVideo remuxes (or re-encodes) one file into its destination directory.
//
// The output is written atomically: ffmpeg writes into a 0600 ".part" file
// created in the destination directory itself (same filesystem as the final
// path, so the final step can be a rename), and only a fully successful run
// is chmod'ed to 0644 and renamed onto destPath. A failure at any point
// removes the ".part" file and never leaves a partial file at destPath, so
// the skip-if-exists check cannot be poisoned by a half-written result.
func moveVideo(videoInfo VideoInfo) error {

	destDir, err := destinationFor(videoInfo.DestinationPath, videoInfo.Season, videoInfo.CustomSeason)
	if err != nil {
		return err
	}

	destPath := filepath.Join(destDir, videoInfo.FileMKV)

	if err := ensureDirectoryExists(destDir); err != nil {
		return fmt.Errorf("failed to ensure directory %s: %w", destDir, err)
	}

	exists, err := fileExists(destPath)
	if err != nil {
		return err
	}

	if exists {
		fmt.Printf("File %s already exists. Skipping.\n", destPath)
		return nil
	}

	// Proceed with creating or processing the file
	fmt.Printf("File %s does not exists.\n", destPath)

	result, err := probeFile(videoInfo.FilePath)
	if err != nil {
		return fmt.Errorf("probe failed for %s: %w", videoInfo.FilePath, err)
	}

	maps := summariseStreams(result)

	// Re-encode only when the startup plan resolved a usable VAAPI device
	// AND this file was requested to re-encode; otherwise (including the
	// approved auto downgrade) fall back to stream copy.
	reencode := videoInfo.EncodePlan.Vaapi && videoInfo.Reencode
	if reencode {
		fmt.Println("Video acceleration is available...")
	}

	// Temp file lives beside the final path: same filesystem (rename is
	// atomic), no /tmp, and os.CreateTemp gives O_EXCL semantics with a
	// random name opened 0600 (umask/symlink hardening). The name must still
	// END in ".mkv": ffmpeg picks the muxer from the output extension and
	// aborts with "Unable to choose an output format" on a bare ".part"
	// (verified: exit 234). The ".part" marker stays mid-name so leftover
	// files from a crash are identifiable and matchable with *part*.
	tempFile, err := os.CreateTemp(destDir, ".mkvremux-*.part.mkv")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", destDir, err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath) // best-effort cleanup of the freshly created temp file
		return fmt.Errorf("failed to close temp file %s: %w", tempPath, err)
	}

	// Deferred cleanup on every failure path. After a successful rename the
	// path no longer exists, so the IsNotExist check makes this a no-op and
	// the rename path never double-removes.
	defer func() {
		if err := os.Remove(tempPath); err != nil && !os.IsNotExist(err) {
			log.Printf("Warning: leftover temp file %s not removed: %v", tempPath, err)
		}
	}()

	args := buildFFmpegArgs(reencode, videoInfo.EncodePlan.Device, videoInfo.FilePath, tempPath, maps)

	// If debugging is needed, append "-report" to args before running.
	cmd := exec.Command("ffmpeg", args...)
	setChildPdeathsig(cmd) // child dies with makeMKV (no orphaned encode)

	// Capture ffmpeg's stderr into a bounded tail so a failed encode reports
	// the actual diagnostic (e.g. the muxer/codec error) instead of a bare
	// "exit status N". Without this both stdout and stderr go to /dev/null.
	var stderr tailBuffer
	stderr.limit = stderrTailLimit
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			return fmt.Errorf("failed to run ffmpeg for %s; ffmpeg stderr tail:\n%s: %w", videoInfo.FileMKV, tail, err)
		}
		return fmt.Errorf("failed to run ffmpeg for %s: %w", videoInfo.FileMKV, err)
	}

	// Outputs must not be owner-only: ffmpeg wrote the temp file 0600, so
	// relax it to 0644 before it becomes visible under its final name.
	if err := os.Chmod(tempPath, 0644); err != nil {
		return fmt.Errorf("failed to chmod %s: %w", tempPath, err)
	}

	// Atomic publish: same directory, same filesystem, so destPath either
	// holds the complete new file or is untouched.
	if err := os.Rename(tempPath, destPath); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", tempPath, destPath, err)
	}

	return nil
}

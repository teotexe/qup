package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"aurashare/internal/crypto"
	"aurashare/internal/portal"
	"aurashare/internal/stats"

	"github.com/quic-go/quic-go"
)

type CaptureBackend int

const (
	BackendX11 CaptureBackend = iota
	BackendWaylandFFmpeg
	BackendWaylandGStreamer
)

func main() {
	os.Setenv("PULSE_LATENCY_MSEC", "30")
	// Parse CLI flags
	portFlag := flag.Int("port", 50001, "Port to listen on")
	displayFlag := flag.String("display", "", "X11 display to grab (e.g. :0.0). Defaults to $DISPLAY env var.")
	sizeFlag := flag.String("size", "1920x1080", "Video capture dimensions (width x height)")
	fpsFlag := flag.Int("fps", 60, "Frame rate for capturing")
	presetFlag := flag.String("preset", "ultrafast", "x264 encoder preset (e.g., ultrafast, superfast, veryfast, medium)")
	tuneFlag := flag.String("tune", "zerolatency", "x264 encoder tune option")
	gopFlag := flag.Int("g", 30, "GOP (keyframe interval) size. Default is 30 for low-latency fast startup connection.")
	codecFlag := flag.String("codec", "libx264", "H.264 encoder library. Auto-probes hardware acceleration (QSV, NVENC, VA-API) by default.")
	bitrateFlag := flag.Int("bitrate", 8000, "Target video bitrate in kbps (e.g., 8000 for 8 Mbps high-quality 1080p)")
	testFlag := flag.Bool("test", false, "Use a synthetic test video source (lavfi testsrc) instead of X11 capture")
	mockPortalFlag := flag.Bool("mock-portal", false, "Start a mock D-Bus ScreenCast portal in the background (for testing)")
	debugFlag := flag.Bool("debug", false, "Enable verbose diagnostic logging for pipeline debugging")
	headlessFlag := flag.Bool("headless", false, "Use synthetic GStreamer test source (no screen capture, no portal popup). Tests the full GStreamer→FFmpeg→QUIC pipeline.")
	volumeFlag := flag.Float64("volume", 150.0, "Audio volume amplification factor (e.g. 150.0 for 150x volume boost)")
	flag.Parse()

	display := *displayFlag
	if display == "" {
		display = os.Getenv("DISPLAY")
		if display == "" {
			display = ":0.0"
		}
	}

	log.Printf("Starting AuraShare Host (Bob) on port %d...", *portFlag)
	log.Printf("Capture config: TestMode=%v, Headless=%v, Debug=%v, Display=%s, Size=%s, FPS=%d, Codec=%s, GOP=%d, Preset=%s, Tune=%s, Bitrate=%d, Volume=%.1f",
		*testFlag, *headlessFlag, *debugFlag, display, *sizeFlag, *fpsFlag, *codecFlag, *gopFlag, *presetFlag, *tuneFlag, *bitrateFlag, *volumeFlag)

	// Create main context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start Mock D-Bus Portal if requested or configured
	if *mockPortalFlag || os.Getenv("AURASHARE_MOCK_PORTAL") == "1" {
		log.Println("Starting Mock D-Bus ScreenCast Portal...")
		err := portal.StartMockPortal(ctx)
		if err != nil {
			log.Fatalf("Failed to start mock portal: %v", err)
		}
	}

	// Build TLS config for server
	tlsConfig, err := crypto.GenerateServerTLSConfig()
	if err != nil {
		log.Fatalf("Failed to generate TLS configuration: %v", err)
	}

	// Create listener with datagram support enabled
	quicConfig := &quic.Config{
		EnableDatagrams: true,
	}
	listener, err := quic.ListenAddr(fmt.Sprintf(":%d", *portFlag), tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("Failed to start QUIC listener: %v", err)
	}
	defer listener.Close()

	log.Printf("Listening for peers. Please connect using your connect client...")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Shutting down AuraShare Host...")
		cancel()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("Error accepting connection: %v", err)
				continue
			}
		}

		log.Printf("Peer connected from: %s", conn.RemoteAddr().String())
		go handlePeer(ctx, conn, *testFlag, *headlessFlag, *debugFlag, display, *sizeFlag, *fpsFlag, *codecFlag, *gopFlag, *presetFlag, *tuneFlag, *bitrateFlag, *volumeFlag)
	}
}

// supportsPipeWireGrab checks if the local ffmpeg binary supports the pipewiregrab filter.
func supportsPipeWireGrab() bool {
	cmd := exec.Command("ffmpeg", "-filters")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	// Verify if "pipewiregrab" is present in the supported filters list
	return strings.Contains(string(output), "pipewiregrab")
}

// supportsGstElement checks if a GStreamer element is available on the system.
func supportsGstElement(name string) bool {
	cmd := exec.Command("gst-inspect-1.0", name)
	return cmd.Run() == nil
}

// supportsGStreamerPipeWire checks if GStreamer, pipewiresrc, and at least one H.264 encoder are available.
func supportsGStreamerPipeWire() bool {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		return false
	}
	if !supportsGstElement("pipewiresrc") {
		return false
	}
	// Check for at least one H.264 encoder
	return supportsGstElement("vaapih264enc") || supportsGstElement("nvh264enc") || supportsGstElement("x264enc")
}

// supportsEncoder checks if the local ffmpeg binary supports a specific encoder.
func supportsEncoder(name string) bool {
	cmd := exec.Command("ffmpeg", "-encoders")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), name)
}

// verifyPhysicalEncoder executes a 0.1-second dry-run encoding of a dummy video stream in FFmpeg
// to verify that the GPU/hardware capabilities are physically available in the OS.
func verifyPhysicalEncoder(name string, extraArgs []string) bool {
	if !supportsEncoder(name) {
		return false
	}
	// Build a 0.1-second dummy encoding task
	args := []string{"-f", "lavfi", "-i", "testsrc=duration=0.1", "-c:v", name}
	args = append(args, extraArgs...)
	args = append(args, "-f", "null", "-")

	cmd := exec.Command("ffmpeg", args...)
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func getCaptureBackend(isWayland, testMode bool) (CaptureBackend, string) {
	if !isWayland {
		return BackendX11, "X11 (x11grab)"
	}
	if testMode {
		// During automated tests, force Wayland FFmpeg to fully test D-Bus handshakes against our mock portal
		return BackendWaylandFFmpeg, "Wayland (D-Bus ScreenCast Handshake Loop)"
	}
	if supportsGStreamerPipeWire() {
		return BackendWaylandGStreamer, "Wayland via GStreamer (pipewiresrc + H.264 HW/SW encoder)"
	}
	if supportsPipeWireGrab() {
		return BackendWaylandFFmpeg, "Wayland via FFmpeg (pipewiregrab)"
	}
	return BackendX11, "X11 fallback (x11grab)"
}

// selectBestH264Encoder auto-probes the system for physical hardware acceleration capability.
func selectBestH264Encoder(requested string, defaultPreset, defaultTune string, bitrate int) (string, []string) {
	// Respect explicitly passed codecs other than "libx264"
	if requested != "libx264" {
		return requested, nil
	}

	// 1. Check for Intel QSV (Quick Sync Video) - native on Intel Arc V140/Xe GPUs
	qsvArgs := []string{
		"-preset", "veryfast",
		"-forced_idr", "1",
		"-low_delay", "1", // Forces low-latency mode on Intel silicon
		"-async_depth", "1", // Reduces frame buffering pipeline inside the GPU
		"-b:v", fmt.Sprintf("%dk", bitrate),
	}
	if verifyPhysicalEncoder("h264_qsv", qsvArgs) {
		log.Println("[HW Accel] Intel Quick Sync Video (QSV) hardware encoder verified and activated!")
		return "h264_qsv", qsvArgs
	}

	// 2. Check for NVIDIA NVENC
	nvencArgs := []string{
		"-preset", "p1",
		"-tune", "ull",
		"-b:v", fmt.Sprintf("%dk", bitrate),
	}
	if verifyPhysicalEncoder("h264_nvenc", nvencArgs) {
		log.Println("[HW Accel] NVIDIA NVENC hardware encoder verified and activated!")
		return "h264_nvenc", nvencArgs
	}

	// 3. Check for VA-API (Intel/AMD standard hardware acceleration on Linux)
	vaapiArgs := []string{
		"-vaapi_device", "/dev/dri/renderD128",
		"-vf", "format=nv12,hwupload",
		"-b:v", fmt.Sprintf("%dk", bitrate),
	}
	if verifyPhysicalEncoder("h264_vaapi", vaapiArgs) {
		log.Println("[HW Accel] Intel/AMD VA-API hardware encoder verified and activated!")
		return "h264_vaapi", vaapiArgs
	}

	// 4. Fallback to CPU software encoding
	log.Println("[HW Accel] No hardware acceleration codec verified physically. Falling back to optimized CPU encoding.")
	return "libx264", []string{
		"-preset", defaultPreset,
		"-tune", defaultTune,
		"-b:v", fmt.Sprintf("%dk", bitrate),
	}
}

// selectBestGstH264Encoder selects the best available H.264 encoder element for GStreamer.
func selectBestGstH264Encoder(codec, preset, tune string, gop int, bitrate int) (string, []string) {
	// If the user requested a specific GStreamer encoder, respect it
	if codec == "vaapih264enc" || codec == "nvh264enc" || codec == "x264enc" {
		return codec, getGstEncoderArgs(codec, preset, tune, gop, bitrate)
	}

	// 1. Check for VA-API (Intel/AMD hardware acceleration)
	if supportsGstElement("vaapih264enc") {
		log.Println("[GStreamer HW Accel] Intel/AMD VA-API hardware encoder (vaapih264enc) verified and activated!")
		return "vaapih264enc", getGstEncoderArgs("vaapih264enc", preset, tune, gop, bitrate)
	}

	// 2. Check for NVIDIA NVENC
	if supportsGstElement("nvh264enc") {
		log.Println("[GStreamer HW Accel] NVIDIA NVENC hardware encoder (nvh264enc) verified and activated!")
		return "nvh264enc", getGstEncoderArgs("nvh264enc", preset, tune, gop, bitrate)
	}

	// 3. Fallback to CPU software encoding
	log.Println("[GStreamer HW Accel] No hardware acceleration element found in GStreamer. Falling back to software encoding (x264enc).")
	return "x264enc", getGstEncoderArgs("x264enc", preset, tune, gop, bitrate)
}

// getGstEncoderArgs returns low-latency properties optimized for each GStreamer H.264 encoder.
func getGstEncoderArgs(encoder, preset, tune string, gop int, bitrate int) []string {
	switch encoder {
	case "vaapih264enc":
		return []string{
			fmt.Sprintf("keyframe-period=%d", gop),
			"max-bframes=0",
			fmt.Sprintf("bitrate=%d", bitrate),
		}
	case "nvh264enc":
		return []string{
			fmt.Sprintf("gop-size=%d", gop),
			"bframes=0",
			"zerolatency=true",
			fmt.Sprintf("bitrate=%d", bitrate),
		}
	case "x264enc":
		gstPreset := "ultrafast"
		if preset != "" {
			gstPreset = preset
		}
		gstTune := "zerolatency"
		if tune != "" {
			gstTune = tune
		}
		return []string{
			fmt.Sprintf("key-int-max=%d", gop),
			"bframes=0",
			fmt.Sprintf("tune=%s", gstTune),
			fmt.Sprintf("speed-preset=%s", gstPreset),
			fmt.Sprintf("bitrate=%d", bitrate),
		}
	default:
		return nil
	}
}

// getDefaultPulseAudioMonitor finds the name of the default PulseAudio monitor source.
func getDefaultPulseAudioMonitor() string {
	return "default"
}

// appendVideoFilter appends a filter to the existing -vf argument in args, or adds -vf filter if not present.
func appendVideoFilter(args []string, filter string) []string {
	for i, arg := range args {
		if arg == "-vf" && i+1 < len(args) {
			// Copy args to avoid modifying the original slice in place
			newArgs := make([]string, len(args))
			copy(newArgs, args)
			newArgs[i+1] = newArgs[i+1] + "," + filter
			return newArgs
		}
	}
	return append(args, "-vf", filter)
}

// getGstVolumeChain returns a sliced chain of GStreamer volume elements to bypass the 10.0 limit per element.
func getGstVolumeChain(volume float64) []string {
	var args []string
	remaining := volume
	for remaining > 5.0 {
		args = append(args, "!", "volume", "volume=5.0")
		remaining /= 5.0
	}
	if remaining > 1.0 || len(args) == 0 {
		args = append(args, "!", "volume", fmt.Sprintf("volume=%.4f", remaining))
	}
	return args
}

func handlePeer(ctx context.Context, conn *quic.Conn, testMode, headlessMode, debugMode bool, display, size string, fps int, codec string, gop int, preset, tune string, bitrate int, volume float64) {
	defer conn.CloseWithError(0, "connection closed")
	log.Printf("Establishing outbound media stream...")

	log.Printf("Opening outbound QUIC unidirectional stream...")
	stream, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		log.Printf("Failed to open QUIC stream: %v", err)
		return
	}
	defer stream.Close()

	// Set up stats reporting
	reporter := stats.NewStatsReporter(true)
	reporter.StartReporting(2 * time.Second)
	defer reporter.Stop()

	// Wrap stream writer with stats reporter
	proxyWriter := stats.NewProxyWriter(stream, reporter)

	// Determine the best capture engine to use
	isWayland := portal.IsWayland()
	backend, backendName := getCaptureBackend(isWayland, testMode)

	// Headless mode overrides: use GStreamer with synthetic videotestsrc
	if headlessMode {
		backend = BackendWaylandGStreamer
		backendName = "Headless (GStreamer videotestsrc → FFmpeg → QUIC)"
	}

	log.Printf("Selected capture engine: %s", backendName)

	var nodeID uint32
	var pwFD int = -1
	var sess *portal.ScreenCastSession

	// If a Wayland backend is chosen AND we're not headless, trigger the portal screen-sharing selection UI
	if !headlessMode && (backend == BackendWaylandFFmpeg || backend == BackendWaylandGStreamer) {
		log.Println("Initializing D-Bus ScreenCast portal...")
		sess, err = portal.NewScreenCastSession()
		if err != nil {
			log.Printf("Failed to initialize ScreenCast session: %v. Falling back to X11 grab...", err)
			backend = BackendX11
		} else {
			defer sess.Close()
			nodeID, pwFD, err = sess.Handshake(ctx)
			if err != nil {
				log.Printf("ScreenCast portal handshake failed: %v. Falling back to X11 grab...", err)
				backend = BackendX11
			} else {
				log.Printf("ScreenCast portal handshake succeeded! PipeWire Node ID: %d, FD: %d", nodeID, pwFD)
			}
		}
	}

	var cmdFfmpeg *exec.Cmd
	var cmdGstreamer *exec.Cmd
	var extraFiles []*os.File
	var mediaStdout io.ReadCloser
	// OS pipe for GStreamer→FFmpeg data transfer (only used in GStreamer backend)
	var gstPipeR, gstPipeW *os.File

	// Parse size into width and height
	var width, height int
	if _, err := fmt.Sscanf(size, "%dx%d", &width, &height); err != nil {
		width = 1920
		height = 1080
	}

	// Select best encoder (incorporating GPU acceleration)
	hwCodec, extraCodecArgs := selectBestH264Encoder(codec, preset, tune, bitrate)
	if debugMode {
		log.Printf("[Debug] Selected encoder: %s, extra args: %v", hwCodec, extraCodecArgs)
	}

	// Dynamically discover default PulseAudio monitor source name
	audioDevice := getDefaultPulseAudioMonitor()
	log.Printf("[Audio] Dynamically selected PulseAudio monitor source: %s", audioDevice)

	if testMode {
		log.Println("Using synthetic test video source (lavfi testsrc)...")
		ffmpegArgs := []string{
			"-f", "lavfi",
			"-i", fmt.Sprintf("testsrc=size=%s:rate=%d", size, fps),
			"-c:v", codec,
			"-preset", preset,
			"-tune", tune,
			"-g", fmt.Sprintf("%d", gop),
			"-f", "h264",
			"pipe:1",
		}
		cmdFfmpeg = exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	} else if backend == BackendWaylandFFmpeg {
		log.Printf("Adapting streaming engine for Wayland/PipeWire Native (Node ID: %d, FD: %d)...", nodeID, pwFD)
		file := os.NewFile(uintptr(pwFD), "pipewire-fd")
		extraFiles = append(extraFiles, file)

		ffmpegArgs := []string{
			"-fflags", "nobuffer",
			"-thread_queue_size", "1024",
			"-f", "lavfi",
			"-i", fmt.Sprintf("pipewiregrab=fd=3:node=%d", nodeID),
			"-thread_queue_size", "1024",
			"-f", "pulse",
			"-ac", "2",
			"-ar", "48000",
			"-i", audioDevice,
			"-map", "0:v",
			"-map", "1:a",
			"-c:v", codec,
		}
		videoArgs := appendVideoFilter(extraCodecArgs, "setpts=PTS-STARTPTS")
		ffmpegArgs = append(ffmpegArgs, videoArgs...)
		ffmpegArgs = append(ffmpegArgs,
			"-c:a", "aac",
			"-b:a", "128k",
			"-ac", "2",
			"-ar", "48000",
			"-af", fmt.Sprintf("volume=%.1f,asetpts=PTS-STARTPTS,aresample=async=1", volume),
			"-g", fmt.Sprintf("%d", gop),
			"-muxdelay", "0",
			"-max_interleave_delta", "100",
			"-fps_mode", "cfr",
			"-r", fmt.Sprintf("%d", fps),
			"-mpegts_flags", "initial_discontinuity+resend_headers",
			"-f", "mpegts",
			"pipe:1",
		)
		cmdFfmpeg = exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
		cmdFfmpeg.ExtraFiles = extraFiles
	} else if backend == BackendWaylandGStreamer {
		log.Printf("Building Wayland/GStreamer pipeline...")

		// Select the best native GStreamer encoder
		gstEncoder, gstEncoderArgs := selectBestGstH264Encoder(codec, preset, tune, gop, bitrate)
		log.Printf("Selected GStreamer H.264 Encoder: %s, with args: %v", gstEncoder, gstEncoderArgs)

		// Define explicit format caps to enforce hardware negotiation success
		formatCaps := "video/x-raw,format=NV12"
		if gstEncoder == "x264enc" {
			formatCaps = "video/x-raw,format=I420"
		}

		// Build GStreamer command
		gstVerbosity := "-q"
		if debugMode {
			gstVerbosity = "-v"
		}

		var videoArgs []string
		if headlessMode {
			log.Printf("[Headless] Using synthetic videotestsrc (%dx%d @ %d fps)...", width, height, fps)
			videoArgs = []string{
				"videotestsrc", "is-live=true",
				"!", fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1", width, height, fps),
				"!", "videoconvert",
				"!", formatCaps,
				"!", gstEncoder,
			}
			videoArgs = append(videoArgs, gstEncoderArgs...)
			videoArgs = append(videoArgs,
				"!", "h264parse", "config-interval=-1",
				"!", "video/x-h264,stream-format=byte-stream,alignment=au",
				"!", "queue", "max-size-buffers=3", "leaky=downstream",
				"!", "mux.",
			)
		} else {
			log.Printf("PipeWire capture: Node ID=%d, FD=%d", nodeID, pwFD)
			// Pass the portal's PipeWire FD to GStreamer via ExtraFiles.
			// ExtraFiles[0] becomes fd=3 in the child process.
			pwFile := os.NewFile(uintptr(pwFD), "pipewire-fd")
			extraFiles = append(extraFiles, pwFile)

			videoArgs = []string{
				"pipewiresrc", fmt.Sprintf("path=%d", nodeID), "fd=3", "do-timestamp=true", "keepalive-time=100",
				"!", "queue", "max-size-buffers=3", "leaky=downstream",
				"!", "videoconvert",
				"!", formatCaps,
				"!", "videorate",
				"!", fmt.Sprintf("video/x-raw,framerate=%d/1", fps),
				"!", gstEncoder,
			}
			videoArgs = append(videoArgs, gstEncoderArgs...)
			videoArgs = append(videoArgs,
				"!", "h264parse", "config-interval=-1",
				"!", "video/x-h264,stream-format=byte-stream,alignment=au",
				"!", "queue", "max-size-buffers=3", "leaky=downstream",
				"!", "mux.",
			)
		}

		var audioArgs []string
		if headlessMode {
			audioArgs = []string{
				"audiotestsrc", "is-live=true", "samplesperbuffer=1024",
			}
			audioArgs = append(audioArgs, getGstVolumeChain(volume)...)
			audioArgs = append(audioArgs,
				"!", "audioconvert",
				"!", "opusenc", "bitrate=96000",
				"!", "queue", "max-size-buffers=10", "leaky=downstream",
				"!", "mux.",
			)
		} else {
			audioArgs = []string{
				"pulsesrc", fmt.Sprintf("device=%s", audioDevice),
				"!", "queue", "max-size-buffers=10", "leaky=downstream",
				"!", "audio/x-raw,rate=48000,channels=2",
			}
			audioArgs = append(audioArgs, getGstVolumeChain(volume)...)
			audioArgs = append(audioArgs,
				"!", "audioconvert",
				"!", "opusenc", "bitrate=96000",
				"!", "queue", "max-size-buffers=10", "leaky=downstream",
				"!", "mux.",
			)
		}

		gstArgs := []string{gstVerbosity}
		gstArgs = append(gstArgs, videoArgs...)
		gstArgs = append(gstArgs, audioArgs...)
		gstArgs = append(gstArgs,
			"mpegtsmux", "name=mux",
			"!", "fdsink", "fd=1", "sync=false",
		)

		if debugMode {
			log.Printf("[Debug] GStreamer command: gst-launch-1.0 %s", strings.Join(gstArgs, " "))
		}

		cmdGstreamer = exec.CommandContext(ctx, "gst-launch-1.0", gstArgs...)
		if len(extraFiles) > 0 {
			cmdGstreamer.ExtraFiles = extraFiles
		}
		cmdGstreamer.Stderr = os.Stderr
	} else {
		log.Printf("Using standard X11 capture on display %s...", display)
		ffmpegArgs := []string{
			"-fflags", "nobuffer",
			"-thread_queue_size", "1024",
			"-f", "x11grab",
			"-video_size", size,
			"-framerate", fmt.Sprintf("%d", fps),
			"-i", display, // Input 1: Video from X11
			"-thread_queue_size", "1024",
			"-f", "pulse",
			"-ac", "2",
			"-ar", "48000",
			"-i", audioDevice,
		}

		ffmpegArgs = append(ffmpegArgs,
			"-map", "0:v",
			"-map", "1:a",
		)

		ffmpegArgs = append(ffmpegArgs, "-c:v", hwCodec)
		videoArgs := appendVideoFilter(extraCodecArgs, "setpts=PTS-STARTPTS")
		ffmpegArgs = append(ffmpegArgs, videoArgs...)
		ffmpegArgs = append(ffmpegArgs, "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000", "-af", fmt.Sprintf("volume=%.1f,asetpts=PTS-STARTPTS,aresample=async=1", volume))
		ffmpegArgs = append(ffmpegArgs,
			"-g", fmt.Sprintf("%d", gop),
			"-muxdelay", "0",
			"-max_interleave_delta", "100",
			"-fps_mode", "cfr",
			"-r", fmt.Sprintf("%d", fps),
			"-mpegts_flags", "initial_discontinuity+resend_headers",
			"-f", "mpegts",
			"pipe:1",
		)
		cmdFfmpeg = exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	}

	// Create pipe for stdout
	var errOut error
	if cmdFfmpeg != nil {
		mediaStdout, errOut = cmdFfmpeg.StdoutPipe()
		cmdFfmpeg.Stderr = os.Stderr
	} else {
		mediaStdout, errOut = cmdGstreamer.StdoutPipe()
	}
	if errOut != nil {
		log.Printf("Failed to create stdout pipe: %v", errOut)
		if gstPipeR != nil {
			gstPipeR.Close()
		}
		if gstPipeW != nil {
			gstPipeW.Close()
		}
		return
	}

	// ── Start capture processes ──
	if cmdGstreamer != nil {
		if err := cmdGstreamer.Start(); err != nil {
			log.Printf("Failed to start GStreamer capture: %v", err)
			if gstPipeR != nil {
				gstPipeR.Close()
			}
			if gstPipeW != nil {
				gstPipeW.Close()
			}
			return
		}
		log.Printf("[Pipeline] GStreamer started (PID: %d)", cmdGstreamer.Process.Pid)
	}
	if cmdFfmpeg != nil {
		if err := cmdFfmpeg.Start(); err != nil {
			log.Printf("Failed to start FFmpeg encoder: %v", err)
			if gstPipeR != nil {
				gstPipeR.Close()
			}
			if gstPipeW != nil {
				gstPipeW.Close()
			}
			return
		}
		log.Printf("[Pipeline] FFmpeg started (PID: %d)", cmdFfmpeg.Process.Pid)
	}

	// ── CRITICAL: Close parent's copies of the GStreamer→FFmpeg pipe ──
	// The child processes have inherited their own FD copies via dup2.
	// If we don't close these in the parent:
	//   - gstPipeW: FFmpeg would never see EOF when GStreamer exits (parent holds a ref)
	//   - gstPipeR: The read end leaks in the parent (minor but wasteful)
	if gstPipeW != nil {
		gstPipeW.Close()
		gstPipeW = nil // Mark as closed so deferred cleanup doesn't double-close
	}
	if gstPipeR != nil {
		gstPipeR.Close()
		gstPipeR = nil
	}

	// ── Monitor GStreamer exit asynchronously (for diagnostics) ──
	var gstWaitOnce sync.Once
	var gstExitErr error
	gstDone := make(chan struct{})
	if cmdGstreamer != nil {
		go func() {
			gstExitErr = cmdGstreamer.Wait()
			gstWaitOnce.Do(func() {}) // Mark that Wait has been called
			close(gstDone)
			if gstExitErr != nil {
				log.Printf("[Pipeline] GStreamer exited with error: %v", gstExitErr)
			} else {
				log.Printf("[Pipeline] GStreamer exited cleanly")
			}
		}()
	} else {
		close(gstDone) // No GStreamer to wait for
	}

	// Ensure processes are gracefully terminated upon exit
	defer func() {
		log.Println("Stopping capture processes...")
		if gstPipeW != nil {
			_ = gstPipeW.Close()
		}
		if gstPipeR != nil {
			_ = gstPipeR.Close()
		}
		if cmdFfmpeg != nil && cmdFfmpeg.Process != nil {
			_ = cmdFfmpeg.Process.Signal(syscall.SIGTERM)
			_ = cmdFfmpeg.Wait()
		}
		if cmdGstreamer != nil && cmdGstreamer.Process != nil {
			// Only call Wait if the monitoring goroutine hasn't already
			select {
			case <-gstDone:
				// Already waited
			default:
				_ = cmdGstreamer.Process.Signal(syscall.SIGTERM)
				<-gstDone // Wait for the monitoring goroutine
			}
		}
		for _, f := range extraFiles {
			_ = f.Close()
		}
	}()

	log.Printf("Streaming media stream to peer over reliable QUIC stream...")
	_, err = io.Copy(proxyWriter, mediaStdout)
	if err != nil && err != io.EOF {
		log.Printf("Streaming finished with error: %v", err)
	} else {
		log.Printf("Streaming finished successfully.")
	}
}

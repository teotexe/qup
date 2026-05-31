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
	// Parse CLI flags
	portFlag := flag.Int("port", 50001, "Port to listen on")
	displayFlag := flag.String("display", "", "X11 display to grab (e.g. :0.0). Defaults to $DISPLAY env var.")
	sizeFlag := flag.String("size", "1920x1080", "Video capture dimensions (width x height)")
	fpsFlag := flag.Int("fps", 60, "Frame rate for capturing")
	presetFlag := flag.String("preset", "ultrafast", "x264 encoder preset (e.g., ultrafast, superfast, veryfast, medium)")
	tuneFlag := flag.String("tune", "zerolatency", "x264 encoder tune option")
	gopFlag := flag.Int("g", 60, "GOP (keyframe interval) size. Default is 60 for low-bandwidth 60 FPS streaming.")
	codecFlag := flag.String("codec", "libx264", "H.264 encoder library. Auto-probes hardware acceleration (QSV, NVENC, VA-API) by default.")
	testFlag := flag.Bool("test", false, "Use a synthetic test video source (lavfi testsrc) instead of X11 capture")
	mockPortalFlag := flag.Bool("mock-portal", false, "Start a mock D-Bus ScreenCast portal in the background (for testing)")
	debugFlag := flag.Bool("debug", false, "Enable verbose diagnostic logging for pipeline debugging")
	headlessFlag := flag.Bool("headless", false, "Use synthetic GStreamer test source (no screen capture, no portal popup). Tests the full GStreamer→FFmpeg→QUIC pipeline.")
	flag.Parse()

	display := *displayFlag
	if display == "" {
		display = os.Getenv("DISPLAY")
		if display == "" {
			display = ":0.0"
		}
	}

	log.Printf("Starting AuraShare Host (Bob) on port %d...", *portFlag)
	log.Printf("Capture config: TestMode=%v, Headless=%v, Debug=%v, Display=%s, Size=%s, FPS=%d, Codec=%s, GOP=%d, Preset=%s, Tune=%s",
		*testFlag, *headlessFlag, *debugFlag, display, *sizeFlag, *fpsFlag, *codecFlag, *gopFlag, *presetFlag, *tuneFlag)

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

	// Create listener
	listener, err := quic.ListenAddr(fmt.Sprintf(":%d", *portFlag), tlsConfig, nil)
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
		go handlePeer(ctx, conn, *testFlag, *headlessFlag, *debugFlag, display, *sizeFlag, *fpsFlag, *codecFlag, *gopFlag, *presetFlag, *tuneFlag)
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

// supportsGStreamerPipeWire checks if GStreamer and the pipewiresrc and y4menc plugins are available.
func supportsGStreamerPipeWire() bool {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		return false
	}
	cmd := exec.Command("gst-inspect-1.0", "pipewiresrc")
	if err := cmd.Run(); err != nil {
		return false
	}
	cmd2 := exec.Command("gst-inspect-1.0", "y4menc")
	if err := cmd2.Run(); err != nil {
		return false
	}
	return true
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
	if supportsPipeWireGrab() {
		return BackendWaylandFFmpeg, "Wayland via FFmpeg (pipewiregrab)"
	}
	if supportsGStreamerPipeWire() {
		return BackendWaylandGStreamer, "Wayland via GStreamer (pipewiresrc + y4menc)"
	}
	return BackendX11, "X11 fallback (x11grab)"
}

// selectBestH264Encoder auto-probes the system for physical hardware acceleration capability.
func selectBestH264Encoder(requested string, defaultPreset, defaultTune string) (string, []string) {
	// Respect explicitly passed codecs other than "libx264"
	if requested != "libx264" {
		return requested, nil
	}

	// 1. Check for Intel QSV (Quick Sync Video) - native on Intel Arc V140/Xe GPUs
	qsvArgs := []string{"-preset", "veryfast", "-forced_idr", "1"}
	if verifyPhysicalEncoder("h264_qsv", qsvArgs) {
		log.Println("[HW Accel] Intel Quick Sync Video (QSV) hardware encoder verified and activated!")
		return "h264_qsv", qsvArgs
	}

	// 2. Check for NVIDIA NVENC
	nvencArgs := []string{"-preset", "p1", "-tune", "ull"}
	if verifyPhysicalEncoder("h264_nvenc", nvencArgs) {
		log.Println("[HW Accel] NVIDIA NVENC hardware encoder verified and activated!")
		return "h264_nvenc", nvencArgs
	}

	// 3. Check for VA-API (Intel/AMD standard hardware acceleration on Linux)
	vaapiArgs := []string{"-vaapi_device", "/dev/dri/renderD128", "-vf", "format=nv12,hwupload"}
	if verifyPhysicalEncoder("h264_vaapi", vaapiArgs) {
		log.Println("[HW Accel] Intel/AMD VA-API hardware encoder verified and activated!")
		return "h264_vaapi", vaapiArgs
	}

	// 4. Fallback to CPU software encoding
	log.Println("[HW Accel] No hardware acceleration codec verified physically. Falling back to optimized CPU encoding.")
	return "libx264", []string{
		"-preset", defaultPreset,
		"-tune", defaultTune,
	}
}

func handlePeer(ctx context.Context, conn *quic.Conn, testMode, headlessMode, debugMode bool, display, size string, fps int, codec string, gop int, preset, tune string) {
	defer conn.CloseWithError(0, "connection closed")
	log.Printf("Establishing outbound media stream...")

	// Create a new unidirectional stream to send video
	stream, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		log.Printf("Failed to open QUIC stream: %v", err)
		return
	}
	defer stream.Close()

	log.Printf("Opened unidirectional QUIC stream. Spawning media capture processes...")

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
	var ffmpegStdout io.ReadCloser
	// OS pipe for GStreamer→FFmpeg data transfer (only used in GStreamer backend)
	var gstPipeR, gstPipeW *os.File

	// Parse size into width and height
	var width, height int
	if _, err := fmt.Sscanf(size, "%dx%d", &width, &height); err != nil {
		width = 1920
		height = 1080
	}

	// Select best encoder (incorporating GPU acceleration)
	hwCodec, extraCodecArgs := selectBestH264Encoder(codec, preset, tune)
	if debugMode {
		log.Printf("[Debug] Selected encoder: %s, extra args: %v", hwCodec, extraCodecArgs)
	}

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
			"-f", "lavfi",
			"-i", fmt.Sprintf("pipewiregrab=fd=3:node=%d", nodeID),
			"-c:v", codec,
			"-preset", preset,
			"-tune", tune,
			"-g", fmt.Sprintf("%d", gop),
			"-f", "h264",
			"pipe:1",
		}
		cmdFfmpeg = exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
		cmdFfmpeg.ExtraFiles = extraFiles
	} else if backend == BackendWaylandGStreamer {
		log.Printf("Building Wayland/GStreamer pipeline...")

		// ── Create OS pipe for GStreamer→FFmpeg data transfer ──
		// CRITICAL: We use os.Pipe() instead of io.Pipe() because:
		// - With io.Pipe, Go's exec creates internal goroutines to bridge data.
		// - When GStreamer exits, those goroutines finish but do NOT close the
		//   io.PipeWriter. So FFmpeg never sees EOF on stdin → deadlock.
		// - With os.Pipe, the child processes directly inherit the pipe FDs.
		//   When GStreamer exits, its FD closes, FFmpeg sees EOF. No deadlock.
		var pipeErr error
		gstPipeR, gstPipeW, pipeErr = os.Pipe()
		if pipeErr != nil {
			log.Printf("Failed to create GStreamer→FFmpeg pipe: %v", pipeErr)
			return
		}

		// Build GStreamer command
		var gstArgs []string
		if headlessMode {
			log.Printf("[Headless] Using synthetic videotestsrc (%dx%d @ %d fps)...", width, height, fps)
			gstArgs = []string{
				"videotestsrc", "is-live=true",
				"!", fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1", width, height, fps),
				"!", "y4menc",
				"!", "fdsink", "fd=1", "sync=false",
			}
		} else {
			log.Printf("PipeWire capture: Node ID=%d, FD=%d", nodeID, pwFD)
			// Pass the portal's PipeWire FD to GStreamer via ExtraFiles.
			// ExtraFiles[0] becomes fd=3 in the child process.
			pwFile := os.NewFile(uintptr(pwFD), "pipewire-fd")
			extraFiles = append(extraFiles, pwFile)

			gstVerbosity := "-q"
			if debugMode {
				gstVerbosity = "-v"
			}
			gstArgs = []string{
				gstVerbosity,
				"pipewiresrc", fmt.Sprintf("path=%d", nodeID), "fd=3",
				"!", "queue", "max-size-buffers=3", "leaky=downstream",
				"!", "videoconvert",
				"!", "video/x-raw,format=I420",
				"!", "y4menc",
				"!", "fdsink", "fd=1", "sync=false",
			}
		}

		if debugMode {
			log.Printf("[Debug] GStreamer command: gst-launch-1.0 %s", strings.Join(gstArgs, " "))
		}

		cmdGstreamer = exec.CommandContext(ctx, "gst-launch-1.0", gstArgs...)
		cmdGstreamer.Stdout = gstPipeW // Direct OS FD - child inherits via dup2, no Go goroutine
		if len(extraFiles) > 0 {
			cmdGstreamer.ExtraFiles = extraFiles
		}
		cmdGstreamer.Stderr = os.Stderr

		// Build FFmpeg encoding command
		ffmpegArgs := []string{
			"-f", "yuv4mpegpipe",
			"-i", "pipe:0",
		}
		// If custom dimensions are explicitly passed (non-default size), scale using FFmpeg's fast swscale
		if size != "1920x1080" && !headlessMode {
			ffmpegArgs = append(ffmpegArgs, "-vf", fmt.Sprintf("scale=%d:%d", width, height))
		}
		// Add auto-selected hardware acceleration encoding arguments
		ffmpegArgs = append(ffmpegArgs, "-c:v", hwCodec)
		ffmpegArgs = append(ffmpegArgs, extraCodecArgs...)
		ffmpegArgs = append(ffmpegArgs,
			"-g", fmt.Sprintf("%d", gop),
			"-f", "h264",
			"pipe:1",
		)

		if debugMode {
			log.Printf("[Debug] FFmpeg command: ffmpeg %s", strings.Join(ffmpegArgs, " "))
		}

		cmdFfmpeg = exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
		cmdFfmpeg.Stdin = gstPipeR // Direct OS FD - child inherits via dup2, no Go goroutine
	} else {
		log.Printf("Using standard X11 capture on display %s...", display)
		ffmpegArgs := []string{
			"-f", "x11grab",
			"-video_size", size,
			"-framerate", fmt.Sprintf("%d", fps),
			"-i", display,
		}

		// Add auto-selected hardware acceleration encoding arguments
		ffmpegArgs = append(ffmpegArgs, "-c:v", hwCodec)
		ffmpegArgs = append(ffmpegArgs, extraCodecArgs...)
		ffmpegArgs = append(ffmpegArgs,
			"-g", fmt.Sprintf("%d", gop),
			"-f", "h264",
			"pipe:1",
		)
		cmdFfmpeg = exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	}

	// Create pipe for ffmpeg stdout
	var errOut error
	ffmpegStdout, errOut = cmdFfmpeg.StdoutPipe()
	if errOut != nil {
		log.Printf("Failed to create ffmpeg stdout pipe: %v", errOut)
		if gstPipeR != nil {
			gstPipeR.Close()
		}
		if gstPipeW != nil {
			gstPipeW.Close()
		}
		return
	}
	cmdFfmpeg.Stderr = os.Stderr

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

	log.Printf("Streaming H.264 desktop stream to peer...")
	_, err = io.Copy(proxyWriter, ffmpegStdout)
	if err != nil && err != io.EOF {
		log.Printf("Streaming finished with error: %v", err)
	} else {
		log.Printf("Streaming finished successfully.")
	}
}

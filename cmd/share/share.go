package share

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

	"qup/internal/crypto"
	"qup/internal/stats"

	"github.com/quic-go/quic-go"
	portal "github.com/teotexe/wayland-portal-go"
	"github.com/teotexe/wayland-portal-go/pwrouter"
)

type CaptureBackend int

const (
	BackendX11GStreamer CaptureBackend = iota
	BackendWaylandGStreamer
)

func Run(args []string) {
	os.Setenv("PULSE_LATENCY_MSEC", "30")

	// Create custom flag set to avoid pollution
	fs := flag.NewFlagSet("share", flag.ExitOnError)

	// Parse CLI flags
	portFlag := fs.Int("port", 22050, "Port to listen on")
	displayFlag := fs.String("display", "", "X11 display to grab (e.g. :0.0). Defaults to $DISPLAY env var.")
	sizeFlag := fs.String("size", "1920x1080", "Video capture dimensions (width x height)")
	fpsFlag := fs.Int("fps", 60, "Frame rate for capturing")
	presetFlag := fs.String("preset", "ultrafast", "x264 encoder preset (e.g., ultrafast, superfast, veryfast, medium)")
	tuneFlag := fs.String("tune", "zerolatency", "x264 encoder tune option")
	gopFlag := fs.Int("g", 30, "GOP (keyframe interval) size. Default is 30 for low-latency fast startup connection.")
	codecFlag := fs.String("codec", "auto", "GStreamer H.264 encoder element (e.g., x264enc, vah264enc, nvh264enc). Defaults to 'auto' for hardware autoprobing.")
	bitrateFlag := fs.Int("bitrate", 8000, "Target video bitrate in kbps (e.g., 8000 for 8 Mbps high-quality 1080p)")
	testFlag := fs.Bool("test", false, "Use a synthetic test video source (lavfi testsrc) instead of X11 capture")
	mockPortalFlag := fs.Bool("mock-portal", false, "Start a mock D-Bus ScreenCast portal in the background (for testing)")
	debugFlag := fs.Bool("debug", false, "Enable verbose diagnostic logging for pipeline debugging")
	headlessFlag := fs.Bool("headless", false, "Use synthetic GStreamer test source (no screen capture, no portal popup).")
	volumeFlag := fs.Float64("volume", 5.0, "Audio volume amplification factor (e.g. 150.0 for 150x volume boost)")
	audioAppFlag := fs.String("audio-app", "", "Name of the app to capture audio from (e.g., 'Firefox'). If empty, falls back to system audio.")
	fs.Parse(args)

	audioApp := *audioAppFlag
	if audioApp == "" && !*testFlag && !*headlessFlag {
		if isTerminal(os.Stdin) {
			audioApp = promptForAudioApp()
		} else {
			log.Println("[AudioApp] Non-interactive environment detected or -audio-app flag omitted. Defaulting to system audio.")
		}
	}

	display := *displayFlag
	if display == "" {
		display = os.Getenv("DISPLAY")
		if display == "" {
			display = ":0.0"
		}
	}

	log.Printf("Starting AuraShare Host (Bob) on port %d...", *portFlag)
	log.Printf("Capture config: TestMode=%v, Headless=%v, Debug=%v, Display=%s, Size=%s, FPS=%d, Codec=%s, GOP=%d, Preset=%s, Tune=%s, Bitrate=%d, Volume=%.1f, AudioApp=%s",
		*testFlag, *headlessFlag, *debugFlag, display, *sizeFlag, *fpsFlag, *codecFlag, *gopFlag, *presetFlag, *tuneFlag, *bitrateFlag, *volumeFlag, audioApp)

	// Create main context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start Mock D-Bus Portal if requested or configured
	if *mockPortalFlag || os.Getenv("AURASHARE_MOCK_PORTAL") == "1" {
		log.Println("Starting Mock D-Bus ScreenCast Portal...")
		_, err := portal.StartMockPortal(ctx)
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
		go handlePeer(ctx, conn, *testFlag, *headlessFlag, *debugFlag, display, *sizeFlag, *fpsFlag, *codecFlag, *gopFlag, *presetFlag, *tuneFlag, *bitrateFlag, *volumeFlag, audioApp)
	}
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
	return supportsGstElement("vah264enc") || supportsGstElement("nvh264enc") || supportsGstElement("x264enc")
}

func getCaptureBackend(isWayland, testMode bool) (CaptureBackend, string) {
	if !isWayland {
		return BackendX11GStreamer, "X11 via GStreamer (ximagesrc)"
	}
	return BackendWaylandGStreamer, "Wayland via GStreamer (pipewiresrc)"
}

// selectBestH264Encoder selects the best available H.264 encoder element for GStreamer.
func selectBestH264Encoder(codec, preset, tune string, gop int, bitrate int) (string, []string) {
	if codec != "auto" {
		if supportsGstElement(codec) {
			log.Printf("[Codec] Respecting user-specified GStreamer encoder element: '%s'", codec)
			return codec, getGstEncoderArgs(codec, preset, tune, gop, bitrate)
		}
		log.Printf("[Codec] Warning: Requested GStreamer encoder element '%s' is not supported on this system. Falling back to autoprobing...", codec)
	}

	// 1. Check for VA-API (Intel/AMD hardware acceleration)
	if supportsGstElement("vah264enc") {
		log.Println("[GStreamer HW Accel] Intel/AMD VA-API hardware encoder (vah264enc) verified and activated!")
		return "vah264enc", getGstEncoderArgs("vah264enc", preset, tune, gop, bitrate)
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
	case "vah264enc":
		return []string{
			fmt.Sprintf("key-int-max=%d", gop),
			"b-frames=0",
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
	case "qsvh264enc", "msdkh264enc":
		return []string{
			fmt.Sprintf("gop-size=%d", gop),
			"bframes=0",
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

func handlePeer(ctx context.Context, conn *quic.Conn, testMode, headlessMode, debugMode bool, display, size string, fps int, codec string, gop int, preset, tune string, bitrate int, volume float64, audioApp string) {
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
		backendName = "Headless (GStreamer videotestsrc → QUIC)"
	}

	log.Printf("Selected capture engine: %s", backendName)

	var nodeID uint32
	var audioNodeID uint32
	var pwFD int = -1
	var sess *portal.ScreenCastSession

	// If a Wayland backend is chosen AND we're not headless, trigger the portal screen-sharing selection UI
	if !headlessMode && backend == BackendWaylandGStreamer {
		log.Println("Initializing D-Bus ScreenCast portal...")
		sess, err = portal.NewScreenCastSession("aurashare")
		if err != nil {
			log.Printf("Failed to initialize ScreenCast session: %v. Falling back to X11 grab...", err)
			backend = BackendX11GStreamer
		} else {
			defer sess.Close()
			opts := portal.ScreenCastOptions{
				SourceTypes: portal.SourceMonitor | portal.SourceWindow,
				CursorMode:  portal.CursorEmbedded,
				Audio:       true,
			}
			info, err := sess.Handshake(ctx, opts)
			if err != nil {
				if errors.Is(err, portal.ErrUserCancelled) {
					log.Println("User cancelled the screen share prompt. Exiting.")
					os.Exit(0)
				}
				log.Printf("ScreenCast portal handshake failed: %v. Falling back to X11 grab...", err)
				backend = BackendX11GStreamer
			} else {
				nodeID = info.VideoNodeID
				audioNodeID = info.AudioNodeID
				pwFD = info.PipeWireFD
				log.Printf("ScreenCast portal handshake succeeded! PipeWire Node ID: %d, Audio Node ID: %d, FD: %d", nodeID, audioNodeID, pwFD)
			}
		}
	}

	var cmdGstreamer *exec.Cmd
	var extraFiles []*os.File
	var mediaStdout io.ReadCloser

	// Parse size into width and height
	var width, height int
	if _, err := fmt.Sscanf(size, "%dx%d", &width, &height); err != nil {
		width = 1920
		height = 1080
	}

	// Dynamically discover default PulseAudio monitor source name
	audioDevice := getDefaultPulseAudioMonitor()
	log.Printf("[Audio] Dynamically selected PulseAudio monitor source: %s", audioDevice)

	var router *pwrouter.Router
	if audioApp != "" {
		var errRouter error
		router, errRouter = pwrouter.NewRouter("AuraShareAudioSink")
		if errRouter != nil {
			log.Fatalf("Failed to initialize audio router: %v", errRouter)
		}
		defer router.Close()

		// Run the automatic monitoring loop in the background
		go router.WatchAndLink(ctx, audioApp)
	}

	log.Printf("Building GStreamer pipeline...")

	// Select the best native GStreamer encoder
	gstEncoder, gstEncoderArgs := selectBestH264Encoder(codec, preset, tune, gop, bitrate)
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
	if headlessMode || testMode {
		log.Printf("[Synthetic] Using synthetic videotestsrc (%dx%d @ %d fps)...", width, height, fps)
		videoArgs = []string{
			"videotestsrc", "is-live=true",
			"!", fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1", width, height, fps),
			"!", "videoconvert", "n-threads=4",
			"!", formatCaps,
			"!", gstEncoder,
		}
		videoArgs = append(videoArgs, gstEncoderArgs...)
		videoArgs = append(videoArgs,
			"!", "h264parse", "config-interval=-1",
			"!", "video/x-h264,stream-format=byte-stream,alignment=au",
			"!", "queue",
			"!", "mux.",
		)
	} else if backend == BackendWaylandGStreamer {
		log.Printf("PipeWire capture: Node ID=%d, FD=%d", nodeID, pwFD)
		// Pass the portal's PipeWire FD to GStreamer via ExtraFiles.
		// ExtraFiles[0] becomes fd=3 in the child process.
		pwFile := os.NewFile(uintptr(pwFD), "pipewire-fd")
		extraFiles = append(extraFiles, pwFile)

		var convElements []string
		if gstEncoder == "vah264enc" && supportsGstElement("vapostproc") {
			convElements = []string{"!", "vapostproc"}
		} else {
			convElements = []string{
				"!", "videoconvert", "n-threads=4",
				"!", "videoscale", "n-threads=4",
			}
		}

		videoArgs = []string{
			"pipewiresrc", fmt.Sprintf("path=%d", nodeID), "fd=3", "do-timestamp=true", "keepalive-time=100",
			"!", "queue", "max-size-buffers=3", "leaky=downstream",
		}
		videoArgs = append(videoArgs, convElements...)
		videoArgs = append(videoArgs,
			"!", fmt.Sprintf("%s,width=%d,height=%d", formatCaps, width, height),
			"!", "videorate",
			"!", fmt.Sprintf("video/x-raw,framerate=%d/1", fps),
			"!", gstEncoder,
		)
		videoArgs = append(videoArgs, gstEncoderArgs...)
		videoArgs = append(videoArgs,
			"!", "h264parse", "config-interval=-1",
			"!", "video/x-h264,stream-format=byte-stream,alignment=au",
			"!", "queue",
			"!", "mux.",
		)
	} else {
		// BackendX11GStreamer
		log.Printf("X11 capture: Display=%s", display)

		var convElements []string
		if gstEncoder == "vah264enc" && supportsGstElement("vapostproc") {
			convElements = []string{"!", "vapostproc"}
		} else {
			convElements = []string{
				"!", "videoconvert", "n-threads=4",
				"!", "videoscale", "n-threads=4",
			}
		}

		videoArgs = []string{
			"ximagesrc", fmt.Sprintf("display-name=%s", display), "show-pointer=true", "use-damage=false",
			"!", "queue", "max-size-buffers=3", "leaky=downstream",
		}
		videoArgs = append(videoArgs, convElements...)
		videoArgs = append(videoArgs,
			"!", fmt.Sprintf("%s,width=%d,height=%d", formatCaps, width, height),
			"!", "videorate",
			"!", fmt.Sprintf("video/x-raw,framerate=%d/1", fps),
			"!", gstEncoder,
		)
		videoArgs = append(videoArgs, gstEncoderArgs...)
		videoArgs = append(videoArgs,
			"!", "h264parse", "config-interval=-1",
			"!", "video/x-h264,stream-format=byte-stream,alignment=au",
			"!", "queue",
			"!", "mux.",
		)
	}

	var audioArgs []string
	if headlessMode || testMode {
		audioArgs = []string{
			"audiotestsrc", "is-live=true", "samplesperbuffer=1024",
		}
		audioArgs = append(audioArgs, getGstVolumeChain(volume)...)
		audioArgs = append(audioArgs,
			"!", "audioconvert",
			"!", "opusenc", "bitrate=96000",
			"!", "queue",
			"!", "mux.",
		)
	} else {
		if audioApp != "" && router != nil {
			audioArgs = []string{
				"pulsesrc", "device=AuraShareAudioSink.monitor",
				"!", "queue",
				"!", "audio/x-raw,rate=48000,channels=2",
			}
		} else if audioNodeID > 0 {
			audioArgs = []string{
				"pipewiresrc", fmt.Sprintf("path=%d", audioNodeID), "fd=3", "do-timestamp=true", "keepalive-time=100",
				"!", "queue",
				"!", "audio/x-raw,rate=48000,channels=2",
			}
		} else {
			audioArgs = []string{
				"pulsesrc", fmt.Sprintf("device=%s", audioDevice),
				"!", "queue",
				"!", "audio/x-raw,rate=48000,channels=2",
			}
		}
		audioArgs = append(audioArgs, getGstVolumeChain(volume)...)
		audioArgs = append(audioArgs,
			"!", "audioconvert",
			"!", "opusenc", "bitrate=96000",
			"!", "queue",
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

	// Create pipe for stdout
	var errOut error
	mediaStdout, errOut = cmdGstreamer.StdoutPipe()
	if errOut != nil {
		log.Printf("Failed to create stdout pipe: %v", errOut)
		return
	}

	// ── Start capture process ──
	if err := cmdGstreamer.Start(); err != nil {
		log.Printf("Failed to start GStreamer capture: %v", err)
		return
	}
	log.Printf("[Pipeline] GStreamer started (PID: %d)", cmdGstreamer.Process.Pid)

	// ── Monitor GStreamer exit asynchronously (for diagnostics) ──
	var gstWaitOnce sync.Once
	var gstExitErr error
	gstDone := make(chan struct{})
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

	// Ensure process is gracefully terminated upon exit
	defer func() {
		log.Println("Stopping capture process...")
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

func isTerminal(f *os.File) bool {
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

func getActiveAudioApps() ([]string, error) {
	cmd := exec.Command("pw-dump")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run pw-dump: %w", err)
	}

	var objects []struct {
		ID   uint32 `json:"id"`
		Type string `json:"type"`
		Info *struct {
			Props map[string]interface{} `json:"props"`
		} `json:"info"`
	}

	if err := json.Unmarshal(output, &objects); err != nil {
		return nil, fmt.Errorf("failed to parse pw-dump JSON: %w", err)
	}

	uniqueApps := make(map[string]bool)
	var apps []string

	for _, obj := range objects {
		if obj.Type != "PipeWire:Interface:Node" {
			continue
		}
		if obj.Info == nil || obj.Info.Props == nil {
			continue
		}
		props := obj.Info.Props

		// Verify media class is "Stream/Output/Audio"
		mediaClassVal, ok := props["media.class"]
		if !ok {
			continue
		}
		mediaClass, ok := mediaClassVal.(string)
		if !ok || mediaClass != "Stream/Output/Audio" {
			continue
		}

		var appName string
		if nameVal, ok := props["application.name"]; ok {
			if nameStr, ok := nameVal.(string); ok && nameStr != "" {
				appName = nameStr
			}
		}
		if appName == "" {
			if binaryVal, ok := props["application.process.binary"]; ok {
				if binaryStr, ok := binaryVal.(string); ok && binaryStr != "" {
					appName = binaryStr
				}
			}
		}

		if appName != "" {
			if !uniqueApps[appName] {
				uniqueApps[appName] = true
				apps = append(apps, appName)
			}
		}
	}

	return apps, nil
}

func promptForAudioApp() string {
	apps, err := getActiveAudioApps()
	if err != nil {
		log.Printf("[AudioApp] Warning: failed to query active audio apps: %v", err)
	}

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Println("\nSelect an audio source to share:")
		fmt.Println("[0] Default System Audio")
		for i, app := range apps {
			fmt.Printf("[%d] %s (Active)\n", i+1, app)
		}
		manualIdx := len(apps) + 1
		fmt.Printf("[%d] Type application name manually (for muted apps)\n", manualIdx)
		fmt.Print("Enter choice: ")

		choiceStr, err := reader.ReadString('\n')
		if err != nil {
			log.Printf("[AudioApp] Error reading input: %v. Defaulting to system audio.", err)
			return ""
		}
		choiceStr = strings.TrimSpace(choiceStr)
		if choiceStr == "" {
			continue
		}

		var choice int
		_, err = fmt.Sscanf(choiceStr, "%d", &choice)
		if err != nil {
			fmt.Println("Invalid choice. Please enter a number.")
			continue
		}

		if choice == 0 {
			return ""
		} else if choice > 0 && choice <= len(apps) {
			return apps[choice-1]
		} else if choice == manualIdx {
			fmt.Print("Enter application name: ")
			nameStr, err := reader.ReadString('\n')
			if err != nil {
				log.Printf("[AudioApp] Error reading input: %v. Defaulting to system audio.", err)
				return ""
			}
			return strings.TrimSpace(nameStr)
		} else {
			fmt.Printf("Invalid choice. Please choose between 0 and %d.\n", manualIdx)
		}
	}
}

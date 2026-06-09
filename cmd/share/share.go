package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	BackendX11GStreamer CaptureBackend = iota
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
	codecFlag := flag.String("codec", "libx264", "H.264 encoder library. Auto-probes hardware acceleration (NVENC, VA-API) by default.")
	bitrateFlag := flag.Int("bitrate", 8000, "Target video bitrate in kbps (e.g., 8000 for 8 Mbps high-quality 1080p)")
	testFlag := flag.Bool("test", false, "Use a synthetic test video source (lavfi testsrc) instead of X11 capture")
	mockPortalFlag := flag.Bool("mock-portal", false, "Start a mock D-Bus ScreenCast portal in the background (for testing)")
	debugFlag := flag.Bool("debug", false, "Enable verbose diagnostic logging for pipeline debugging")
	headlessFlag := flag.Bool("headless", false, "Use synthetic GStreamer test source (no screen capture, no portal popup).")
	volumeFlag := flag.Float64("volume", 5.0, "Audio volume amplification factor (e.g. 150.0 for 150x volume boost)")
	audioAppFlag := flag.String("audio-app", "", "Name of the app to capture audio from (e.g., 'Firefox'). If empty, falls back to system audio.")
	flag.Parse()

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
	// If the user requested a specific GStreamer encoder, respect it
	if codec == "vah264enc" || codec == "nvh264enc" || codec == "x264enc" {
		return codec, getGstEncoderArgs(codec, preset, tune, gop, bitrate)
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
		sess, err = portal.NewScreenCastSession()
		if err != nil {
			log.Printf("Failed to initialize ScreenCast session: %v. Falling back to X11 grab...", err)
			backend = BackendX11GStreamer
		} else {
			defer sess.Close()
			nodeID, audioNodeID, pwFD, err = sess.Handshake(ctx)
			if err != nil {
				log.Printf("ScreenCast portal handshake failed: %v. Falling back to X11 grab...", err)
				backend = BackendX11GStreamer
			} else {
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

	var virtualSinkModuleID string
	if audioApp != "" {
		cleanupStaleNullSinks()
		cmdSink := exec.Command("pactl", "load-module", "module-null-sink", "sink_name=AuraShare_AppCapture", "sink_properties=device.description=\"AuraShare_AppCapture\"")
		output, err := cmdSink.Output()
		if err != nil {
			log.Printf("[AudioApp] Failed to create virtual null sink: %v. Audio app capture might fail.", err)
		} else {
			virtualSinkModuleID = strings.TrimSpace(string(output))
			log.Printf("[AudioApp] Successfully created virtual null sink. Module ID: %s", virtualSinkModuleID)
			defer func() {
				log.Printf("[AudioApp] Unloading virtual null sink module ID: %s", virtualSinkModuleID)
				_ = exec.Command("pactl", "unload-module", virtualSinkModuleID).Run()
			}()

			// Start background watcher to link app audio to this sink
			go linkAppAudioToSink(ctx, audioApp)
		}
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
	} else if backend == BackendWaylandGStreamer {
		log.Printf("PipeWire capture: Node ID=%d, FD=%d", nodeID, pwFD)
		// Pass the portal's PipeWire FD to GStreamer via ExtraFiles.
		// ExtraFiles[0] becomes fd=3 in the child process.
		pwFile := os.NewFile(uintptr(pwFD), "pipewire-fd")
		extraFiles = append(extraFiles, pwFile)

		videoArgs = []string{
			"pipewiresrc", fmt.Sprintf("path=%d", nodeID), "fd=3", "do-timestamp=true", "keepalive-time=100",
			"!", "queue", "max-size-buffers=3", "leaky=downstream",
			"!", "videoconvert",
			"!", "videoscale",
			"!", fmt.Sprintf("%s,width=%d,height=%d", formatCaps, width, height),
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
	} else {
		// BackendX11GStreamer
		log.Printf("X11 capture: Display=%s", display)
		videoArgs = []string{
			"ximagesrc", fmt.Sprintf("display-name=%s", display), "show-pointer=true", "use-damage=false",
			"!", "queue", "max-size-buffers=3", "leaky=downstream",
			"!", "videoconvert",
			"!", "videoscale",
			"!", fmt.Sprintf("%s,width=%d,height=%d", formatCaps, width, height),
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
	if headlessMode || testMode {
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
		if audioApp != "" && virtualSinkModuleID != "" {
			audioArgs = []string{
				"pulsesrc", "device=AuraShare_AppCapture.monitor",
				"!", "queue", "max-size-buffers=10", "leaky=downstream",
				"!", "audio/x-raw,rate=48000,channels=2",
			}
		} else if audioNodeID > 0 {
			audioArgs = []string{
				"pipewiresrc", fmt.Sprintf("path=%d", audioNodeID), "fd=3", "do-timestamp=true", "keepalive-time=100",
				"!", "queue", "max-size-buffers=10", "leaky=downstream",
				"!", "audio/x-raw,rate=48000,channels=2",
			}
		} else {
			audioArgs = []string{
				"pulsesrc", fmt.Sprintf("device=%s", audioDevice),
				"!", "queue", "max-size-buffers=10", "leaky=downstream",
				"!", "audio/x-raw,rate=48000,channels=2",
			}
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

// getAppAudioPorts parses pw-dump JSON to find the output ports of the specified app.
func getAppAudioPorts(appName string) ([]string, error) {
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

	appNameLower := strings.ToLower(appName)
	var appNodeID uint32
	foundNode := false

	// Step 1: Find the Node ID of the application
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

		// Check application name or application process binary
		match := false
		if appNameVal, ok := props["application.name"]; ok {
			if appNameStr, ok := appNameVal.(string); ok {
				appNameStrLower := strings.ToLower(appNameStr)
				if appNameStrLower == appNameLower || strings.Contains(appNameStrLower, appNameLower) {
					match = true
				}
			}
		}
		if !match {
			if binaryVal, ok := props["application.process.binary"]; ok {
				if binaryStr, ok := binaryVal.(string); ok {
					binaryStrLower := strings.ToLower(binaryStr)
					if binaryStrLower == appNameLower || strings.Contains(binaryStrLower, appNameLower) {
						match = true
					}
				}
			}
		}

		if match {
			appNodeID = obj.ID
			foundNode = true
			// log.Printf("[AudioApp] Found app '%s' Node ID: %d", appName, appNodeID)
			break
		}
	}

	if !foundNode {
		return nil, fmt.Errorf("could not find audio node for application: %s", appName)
	}

	// Step 2: Find all output Port IDs for this Node ID
	var portIDs []string
	for _, obj := range objects {
		if obj.Type != "PipeWire:Interface:Port" {
			continue
		}
		if obj.Info == nil || obj.Info.Props == nil {
			continue
		}
		props := obj.Info.Props

		// Match port's node.id
		nodeIDVal, ok := props["node.id"]
		if !ok {
			continue
		}

		// node.id can be float64 or int or uint32
		var portNodeID uint32
		switch v := nodeIDVal.(type) {
		case float64:
			portNodeID = uint32(v)
		case int:
			portNodeID = uint32(v)
		case uint32:
			portNodeID = v
		default:
			continue
		}

		if portNodeID != appNodeID {
			continue
		}

		// Verify port direction is "out"
		portDirVal, ok := props["port.direction"]
		if !ok {
			continue
		}
		portDir, ok := portDirVal.(string)
		if !ok || portDir != "out" {
			continue
		}

		// Add this port ID
		portIDs = append(portIDs, fmt.Sprintf("%d", obj.ID))
	}

	if len(portIDs) == 0 {
		return nil, fmt.Errorf("no output ports found for application node: %d", appNodeID)
	}

	// log.Printf("[AudioApp] Found %d output ports for app '%s': %v", len(portIDs), appName, portIDs)
	return portIDs, nil
}

// linkAppAudioToSink runs in a background goroutine, monitoring the app's ports
// and linking them to the virtual null sink whenever they appear.
func linkAppAudioToSink(ctx context.Context, appName string) {
	log.Printf("[Watcher] Starting audio link watcher for application '%s'...", appName)
	linked := false
	var activePorts []string

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Watcher] Stopping watcher for '%s' (context cancelled)", appName)
			return
		case <-ticker.C:
			ports, err := getAppAudioPorts(appName)
			if err != nil {
				// App is closed or muted/inactive, reset linked state
				if linked {
					log.Printf("[Watcher] Application '%s' audio ports disappeared. Resetting link state.", appName)
					linked = false
					activePorts = nil
				}
				continue
			}

			// If already linked with exact same ports, check if the ports changed. If not, do nothing.
			if linked && equalSlices(ports, activePorts) {
				continue
			}

			// Perform link
			log.Printf("[Watcher] Application '%s' audio ports discovered: %v. Linking to AuraShare_AppCapture...", appName, ports)

			// Discover the virtual sink's playback ports dynamically
			targets, err := getSinkPlaybackPorts("AuraShare_AppCapture")
			if err != nil {
				log.Printf("[Watcher] Failed to find playback ports for AuraShare_AppCapture: %v. Retrying in next cycle...", err)
				continue
			}

			for i, port := range ports {
				target := targets[0] // Fallback to Left if more than 2 ports
				if i < len(targets) {
					target = targets[i]
				}

				// Execute pw-link
				cmdLink := exec.Command("pw-link", port, target)
				if err := cmdLink.Run(); err != nil {
					log.Printf("[Watcher] Failed to link port %s to %s: %v", port, target, err)
				} else {
					log.Printf("[Watcher] Successfully linked port %s -> %s", port, target)
				}
			}

			linked = true
			activePorts = ports
		}
	}
}

// equalSlices checks if two string slices are equal.
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// cleanupStaleNullSinks unloads any existing AuraShare_AppCapture virtual null sinks.
func cleanupStaleNullSinks() {
	cmd := exec.Command("pactl", "list", "short", "modules")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "module-null-sink") && strings.Contains(line, "sink_name=AuraShare_AppCapture") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				moduleID := fields[0]
				log.Printf("[AudioApp] Unloading stale virtual null sink module ID: %s", moduleID)
				_ = exec.Command("pactl", "unload-module", moduleID).Run()
			}
		}
	}
}

// getSinkPlaybackPorts queries pw-dump for the input (playback) port IDs of the given sink.
func getSinkPlaybackPorts(sinkName string) ([]string, error) {
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

	// 1. Find Node ID for the sink
	var sinkNodeID uint32
	foundNode := false
	sinkNameLower := strings.ToLower(sinkName)

	for _, obj := range objects {
		if obj.Type != "PipeWire:Interface:Node" {
			continue
		}
		if obj.Info == nil || obj.Info.Props == nil {
			continue
		}
		props := obj.Info.Props

		mediaClassVal, ok := props["media.class"]
		if !ok {
			continue
		}
		mediaClass, ok := mediaClassVal.(string)
		if !ok || mediaClass != "Audio/Sink" {
			continue
		}

		if nodeNameVal, ok := props["node.name"]; ok {
			if nodeNameStr, ok := nodeNameVal.(string); ok {
				if strings.ToLower(nodeNameStr) == sinkNameLower {
					sinkNodeID = obj.ID
					foundNode = true
					break
				}
			}
		}
	}

	if !foundNode {
		return nil, fmt.Errorf("could not find sink node: %s", sinkName)
	}

	// 2. Find all input (playback) Port IDs for this Node ID
	var portIDs []string
	var playbackFL, playbackFR string

	for _, obj := range objects {
		if obj.Type != "PipeWire:Interface:Port" {
			continue
		}
		if obj.Info == nil || obj.Info.Props == nil {
			continue
		}
		props := obj.Info.Props

		nodeIDVal, ok := props["node.id"]
		if !ok {
			continue
		}

		var portNodeID uint32
		switch v := nodeIDVal.(type) {
		case float64:
			portNodeID = uint32(v)
		case int:
			portNodeID = uint32(v)
		case uint32:
			portNodeID = v
		default:
			continue
		}

		if portNodeID != sinkNodeID {
			continue
		}

		portDirVal, ok := props["port.direction"]
		if !ok {
			continue
		}
		portDir, ok := portDirVal.(string)
		if !ok || portDir != "in" {
			continue
		}

		portNameVal, ok := props["port.name"]
		if !ok {
			continue
		}
		portName, ok := portNameVal.(string)
		if !ok {
			continue
		}

		if portName == "playback_FL" {
			playbackFL = fmt.Sprintf("%d", obj.ID)
		} else if portName == "playback_FR" {
			playbackFR = fmt.Sprintf("%d", obj.ID)
		} else {
			portIDs = append(portIDs, fmt.Sprintf("%d", obj.ID))
		}
	}

	var result []string
	if playbackFL != "" {
		result = append(result, playbackFL)
	}
	if playbackFR != "" {
		result = append(result, playbackFR)
	}
	result = append(result, portIDs...)

	if len(result) == 0 {
		return nil, fmt.Errorf("no playback ports found for sink node: %d", sinkNodeID)
	}

	return result, nil
}

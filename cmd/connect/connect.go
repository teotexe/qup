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
	"syscall"
	"time"

	"aurashare/internal/crypto"
	"aurashare/internal/stats"

	"github.com/quic-go/quic-go"
)

// Connection main loop

func main() {
	// Set underlying defaults for optimal low-latency streaming profiles
	os.Setenv("PULSE_LATENCY_MSEC", "30")

	// Parse CLI flags
	// 5000000 bytes (5MB) ensures we catch a keyframe even on high bitrate streams so the window opens
	probesizeFlag := flag.Int("probesize", 5000000, "ffplay probesize in bytes")
	// 2000000 microseconds (2 seconds) gives it enough time to analyze the stream
	analyzeDurationFlag := flag.Int("analyze-duration", 2000000, "ffplay analyze_duration in microseconds")

	lowDelayFlag := flag.Bool("low-delay", true, "Enable ffplay low_delay flag")
	framedropFlag := flag.Bool("framedrop", true, "Enable ffplay framedrop flag")
	ffplayLogLevelFlag := flag.String("loglevel", "warning", "ffplay log level (quiet, panic, fatal, error, warning, info, verbose, debug)")
	testFlag := flag.Bool("test", false, "Run in headless test mode, printing data receipt diagnostics instead of spawning ffplay")
	hwaccelFlag := flag.String("hwaccel", "vaapi", "Hardware acceleration method for ffplay decoding (vaapi, none)")
	hwaccelDeviceFlag := flag.String("hwaccel-device", "/dev/dri/renderD128", "DRI render node for hardware acceleration")
	flag.Parse()

	// The peer address should be the first positional argument
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: ./connect [flags] [BOB_WAN_IP:PORT]")
		fmt.Println("\nFlags:")
		flag.PrintDefaults()
		os.Exit(1)
	}
	targetAddr := args[0]

	log.Printf("Starting AuraShare Receiver (Alice) connecting to %s...", targetAddr)
	log.Printf("Receiver config: TestMode=%v, probesize=%d, analyze_duration=%d, low_delay=%v, framedrop=%v, loglevel=%s, hwaccel=%s",
		*testFlag, *probesizeFlag, *analyzeDurationFlag, *lowDelayFlag, *framedropFlag, *ffplayLogLevelFlag, *hwaccelFlag)

	// Create client TLS config
	tlsConfig := crypto.GenerateClientTLSConfig()

	// Set up cancellation context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Shutting down AuraShare Receiver...")
		cancel()
	}()

	// Establish connection to host Bob
	log.Printf("Connecting to host via QUIC...")
	quicConfig := &quic.Config{
		EnableDatagrams: true,
	}
	conn, err := quic.DialAddr(ctx, targetAddr, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("Failed to establish QUIC connection: %v", err)
	}
	defer conn.CloseWithError(0, "connection closed")

	log.Printf("Connected to host %s! Accepting media stream...", conn.RemoteAddr().String())

	stream, err := conn.AcceptUniStream(ctx)
	if err != nil {
		log.Fatalf("Failed to accept QUIC stream: %v", err)
	}
	log.Printf("Media transport switched to QUIC Stream. Launching ffplay rendering window...")

	// Set up stats reporting
	reporter := stats.NewStatsReporter(false)
	reporter.StartReporting(2 * time.Second)
	defer reporter.Stop()

	// Wrap stream reader with stats reporter
	proxyReader := stats.NewProxyReader(stream, reporter)

	if *testFlag {
		log.Printf("╔══════════════════════════════════════════════════════════╗")
		log.Printf("║  HEADLESS TEST MODE - No video window will appear        ║")
		log.Printf("║  Monitoring data receipt from sender...                  ║")
		log.Printf("║  Look for [Receiver] Rate lines to confirm data flow     ║")
		log.Printf("╚══════════════════════════════════════════════════════════╝")

		// Read data in chunks and report receipt
		buf := make([]byte, 64*1024)
		totalBytes := int64(0)
		chunkCount := 0
		firstByteTime := time.Time{}

		for {
			n, readErr := proxyReader.Read(buf)
			if n > 0 {
				totalBytes += int64(n)
				chunkCount++
				if firstByteTime.IsZero() {
					firstByteTime = time.Now()
					log.Printf("[Receive] ✓ First data received! %d bytes", n)
				}
				// Log every 100th chunk to avoid spam
				if chunkCount%100 == 0 {
					log.Printf("[Receive] Chunk #%d | This: %d bytes | Total: %.2f MB",
						chunkCount, n, float64(totalBytes)/(1024*1024))
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					log.Printf("[Receive] Stream ended (EOF)")
				} else {
					log.Printf("[Receive] Stream error: %v", readErr)
				}
				break
			}
		}

		// Stop stats reporter before printing summary
		reporter.Stop()

		elapsed := time.Since(firstByteTime)
		log.Printf("═══════════════════════════════════════════════════════════")
		log.Printf("[Receive] SUMMARY: %d chunks | %.2f MB | Duration: %v",
			chunkCount, float64(totalBytes)/(1024*1024), elapsed.Round(time.Millisecond))
		if totalBytes == 0 {
			log.Printf("[Receive] ✗ NO DATA RECEIVED - pipeline issue on sender side")
		} else {
			log.Printf("[Receive] ✓ Data transfer successful!")
		}
		return
	}

	log.Printf("Launching ffplay rendering window...")

	// Build ffplay command
	ffplayArgs := []string{
		"-loglevel", *ffplayLogLevelFlag,
	}

	if *hwaccelFlag != "none" {
		log.Printf("Enabling %s hardware-accelerated decoding (device: %s)", *hwaccelFlag, *hwaccelDeviceFlag)
		ffplayArgs = append(ffplayArgs, "-hwaccel", *hwaccelFlag)

		// CRITICAL FIX: Download the hardware frames to system memory before rendering.
		// This forces ffplay to bypass the broken Vulkan pipeline and open the window.
		ffplayArgs = append(ffplayArgs, "-vf", "hwdownload,format=nv12")
	}

	ffplayArgs = append(ffplayArgs,
		"-probesize", fmt.Sprintf("%d", *probesizeFlag),
		"-analyzeduration", fmt.Sprintf("%d", *analyzeDurationFlag),

		// Force immediate packet flushing and disable demuxer caching
		"-fflags", "nobuffer+flush_packets",
		"-flags", "low_delay",

		// Core performance over quality parameters
		"-strict", "experimental",
		"-infbuf",
		"-autoexit",

		// Clock synchronization configuration
		"-sync", "audio",
	)

	if *framedropFlag {
		ffplayArgs = append(ffplayArgs, "-framedrop")
	}

	ffplayArgs = append(ffplayArgs, "-f", "mpegts", "-i", "pipe:0")

	var cmd *exec.Cmd
	if _, err := exec.LookPath("ffplay"); err == nil {
		cmd = exec.CommandContext(ctx, "ffplay", ffplayArgs...)
	} else if _, err := os.Stat("ffplay.exe"); err == nil {
		cmd = exec.CommandContext(ctx, "./ffplay.exe", ffplayArgs...)
	} else if _, err := exec.LookPath("vlc"); err == nil {
		cmd = exec.CommandContext(ctx, "vlc", "-", "--network-caching=100")
	} else {
		cmd = exec.CommandContext(ctx, `C:\Program Files\VideoLAN\VLC\vlc.exe`, "-", "--network-caching=100")
	}

	// CRITICAL FIX: Ensure environment variables correctly append to the current OS context
	cmd.Env = os.Environ()
	if *hwaccelFlag == "vaapi" {
		cmd.Env = append(cmd.Env,
			"LIBVA_DRIVER_NAME=iHD",
			fmt.Sprintf("LIBVA_DEVICE=%s", *hwaccelDeviceFlag),
		)
	}

	// Get ffplay stdin pipe
	ffplayStdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("Failed to open ffplay stdin pipe: %v", err)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start ffplay
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start ffplay process: %v", err)
	}

	// Ensure ffplay process is killed if we exit this function
	defer func() {
		log.Println("Stopping ffplay process...")
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()

	// Copy incoming network stream to ffplay stdin
	log.Printf("Streaming video feed to display rendering engine...")
	_, err = io.Copy(ffplayStdin, proxyReader)
	if err != nil && err != io.EOF {
		log.Printf("Streaming finished with error: %v", err)
	} else {
		log.Printf("Streaming finished successfully.")
	}
}

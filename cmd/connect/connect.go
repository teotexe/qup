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
	// os.Setenv("PULSE_LATENCY_MSEC", "30")
	// os.Setenv("SDL_AUDIODRIVER", "pulseaudio")
	// Parse CLI flags
	// 1048576 bytes (1MB) is recommended to catch the MPEG-TS headers reliably and avoid races
	probesizeFlag := flag.Int("probesize", 1048576, "ffplay probesize in bytes")
	// 1000000 microseconds (1 second) gives it a brief window to analyze the streams.
	analyzeDurationFlag := flag.Int("analyze-duration", 1000000, "ffplay analyze_duration in microseconds")
	lowDelayFlag := flag.Bool("low-delay", true, "Enable ffplay low_delay flag")
	framedropFlag := flag.Bool("framedrop", true, "Enable ffplay framedrop flag")
	ffplayLogLevelFlag := flag.String("loglevel", "warning", "ffplay log level (quiet, panic, fatal, error, warning, info, verbose, debug)")
	testFlag := flag.Bool("test", false, "Run in headless test mode, printing data receipt diagnostics instead of spawning ffplay")
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
	log.Printf("Receiver config: TestMode=%v, probesize=%d, analyze_duration=%d, low_delay=%v, framedrop=%v, loglevel=%s",
		*testFlag, *probesizeFlag, *analyzeDurationFlag, *lowDelayFlag, *framedropFlag, *ffplayLogLevelFlag)

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
		log.Printf("║  HEADLESS TEST MODE - No video window will appear       ║")
		log.Printf("║  Monitoring data receipt from sender...                 ║")
		log.Printf("║  Look for [Receiver] Rate lines to confirm data flow   ║")
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
	// Example: ffplay -probesize 32 -analyzeduration 0 -flags low_delay -framedrop -i pipe:0
	ffplayArgs := []string{
		"-loglevel", *ffplayLogLevelFlag,
		"-probesize", fmt.Sprintf("%d", *probesizeFlag), // Ensure default is 32
		"-analyzeduration", fmt.Sprintf("%d", *analyzeDurationFlag), // Ensure default is 0

		// Force immediate packet flushing and disable demuxer caching
		"-fflags", "nobuffer+flush_packets",
		"-flags", "low_delay",

		// Core performance over quality parameters
		"-strict", "experimental", // Allows cutting corner optimizations if available
		"-infbuf", // Enable infinite buffer to drain network socket continuously and prevent pipe blocking
		"-autoexit", // Cleanly kill window if stream tears down

		// Clock synchronization configuration
		"-sync", "audio", // Sync to the audio master clock for low-jitter smooth playback
	}

	if *framedropFlag {
		// Aggressively drop frames at both the decoder AND filter/display level
		ffplayArgs = append(ffplayArgs, "-framedrop", "-vf", "setpts=N/FRAME_RATE/TB")
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
		// Fallback to hardcoded VLC on Windows
		cmd = exec.CommandContext(ctx, `C:\Program Files\VideoLAN\VLC\vlc.exe`, "-", "--network-caching=100")
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

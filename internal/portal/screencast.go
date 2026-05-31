package portal

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// ScreenCastSession represents a helper to manage the D-Bus ScreenCast portal handshake.
type ScreenCastSession struct {
	conn *dbus.Conn
}

// NewScreenCastSession creates a new portal session.
func NewScreenCastSession() (*ScreenCastSession, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to session bus: %w", err)
	}
	return &ScreenCastSession{conn: conn}, nil
}

// Close closes the D-Bus connection.
func (s *ScreenCastSession) Close() error {
	return s.conn.Close()
}

// Handshake performs the CreateSession -> SelectSources -> Start -> OpenPipeWireRemote flow
// and returns the PipeWire stream node ID and the PipeWire socket file descriptor.
func (s *ScreenCastSession) Handshake(ctx context.Context) (uint32, int, error) {
	// Generate unique tokens for requests and session
	rand.Seed(time.Now().UnixNano())
	tokenSuffix := fmt.Sprintf("%d", rand.Intn(1000000))
	sessionToken := "aurashare_session_" + tokenSuffix
	createToken := "aurashare_create_" + tokenSuffix
	selectToken := "aurashare_select_" + tokenSuffix
	startToken := "aurashare_start_" + tokenSuffix

	bus := s.conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")

	// Set up signal matching for responses
	err := s.conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.portal.Request"),
		dbus.WithMatchMember("Response"),
		dbus.WithMatchSender("org.freedesktop.portal.Desktop"),
	)
	if err != nil {
		return 0, -1, fmt.Errorf("failed to add match signal: %w", err)
	}

	signals := make(chan *dbus.Signal, 10)
	s.conn.Signal(signals)
	defer s.conn.RemoveSignal(signals)

	// Helper to wait for Response signal on a given request path
	waitResponse := func(reqPath dbus.ObjectPath) (uint32, map[string]dbus.Variant, error) {
		for {
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case sig := <-signals:
				if sig.Path == reqPath && sig.Name == "org.freedesktop.portal.Request.Response" {
					if len(sig.Body) < 2 {
						return 0, nil, fmt.Errorf("invalid response payload size")
					}
					respCode, ok1 := sig.Body[0].(uint32)
					results, ok2 := sig.Body[1].(map[string]dbus.Variant)
					if !ok1 || !ok2 {
						return 0, nil, fmt.Errorf("invalid response payload type")
					}
					return respCode, results, nil
				}
			}
		}
	}

	// 1. Create Session
	log.Println("[Portal] Creating ScreenCast session...")
	createOpts := map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(sessionToken),
		"handle_token":         dbus.MakeVariant(createToken),
	}
	var createReqPath dbus.ObjectPath
	err = bus.CallWithContext(ctx, "org.freedesktop.portal.ScreenCast.CreateSession", 0, createOpts).Store(&createReqPath)
	if err != nil {
		return 0, -1, fmt.Errorf("CreateSession call failed: %w", err)
	}

	respCode, results, err := waitResponse(createReqPath)
	if err != nil {
		return 0, -1, fmt.Errorf("error waiting for CreateSession response: %w", err)
	}
	if respCode != 0 {
		return 0, -1, fmt.Errorf("CreateSession was cancelled or failed with code %d", respCode)
	}

	sessionHandleStr, ok := results["session_handle"].Value().(string)
	if !ok {
		return 0, -1, fmt.Errorf("CreateSession did not return a valid session_handle")
	}
	sessionHandle := dbus.ObjectPath(sessionHandleStr)
	log.Printf("[Portal] Session created successfully: %s", sessionHandle)

	// 2. Select Sources
	log.Println("[Portal] Selecting screen source...")
	selectOpts := map[string]dbus.Variant{
		"types":        dbus.MakeVariant(uint32(3)), // 3 allows selecting BOTH Screens and Windows
		"multiple":     dbus.MakeVariant(false),
		"cursor_mode":  dbus.MakeVariant(uint32(2)),
		"handle_token": dbus.MakeVariant(selectToken),
	}
	var selectReqPath dbus.ObjectPath
	err = bus.CallWithContext(ctx, "org.freedesktop.portal.ScreenCast.SelectSources", 0, sessionHandle, selectOpts).Store(&selectReqPath)
	if err != nil {
		return 0, -1, fmt.Errorf("SelectSources call failed: %w", err)
	}

	respCode, _, err = waitResponse(selectReqPath)
	if err != nil {
		return 0, -1, fmt.Errorf("error waiting for SelectSources response: %w", err)
	}
	if respCode != 0 {
		return 0, -1, fmt.Errorf("SelectSources was cancelled or failed with code %d", respCode)
	}
	log.Println("[Portal] Screen source selected successfully.")

	// 3. Start Session
	log.Println("[Portal] Starting ScreenCast session...")
	startOpts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(startToken),
	}
	var startReqPath dbus.ObjectPath
	err = bus.CallWithContext(ctx, "org.freedesktop.portal.ScreenCast.Start", 0, sessionHandle, "", startOpts).Store(&startReqPath)
	if err != nil {
		return 0, -1, fmt.Errorf("Start call failed: %w", err)
	}

	respCode, results, err = waitResponse(startReqPath)
	if err != nil {
		return 0, -1, fmt.Errorf("error waiting for Start response: %w", err)
	}
	if respCode != 0 {
		return 0, -1, fmt.Errorf("Start was cancelled or failed with code %d", respCode)
	}

	streamsVal, ok := results["streams"]
	if !ok {
		return 0, -1, fmt.Errorf("Start response did not contain streams")
	}

	// The signature is a(ua{sv}) -> slice of structs containing (uint32, map[string]variant)
	// Let's use dbus.Store to cleanly unpack it.
	var streams []struct {
		NodeID  uint32
		Options map[string]dbus.Variant
	}
	err = dbus.Store([]interface{}{streamsVal.Value()}, &streams)
	if err != nil {
		return 0, -1, fmt.Errorf("failed to decode streams: %w", err)
	}

	if len(streams) == 0 {
		return 0, -1, fmt.Errorf("no screencast streams returned")
	}

	nodeID := streams[0].NodeID
	log.Printf("[Portal] ScreenCast session started. PipeWire Node ID: %d", nodeID)

	// 4. Open PipeWire Remote
	log.Println("[Portal] Opening PipeWire remote...")
	openOpts := map[string]dbus.Variant{}
	var fd dbus.UnixFD
	err = bus.CallWithContext(ctx, "org.freedesktop.portal.ScreenCast.OpenPipeWireRemote", 0, sessionHandle, openOpts).Store(&fd)
	if err != nil {
		return 0, -1, fmt.Errorf("OpenPipeWireRemote call failed: %w", err)
	}

	log.Printf("[Portal] PipeWire remote opened. File Descriptor: %d", fd)
	return nodeID, int(fd), nil
}

// IsWayland returns true if the environment suggests we are running on Wayland.
func IsWayland() bool {
	return strings.ToLower(os.Getenv("XDG_SESSION_TYPE")) == "wayland" || os.Getenv("AURASHARE_FORCE_WAYLAND") == "1"
}

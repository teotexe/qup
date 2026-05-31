package portal

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// MockScreenCast implements the org.freedesktop.portal.ScreenCast interface.
type MockScreenCast struct {
	conn *dbus.Conn
}

// CreateSession mock method.
func (m *MockScreenCast) CreateSession(options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	reqPath := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/mock_sender/create_token")
	log.Printf("[MockPortal] Received CreateSession. Replying with path %s", reqPath)

	go func() {
		time.Sleep(100 * time.Millisecond)
		results := map[string]dbus.Variant{
			"session_handle": dbus.MakeVariant("/org/freedesktop/portal/desktop/session/mock_session"),
		}
		err := m.conn.Emit(reqPath, "org.freedesktop.portal.Request.Response", uint32(0), results)
		if err != nil {
			log.Printf("[MockPortal] Failed to emit CreateSession Response: %v", err)
		}
	}()

	return reqPath, nil
}

// SelectSources mock method.
func (m *MockScreenCast) SelectSources(session dbus.ObjectPath, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	reqPath := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/mock_sender/select_token")
	log.Printf("[MockPortal] Received SelectSources on %s. Replying with path %s", session, reqPath)

	go func() {
		time.Sleep(100 * time.Millisecond)
		results := map[string]dbus.Variant{}
		err := m.conn.Emit(reqPath, "org.freedesktop.portal.Request.Response", uint32(0), results)
		if err != nil {
			log.Printf("[MockPortal] Failed to emit SelectSources Response: %v", err)
		}
	}()

	return reqPath, nil
}

type mockStream struct {
	NodeID  uint32
	Options map[string]dbus.Variant
}

// Start mock method.
func (m *MockScreenCast) Start(session dbus.ObjectPath, parentWindow string, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	reqPath := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/mock_sender/start_token")
	log.Printf("[MockPortal] Received Start on %s. Replying with path %s", session, reqPath)

	go func() {
		time.Sleep(100 * time.Millisecond)
		streams := []mockStream{
			{
				NodeID:  uint32(42),
				Options: map[string]dbus.Variant{},
			},
		}
		results := map[string]dbus.Variant{
			"streams": dbus.MakeVariant(streams),
		}
		err := m.conn.Emit(reqPath, "org.freedesktop.portal.Request.Response", uint32(0), results)
		if err != nil {
			log.Printf("[MockPortal] Failed to emit Start Response: %v", err)
		}
	}()

	return reqPath, nil
}

// OpenPipeWireRemote mock method.
func (m *MockScreenCast) OpenPipeWireRemote(session dbus.ObjectPath, options map[string]dbus.Variant) (dbus.UnixFD, *dbus.Error) {
	log.Printf("[MockPortal] Received OpenPipeWireRemote on %s.", session)
	r, _, err := os.Pipe()
	if err != nil {
		return dbus.UnixFD(0), dbus.NewError("org.freedesktop.portal.Error.Failed", []interface{}{err.Error()})
	}
	return dbus.UnixFD(r.Fd()), nil
}

// StartMockPortal starts a mock D-Bus service under the org.freedesktop.portal.Desktop name.
func StartMockPortal(ctx context.Context) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("failed to connect to session bus: %w", err)
	}

	reply, err := conn.RequestName("org.freedesktop.portal.Desktop", dbus.NameFlagReplaceExisting)
	if err != nil {
		return fmt.Errorf("failed to request name: %w", err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("name org.freedesktop.portal.Desktop already owned: %v", reply)
	}

	mockSC := &MockScreenCast{conn: conn}
	err = conn.Export(mockSC, "/org/freedesktop/portal/desktop", "org.freedesktop.portal.ScreenCast")
	if err != nil {
		return fmt.Errorf("failed to export screencast portal object: %w", err)
	}

	log.Println("[MockPortal] Mock ScreenCast portal running at org.freedesktop.portal.Desktop on session bus...")
	
	// Keep running until ctx is cancelled
	go func() {
		<-ctx.Done()
		conn.Close()
		log.Println("[MockPortal] Mock ScreenCast portal stopped.")
	}()

	return nil
}

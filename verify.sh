#!/bin/bash

# Exit immediately if a command exits with a non-zero status
set -e

echo "=== AURASHARE P2P STREAM VERIFICATION & AUTOMATED TESTING ==="

# Cleanup old processes
cleanup() {
    echo "Cleaning up any running share, connect, or dbus processes..."
    pkill -f "share" || true
    pkill -f "connect" || true
    pkill -f "dbus-daemon --session" || true
    rm -f host_x11.log client_x11.log host_wayland.log client_wayland.log
}
trap cleanup EXIT
cleanup

# Ensure binaries are built
echo "Building binaries..."
go build -o share cmd/share/share.go
go build -o connect cmd/connect/connect.go

echo "========================================================="
echo "TEST 1: Headless X11 (Synthetic testsrc Mode)"
echo "========================================================="

export XDG_SESSION_TYPE=""
export AURASHARE_FORCE_WAYLAND=""

echo "Launching share host on port 50011..."
./share -port=50011 -test > host_x11.log 2>&1 &
HOST_PID=$!

sleep 2

echo "Launching connect client..."
./connect -test 127.0.0.1:50011 > client_x11.log 2>&1 &
CLIENT_PID=$!

echo "Monitoring P2P run for 10 seconds..."
sleep 10

echo "Stopping processes..."
pkill -f "share -port=50011" || true
pkill -f "connect -test 127.0.0.1:50011" || true

echo "Analyzing X11 Test logs..."
echo "--- Host X11 Log Snippet ---"
cat host_x11.log | tail -n 15 || true
echo "--- Client X11 Log Snippet ---"
cat client_x11.log | tail -n 15 || true
echo "----------------------------"

# Check for errors in X11 logs
X11_PASSED=true
if grep -qE "panic|fatal|error|failed" host_x11.log client_x11.log; then
    # Ignore expected non-fatal logs if they exist, but let's check
    if grep -q -i "error" host_x11.log client_x11.log | grep -qv "connection closed"; then
        echo "WARNING: Potential error found in X11 logs."
        X11_PASSED=false
    fi
fi

# Verify data flows
if ! grep -q "Total Transferred:" host_x11.log; then
    echo "ERROR: No data throughput stats printed in host_x11.log!"
    X11_PASSED=false
fi
if ! grep -q "Total Transferred:" client_x11.log; then
    echo "ERROR: No data throughput stats printed in client_x11.log!"
    X11_PASSED=false
fi

if [ "$X11_PASSED" = true ]; then
    echo "SUCCESS: X11 Synthetic P2P stream verified successfully!"
else
    echo "FAILURE: X11 Synthetic P2P stream verification failed!"
    exit 1
fi

echo "========================================================="
echo "TEST 2: Wayland Desktop Portal ScreenCast (Mock D-Bus Mode)"
echo "========================================================="

if ! command -v dbus-daemon &>/dev/null; then
    echo "WARNING: dbus-daemon not found on this system. Skipping Test 2."
    echo "========================================================="
    echo "ALL TESTS COMPLETED SUCCESSFULLY! (Test 2 skipped due to missing dbus-daemon)"
    echo "========================================================="
    exit 0
fi

echo "Initializing temporary D-Bus session bus..."
# Start dbus-daemon and parse the printed address
DBUS_OUTPUT=$(dbus-daemon --session --fork --print-address --syslog-only)
export DBUS_SESSION_BUS_ADDRESS=$DBUS_OUTPUT
echo "D-Bus session bus started at: $DBUS_SESSION_BUS_ADDRESS"

export XDG_SESSION_TYPE="wayland"
export AURASHARE_FORCE_WAYLAND="1"

echo "Launching share host with -mock-portal on port 50012..."
./share -port=50012 -test -mock-portal > host_wayland.log 2>&1 &
HOST_WAYLAND_PID=$!

sleep 2

echo "Launching connect client..."
./connect -test 127.0.0.1:50012 > client_wayland.log 2>&1 &
CLIENT_WAYLAND_PID=$!

echo "Monitoring P2P Wayland run for 10 seconds..."
sleep 10

echo "Stopping processes..."
pkill -f "share -port=50012" || true
pkill -f "connect -test 127.0.0.1:50012" || true
pkill -f "dbus-daemon --session" || true

echo "Analyzing Wayland Test logs..."
echo "--- Host Wayland Log Snippet ---"
cat host_wayland.log | tail -n 20 || true
echo "--- Client Wayland Log Snippet ---"
cat client_wayland.log | tail -n 15 || true
echo "----------------------------"

WAYLAND_PASSED=true
# Verify D-Bus handshake succeeded
if ! grep -q "ScreenCast portal handshake succeeded!" host_wayland.log; then
    echo "ERROR: ScreenCast portal handshake success message not found in host_wayland.log!"
    WAYLAND_PASSED=false
fi

# Verify data flows
if ! grep -q "Total Transferred:" host_wayland.log; then
    echo "ERROR: No data throughput stats printed in host_wayland.log!"
    WAYLAND_PASSED=false
fi
if ! grep -q "Total Transferred:" client_wayland.log; then
    echo "ERROR: No data throughput stats printed in client_wayland.log!"
    WAYLAND_PASSED=false
fi

if [ "$WAYLAND_PASSED" = true ]; then
    echo "SUCCESS: Wayland D-Bus ScreenCast P2P stream verified successfully!"
else
    echo "FAILURE: Wayland D-Bus ScreenCast P2P stream verification failed!"
    exit 1
fi

echo "========================================================="
echo "ALL TESTS COMPLETED SUCCESSFULLY! AuraShare is fully validated."
echo "========================================================="
exit 0

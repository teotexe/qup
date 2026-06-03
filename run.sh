#!/bin/bash

# Start Terminal 1 (Host) in the current window/tab
ptyxis --tab -- bash -c "go run ./cmd/share/share.go -port=22050; exec bash"

# Wait for 2 seconds to let the host initialize
sleep 2

# Start Terminal 2 (Client) as a vertical split inside the active window
ptyxis --tab -- bash -c "go run ./cmd/connect/connect.go 127.0.0.1:22050; exec bash"

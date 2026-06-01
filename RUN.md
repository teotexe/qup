# AuraShare Streaming & Peer Connection Guide

AuraShare is a zero-infrastructure, low-latency peer-to-peer screen-sharing engine designed for performance comparable to Discord. This document details how to compile, run, and configure the AuraShare Host (`share`) and Client (`connect`) binaries.

---

## Quick Start (How to Run)

### 1. Compile the Binaries
Ensure Go is installed, and run the compilation command from the root of the project:
```bash
go build -o share cmd/share/share.go
go build -o connect cmd/connect/connect.go
```

### 2. Start the Host (Bob)
Bob is the sender sharing his screen. He starts the listener on a forwarded port (e.g., `50001`):
```bash
./share -port=50001
```

### 3. Connect as the Client (Alice)
Alice is the receiver watching the stream. She connects by specifying Bob's target IP and port:
```bash
./connect 127.0.0.1:50001
```

---

## 1. Host Command Line Interface (`share`)

The host handles desktop capturing, dynamic hardware-accelerated video/audio encoding, and QUIC packet broadcasting.

### CLI Syntax
```bash
./share [flags]
```

### Host Parameters Reference

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `-port` | `int` | `50001` | The UDP port to open and listen on for incoming client QUIC connections. |
| `-bitrate` | `int` | `8000` | Target video bitrate in kbps. Defaults to **`8000`** (8 Mbps) for beautiful, pixel-free 1080p 60fps streams. |
| `-codec` | `string` | `"libx264"` | The video codec element/library to use. Under Wayland/GStreamer, it auto-probes GPU hardware acceleration elements (`vaapih264enc`, `nvh264enc`, and falls back to software `x264enc`). For X11, it probes `h264_qsv`, `h264_nvenc`, `h264_vaapi`, and falls back to `libx264`. |
| `-fps` | `int` | `60` | Captured frame rate. A setting of `60` offers butter-smooth Discord-level capture. |
| `-size` | `string` | `"1920x1080"` | Capture resolution width x height. |
| `-g` | `int` | `60` | GOP (Group of Pictures / Keyframe Interval). Forces a keyframe refresh every $N$ frames to allow late-joining clients to instantly synchronize. |
| `-preset` | `string` | `"ultrafast"` | x264 software encoder speed preset (e.g. `ultrafast`, `superfast`, `veryfast`, `medium`). Faster presets decrease CPU load. |
| `-tune` | `string` | `"zerolatency"` | x264 software encoder latency tuning mode. |
| `-display` | `string` | `$DISPLAY` | X11 display string to grab (only applicable on X11 environments, e.g. `:0.0`). |
| `-test` | `bool` | `false` | Synthetic test source mode. Uses FFmpeg's `lavfi testsrc` to feed dummy diagnostic frames instead of capturing the physical screen. |
| `-mock-portal` | `bool` | `false` | Forces a mock D-Bus ScreenCast portal backend in the background (used for testing DBus/Wayland handshake without prompts). |
| `-headless` | `bool` | `false` | Forces GStreamer to run with `videotestsrc` in a headless mode. Bypasses D-Bus popups, testing the GStreamer $\rightarrow$ FFmpeg $\rightarrow$ QUIC loopback. |
| `-debug` | `bool` | `false` | Enables verbose debug logs showing full spawned GStreamer/FFmpeg command lines, element properties, and thread logs. |

---

## 2. Client Command Line Interface (`connect`)

The client establishes a QUIC link, pulls down the H.264/Opus MPEG-TS stream, and feeds it into the rendering engine window.

### CLI Syntax
```bash
./connect [flags] [HOST_IP:PORT]
```

### Client Parameters Reference

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `-probesize` | `int` | `32768` | Size of ffplay buffer in bytes used to probe stream formats. A smaller buffer lowers initial window spawn latency (default optimized to 32KB). |
| `-analyze-duration` | `int` | `100000` | Duration (in microseconds) that ffplay spends analyzing stream properties before opening the window (default optimized to 100ms). |
| `-low-delay` | `bool` | `true` | Tells the player to ignore standard container synchronization buffers and play packets immediately upon network arrival. |
| `-framedrop` | `bool` | `true` | Drop late video frames if decoding lags behind the audio clock. Highly recommended to prevent video drift over long sessions. |
| `-loglevel` | `string` | `"warning"` | Log verbosity for the inner player (options: `quiet`, `panic`, `fatal`, `error`, `warning`, `info`, `verbose`, `debug`). |
| `-test` | `bool` | `false` | Headless client diagnostic mode. Network packets are consumed and measured, but the player UI window is not spawned (useful for network latency testing). |

---

## Premium Tuning Recipes

### 1. High-Performance GPU Capture & Encode (Default Wayland)
If running on a modern Wayland desktop environment with an Intel, AMD, or NVIDIA GPU, starting Bob with default options will automatically activate GStreamer-native hardware GPU capture and encoding:
```bash
./share
```

### 2. High-Quality LAN Mode (Infinite Bandwidth)
If streaming over a high-speed local network or high-speed fiber link, you can push the quality to the absolute limit by raising the target bitrate:
```bash
./share -bitrate=15000 -size=1920x1080 -fps=60
```

### 3. Lower Bandwidth Mode (Mobile Hotspot / Poor Wi-Fi)
If Bob's upload connection is constrained, reduce the framerate and lower the target bitrate to maintain smoothness:
```bash
./share -bitrate=3000 -fps=30 -preset=veryfast
```

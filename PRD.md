AuraShare is a decentralized, zero-infrastructure, peer-to-peer (P2P) screen-sharing application designed for home users. The primary objective is to allow two users behind standard home routers to establish a direct, secure, high-definition, ultra-low-latency video stream of their desktop environment without routing media through centralized, expensive relay servers.

The streaming user must forward a port on his router and the peer can directly stream. AuraShare minimizes infrastructure overhead to $0$ while delivering raw, unthrottled P2P performance.

## Technical Stack

* **Core Runtime:** Go (Golang) - Single binary, static compilation orchestrating subprocess pipelines.
* **P2P Transport:** QUIC via `[github.com/quic-go/quic-go](https://github.com/quic-go/quic-go)`.
* **Media Capture & Encoding Engine:** Native `ffmpeg` CLI binary (invoked via Go `os/exec`) configured via stdout piping.
* **UI Render Engine:** `gioui.org` (Gio) for the window wrapper, executing a nested context window or direct stream consumer depending on platform optimization.
> *Note for MVP:* To hit the 1-week deadline, the Go receiver app will pipe network bytes straight into an `ffplay` subprocess which instantly spawns a zero-latency SDL rendering window, skipping the manual GPU pointer layout.


---

### 1. Host Activation (Bob)

* Bob executes `./share -port=50001`.
* The Go app spins up a `quic-go` listener on the specified forwarded port using a transient, in-memory self-signed TLS certificate.
* The app waits for Alice to connect.

### 2. Peer Connection & Handshake (Alice)

* Alice executes `./connect [BOB_WAN_IP:50001]`.
* Alice initiates a native QUIC connection.
* **Security:** Cryptographic handshakes, identity validation, and forward-secret encryption (ChaCha20-Poly1305/AES-GCM) are managed natively by QUIC's built-in TLS 1.3 layer using `InsecureSkipVerify: true` to bypass the lack of an explicit CA.

### 3. The Hot Path (Streaming Protocol)

#### A. Capture & Encode (The Sender)

Upon successful QUIC session establishment, Bob’s Go runtime spawns a background `ffmpeg` process.

* **X11 Session Command (Linux/Xorg):**
```bash
ffmpeg -f x11grab -video_size 1920x1080 -framerate 60 -i :0.0 -c:v libx264 -preset ultrafast -tune zerolatency -g 1 -f h264 pipe:1
```

* The Go app intercepts `ffmpeg`'s `StdoutPipe()`. As raw Annex B H.264 byte blocks arrive, they are instantly written directly to a raw, un-buffered QUIC stream context.

#### B. Transport

* Data is carried over UDP via QUIC. Because H.264 Annex B contains natural start codes (`0x00000001`), network packet drops do not cause head-of-line blocking; any dropped packet simply causes minor structural visual distortion until the next keyframe or independent P-frame block satisfies the stream.

#### C. Decode & Render (The Receiver)

* Alice’s Go runtime establishes the connection and instantly executes a localized `ffplay` rendering subprocess:
```bash
    ffplay -probesize 32 -analyze_duration 0 -flags low_delay -framedrop -i pipe:0
```
*   The Go runtime binds the incoming QUIC network stream reader directly to the `ffplay` process's `StdinPipe()`.
*   `ffplay` ingests the raw compressed bytes, handles the internal hardware-accelerated decoding, and opens an immediate, unbuffered SDL window mirroring Bob's screen.

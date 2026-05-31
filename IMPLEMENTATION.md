1. CODE ARCHITECTURE:
   - Create 'cmd/share/main.go' (Host/Bob): Accepts a '-port' flag. It must listen via 'quic-go' using a self-signed TLS configuration with 'InsecureSkipVerify: true'. Upon a successful connection, it must execute the following background ffmpeg process via 'os/exec':
     ffmpeg -f x11grab -video_size 1920x1080 -framerate 60 -i :0.0 -c:v libx264 -preset ultrafast -tune zerolatency -g 1 -f h264 pipe:1
     Stream the StdoutPipe() of ffmpeg directly into the open QUIC stream.
   
   - Create 'cmd/connect/main.go' (Peer/Alice): Accepts a target '[IP:PORT]' string as an argument. It must dial the host via 'quic-go'. Upon connection, it must execute the following local ffplay process via 'os/exec':
     ffplay -probesize 32 -analyze_duration 0 -flags low_delay -framedrop -i pipe:0
     Pipe the incoming QUIC stream reader directly into ffplay's StdinPipe().

2. COMPILATION & AUTOMATED TESTING ENVIRONMENT:
   - Compile both binaries: 'go build -o share cmd/share/main.go' and 'go build -o connect cmd/connect/main.go'.
   - Setup a simulated P2P environment locally inside this container using background process execution:
     a. Launch the host process in the background, redirecting output to logs: './share -port=50001 > host.log 2>&1 &'
     b. Sleep for 2 seconds to allow the listener socket to initialize.
     c. Launch the client process in the background, dialing the local loopback: './connect 127.0.0.1:50001 > client.log 2>&1 &'

3. VERIFICATION AND SELF-CORRECTION LOOP:
   - Monitor the background P2P run for 10 seconds. Check 'host.log' and 'client.log' for common errors (e.g., quic-go connection handshakes, ffmpeg X11 server attachment errors, or missing library bindings).
   - Verify that data packets are flowing through the network and being ingested by ffplay's stdin pipe.
   - If an error is detected, automatically apply structural code or execution parameter fixes, gracefully kill any orphaned background processes using 'pkill share' or 'pkill connect', and re-run the compilation and test pipeline until a clean, unblocked P2P stream state is confirmed."

4. WAYLAND DESKTOP PORTAL / D-BUS COUPLING:
   - Update 'cmd/share/main.go' to check if the active display server is Wayland (detecting via 'XDG_SESSION_TYPE=wayland').
   - If Wayland is detected, the Go runtime must make a D-Bus call to 'org.freedesktop.portal.Desktop' under the path '/org/freedesktop/portal/desktop' using the 'org.freedesktop.portal.ScreenCast' interface.
   - Implement the CreateSession, SelectSources, and Start method chain over D-Bus to toggle the native system window/screen selector UI.
   - Intercept the resulting PipeWire stream node ID returned by the portal handshake, and dynamically adapt the background stream engine to capture from the PipeWire context instead of falling back to '-f x11grab'.
   - Ensure 'godbus/dbus' or native command-line 'dbus-send' utilities are configured to cleanly handle this interface validation inside the test run.

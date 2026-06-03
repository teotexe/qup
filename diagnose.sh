#!/bin/bash
# AuraShare Pipeline Diagnostic Script
# Run this script on the actual machine to diagnose GStreamer/FFmpeg pipeline issues.
#
# Usage: bash diagnose.sh
#
set -e

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║           AuraShare Pipeline Diagnostic Tool                ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

# ── Step 1: Check tool availability ──
echo "═══ Step 1: Checking tool availability ═══"
for tool in gst-launch-1.0 gst-inspect-1.0 ffmpeg ffplay pw-cli; do
    if command -v "$tool" &>/dev/null; then
        echo "  ✓ $tool found: $(which $tool)"
    else
        echo "  ✗ $tool NOT FOUND"
    fi
done
echo ""

# ── Step 2: Check GStreamer plugins ──
echo "═══ Step 2: Checking GStreamer plugins ═══"
for plugin in pipewiresrc videoconvert videotestsrc y4menc fdsink queue; do
    if gst-inspect-1.0 "$plugin" &>/dev/null; then
        echo "  ✓ $plugin available"
    else
        echo "  ✗ $plugin NOT AVAILABLE"
    fi
done
echo ""

# ── Step 3: Check pipewiresrc properties ──
echo "═══ Step 3: pipewiresrc element properties ═══"
echo "  Looking for 'fd' property..."
if gst-inspect-1.0 pipewiresrc 2>/dev/null | grep -A2 "^\s*fd\s*:"; then
    echo "  ✓ fd property exists"
else
    echo "  ? fd property not found in standard output. Full property list:"
    gst-inspect-1.0 pipewiresrc 2>/dev/null | grep -E "^\s+\w+" | head -20
fi
echo ""

# ── Step 4: Test GStreamer videotestsrc → y4menc → stdout pipeline ──
echo "═══ Step 4: Testing GStreamer videotestsrc → y4menc pipeline ═══"
echo "  Running: gst-launch-1.0 -q videotestsrc num-buffers=5 ! video/x-raw,format=I420,width=320,height=240,framerate=5/1 ! y4menc ! fdsink fd=1 sync=false"
BYTES=$(gst-launch-1.0 -q videotestsrc num-buffers=5 is-live=false \
    ! "video/x-raw,format=I420,width=320,height=240,framerate=5/1" \
    ! y4menc \
    ! fdsink fd=1 sync=false 2>/dev/null | wc -c)
if [ "$BYTES" -gt 0 ]; then
    echo "  ✓ GStreamer Y4M output: $BYTES bytes (expected ~576,000+ for 5 frames)"
else
    echo "  ✗ GStreamer produced 0 bytes!"
fi
echo ""

# ── Step 5: Test GStreamer → FFmpeg chain ──
echo "═══ Step 5: Testing GStreamer → FFmpeg (Y4M → H.264) pipeline ═══"
echo "  Running chained pipeline..."
H264_BYTES=$(gst-launch-1.0 -q videotestsrc num-buffers=30 is-live=false \
    ! "video/x-raw,format=I420,width=320,height=240,framerate=30/1" \
    ! y4menc \
    ! fdsink fd=1 sync=false 2>/dev/null | \
    ffmpeg -f yuv4mpegpipe -i pipe:0 -c:v libx264 -preset ultrafast -tune zerolatency -g 30 -f h264 pipe:1 2>/dev/null | wc -c)
if [ "$H264_BYTES" -gt 0 ]; then
    echo "  ✓ GStreamer→FFmpeg chain produced $H264_BYTES bytes of H.264"
else
    echo "  ✗ Chain produced 0 bytes!"
fi
echo ""

# ── Step 6: Test with hardware encoder ──
echo "═══ Step 6: Testing hardware encoder availability ═══"
for enc in h264_qsv h264_vaapi h264_nvenc; do
    if ffmpeg -f lavfi -i "testsrc=duration=0.1" -c:v "$enc" -f null - 2>/dev/null; then
        echo "  ✓ $enc works!"
    else
        echo "  ✗ $enc not available or failed"
    fi
done
echo ""

# ── Step 7: Test GStreamer → FFmpeg with QSV encoder ──
echo "═══ Step 7: Testing GStreamer → FFmpeg QSV chain ═══"
QSV_BYTES=$(gst-launch-1.0 -q videotestsrc num-buffers=30 is-live=false \
    ! "video/x-raw,format=I420,width=320,height=240,framerate=30/1" \
    ! y4menc \
    ! fdsink fd=1 sync=false 2>/dev/null | \
    ffmpeg -f yuv4mpegpipe -i pipe:0 -c:v h264_qsv -preset veryfast -forced_idr 1 -g 30 -f h264 pipe:1 2>/dev/null | wc -c)
if [ "$QSV_BYTES" -gt 0 ]; then
    echo "  ✓ GStreamer→FFmpeg (QSV) chain produced $QSV_BYTES bytes of H.264"
else
    echo "  ✗ QSV chain produced 0 bytes! Trying with libx264 fallback..."
fi
echo ""

# ── Step 8: Test PipeWire connectivity ──
echo "═══ Step 8: Testing PipeWire connectivity ═══"
if command -v pw-cli &>/dev/null; then
    echo "  PipeWire nodes:"
    pw-cli ls Node 2>/dev/null | head -20 || echo "  (unable to list nodes)"
else
    echo "  pw-cli not found, skipping"
fi
echo ""

# ── Step 9: List PipeWire stream nodes ──
echo "═══ Step 9: Available PipeWire stream nodes ═══"
if command -v pw-dump &>/dev/null; then
    echo "  Video capture nodes:"
    pw-dump 2>/dev/null | python3 -c "
import json, sys
try:
    data = json.load(sys.stdin)
    for obj in data:
        if obj.get('type') == 'PipeWire:Interface:Node':
            info = obj.get('info', {})
            props = info.get('props', {})
            name = props.get('node.name', 'unknown')
            media_class = props.get('media.class', '')
            node_id = obj.get('id', '?')
            if 'Video' in media_class or 'Screen' in media_class or 'Camera' in media_class:
                print(f'  Node ID: {node_id} | Name: {name} | Class: {media_class}')
except:
    print('  (unable to parse pw-dump output)')
" 2>/dev/null || echo "  (unable to list stream nodes)"
else
    echo "  pw-dump not found, skipping"
fi
echo ""

echo "═══ Diagnostics Complete ═══"
echo "If Steps 4-5 pass but streaming still fails, the issue is in PipeWire capture."
echo "If Step 5 fails, the issue is in the GStreamer→FFmpeg pipe."
echo "Run './share --headless --debug --port=50001' and './connect --test 127.0.0.1:50001' to test without PipeWire."

package stats

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// StatsReporter tracks byte counts and periodically reports throughput metrics.
type StatsReporter struct {
	totalBytes int64
	lastBytes  int64
	lastTime   time.Time
	isSender   bool
	stopChan   chan struct{}
	stopOnce   sync.Once
}

// NewStatsReporter creates a new StatsReporter.
func NewStatsReporter(isSender bool) *StatsReporter {
	return &StatsReporter{
		lastTime: time.Now(),
		isSender: isSender,
		stopChan: make(chan struct{}),
	}
}

// Add updates the total bytes processed.
func (s *StatsReporter) Add(n int64) {
	atomic.AddInt64(&s.totalBytes, n)
}

// StartReporting begins logging throughput metrics to stdout at the specified interval.
func (s *StatsReporter) StartReporting(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				total := atomic.LoadInt64(&s.totalBytes)
				last := atomic.LoadInt64(&s.lastBytes)
				elapsed := now.Sub(s.lastTime)
				
				diff := total - last
				if diff < 0 {
					diff = 0
				}
				
				// Calculate Mbps
				mbps := (float64(diff) * 8) / (elapsed.Seconds() * 1024 * 1024)
				totalMB := float64(total) / (1024 * 1024)
				
				role := "Receiver"
				if s.isSender {
					role = "Sender"
				}
				
				fmt.Printf("[%s] Rate: %.2f Mbps | Total Transferred: %.2f MB\n", role, mbps, totalMB)
				
				// Update states
				atomic.StoreInt64(&s.lastBytes, total)
				s.lastTime = now
			case <-s.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

// Stop terminates the reporting goroutine. Safe to call multiple times.
func (s *StatsReporter) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopChan)
	})
}

// ProxyWriter wraps an io.Writer and tracks how many bytes are written.
type ProxyWriter struct {
	writer   io.Writer
	reporter *StatsReporter
}

// NewProxyWriter wraps an existing writer with stats tracking.
func NewProxyWriter(w io.Writer, r *StatsReporter) io.Writer {
	return &ProxyWriter{writer: w, reporter: r}
}

// Write writes data to the underlying writer and logs stats.
func (p *ProxyWriter) Write(b []byte) (int, error) {
	n, err := p.writer.Write(b)
	if n > 0 {
		p.reporter.Add(int64(n))
	}
	return n, err
}

// ProxyReader wraps an io.Reader and tracks how many bytes are read.
type ProxyReader struct {
	reader   io.Reader
	reporter *StatsReporter
}

// NewProxyReader wraps an existing reader with stats tracking.
func NewProxyReader(r io.Reader, rep *StatsReporter) io.Reader {
	return &ProxyReader{reader: r, reporter: rep}
}

// Read reads data from the underlying reader and logs stats.
func (p *ProxyReader) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	if n > 0 {
		p.reporter.Add(int64(n))
	}
	return n, err
}

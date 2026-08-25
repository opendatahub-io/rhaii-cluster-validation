package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// TCP latency test - custom Go implementation for measuring round-trip latency
//
// This tool replaces our previous dependency on netperf (the TCP_RR test).
// We built our own implementation for the following reasons:
//   1. Packaging: netperf requires EPEL repository, which cannot be included
//      in Red Hat production container images
//   2. Simplicity: we only need basic TCP echo latency measurement
//   3. Dependencies: a self-contained Go implementation eliminates external
//      runtime dependencies
//
// Implementation:
//   - Server: simple echo server listening on port 12865
//   - Client: sends 64-byte messages repeatedly for configured duration
//   - Output: mean latency in microseconds (stdout), JSON stats (stderr)
//
// This is not a replacement for comprehensive network benchmarking tools.
// For advanced testing, use dedicated tools like netperf, iperf3, or qperf.

const (
	defaultPort     = 12865
	defaultDuration = 5  // seconds
	messageSize     = 64 // bytes per request
)

func newTCPLatServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "tcp-lat-server",
		Short:  "Start TCP latency test server (echo server)",
		Hidden: true, // Internal use only
		RunE:   runTCPLatServer,
	}
	return cmd
}

func newTCPLatClientCmd() *cobra.Command {
	var (
		serverAddr string
		duration   int
	)

	cmd := &cobra.Command{
		Use:    "tcp-lat-client",
		Short:  "Run TCP latency test client",
		Hidden: true, // Internal use only
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTCPLatClient(serverAddr, duration)
		},
	}

	cmd.Flags().StringVar(&serverAddr, "server", "", "Server address (required)")
	cmd.Flags().IntVar(&duration, "duration", defaultDuration, "Test duration in seconds")
	cmd.MarkFlagRequired("server")

	return cmd
}

func runTCPLatServer(cmd *cobra.Command, args []string) error {
	addr := fmt.Sprintf(":%d", defaultPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	defer listener.Close()

	fmt.Fprintf(os.Stderr, "TCP latency server listening on %s\n", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Accept error: %v\n", err)
			continue
		}
		go handleEchoConn(conn)
	}
}

func handleEchoConn(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4096)

	for {
		// Set read deadline to prevent goroutine leak from idle clients
		if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to set read deadline: %v\n", err)
			return
		}

		n, err := conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "Read error: %v\n", err)
			}
			return
		}

		// Echo back exactly what was received
		_, err = conn.Write(buf[:n])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Write error: %v\n", err)
			return
		}
	}
}

func runTCPLatClient(serverAddr string, duration int) error {
	addr := net.JoinHostPort(serverAddr, strconv.Itoa(defaultPort))

	// Connect to server with timeout
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	defer conn.Close()

	// Run latency test
	endTime := time.Now().Add(time.Duration(duration) * time.Second)
	var latencies []float64
	message := make([]byte, messageSize)
	response := make([]byte, messageSize)

	for i := 0; i < messageSize; i++ {
		message[i] = byte('A' + (i % 26))
	}

	for time.Now().Before(endTime) {
		start := time.Now()

		// Set deadline for this iteration (5 seconds or remaining time, whichever is shorter)
		remaining := time.Until(endTime)
		deadline := 5 * time.Second
		if remaining < deadline {
			deadline = remaining
		}
		if err := conn.SetDeadline(time.Now().Add(deadline)); err != nil {
			return fmt.Errorf("failed to set deadline: %w", err)
		}

		// Send message
		_, err := conn.Write(message)
		if err != nil {
			return fmt.Errorf("write failed: %w", err)
		}

		// Read echo response
		_, err = io.ReadFull(conn, response)
		if err != nil {
			return fmt.Errorf("read failed: %w", err)
		}

		// Record latency in microseconds
		latency := time.Since(start).Microseconds()
		latencies = append(latencies, float64(latency))

		// Small sleep to avoid overwhelming the connection
		time.Sleep(1 * time.Millisecond)
	}

	if len(latencies) == 0 {
		return fmt.Errorf("no successful measurements")
	}

	// Calculate statistics
	sort.Float64s(latencies)
	mean := calculateMean(latencies)

	// Output mean latency to stdout (parser expects this on first line)
	fmt.Printf("%.2f\n", mean)

	// Calculate detailed stats (not currently output to avoid stdout/stderr ordering issues)
	// Could be used later for richer output formats (e.g., ConfigMap with full test results)
	_ = map[string]interface{}{
		"count":      len(latencies),
		"mean_us":    mean,
		"min_us":     latencies[0],
		"max_us":     latencies[len(latencies)-1],
		"p50_us":     percentile(latencies, 50),
		"p95_us":     percentile(latencies, 95),
		"p99_us":     percentile(latencies, 99),
		"duration_s": duration,
		"requests":   len(latencies),
	}

	return nil
}

func calculateMean(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(p) / 100.0 * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

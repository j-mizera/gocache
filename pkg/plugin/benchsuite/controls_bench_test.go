//go:build unix

package benchsuite

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Kernel floor validation (difference-of-differences):
//
//	Full RTT delta  = AFUnix_full_RTT - NetPipe_full_RTT  (from existing benchmarks)
//	Raw control delta = AFUnix_raw_RTT - Inproc_raw_RTT   (from THESE benchmarks)
//	True kernel floor attribution = Full delta - Raw delta
//
// If Full delta ≈ Raw delta, the "~9µs kernel floor" from PR #101 is validated.
// If Full delta > Raw delta, some of the "kernel floor" was actually GoCache dispatch overhead.
// If Full delta < Raw delta (should not happen), the measurement is confounded.
func BenchmarkRawSyscallPingPong_AFUnix(b *testing.B) {
	socketFDs, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if closeErr := unix.Close(socketFDs[0]); closeErr != nil {
			b.Errorf("close socket fd 0: %v", closeErr)
		}
		if closeErr := unix.Close(socketFDs[1]); closeErr != nil {
			b.Errorf("close socket fd 1: %v", closeErr)
		}
	})

	payload := make([]byte, 1)

	b.ResetTimer()
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		bytesWritten, writeErr := unix.Write(socketFDs[0], payload)
		if writeErr != nil {
			b.Fatal(writeErr)
		}
		if bytesWritten != len(payload) {
			b.Fatalf("write socket fd 0: wrote %d bytes, expected %d", bytesWritten, len(payload))
		}

		bytesRead, readErr := unix.Read(socketFDs[1], payload)
		if readErr != nil {
			b.Fatal(readErr)
		}
		if bytesRead != len(payload) {
			b.Fatalf("read socket fd 1: read %d bytes, expected %d", bytesRead, len(payload))
		}

		bytesWritten, writeErr = unix.Write(socketFDs[1], payload)
		if writeErr != nil {
			b.Fatal(writeErr)
		}
		if bytesWritten != len(payload) {
			b.Fatalf("write socket fd 1: wrote %d bytes, expected %d", bytesWritten, len(payload))
		}

		bytesRead, readErr = unix.Read(socketFDs[0], payload)
		if readErr != nil {
			b.Fatal(readErr)
		}
		if bytesRead != len(payload) {
			b.Fatalf("read socket fd 0: read %d bytes, expected %d", bytesRead, len(payload))
		}
	}
}

func BenchmarkRawSyscallPingPong_Inproc(b *testing.B) {
	pingSignal := make(chan struct{})
	pongSignal := make(chan struct{})

	go func() {
		for range pingSignal {
			pongSignal <- struct{}{}
		}
	}()

	b.ResetTimer()
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		pingSignal <- struct{}{}
		<-pongSignal
	}
	close(pingSignal)
}

package router

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	gcpc "gocache/api/gcpc/v1"
	"gocache/commons/transport"
)

// BenchmarkConcurrentCommandThroughput_Shared measures real-world contention
// when multiple goroutines dispatch commands through a single per-plugin
// PluginConn. The outbound channel and single writeLoop are the contention
// bottlenecks. The former sendMu mutex was removed by ADR-0038 (lock-free
// enqueue); concurrent senders now dispatch through the channel without
// serialization. This is the production model. GOMAXPROCS=1 plus
// SetParallelism(N) ensures exactly N goroutines, isolating goroutine-count
// effects from CPU-core scaling. ops/sec is b.N / b.Elapsed().Seconds(), where
// b.N is total operations across all goroutines per Go testing docs.
func BenchmarkConcurrentCommandThroughput_Shared(b *testing.B) {
	sockPath := filepath.Join(b.TempDir(), "bench-concurrent-afunix.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		b.Fatalf("Listen unix error: %v", err)
	}
	defer listener.Close()

	type unixDialOutcome struct {
		conn net.Conn
		err  error
	}
	unixDialCh := make(chan unixDialOutcome, 1)
	go func() {
		clientConn, dialErr := net.Dial("unix", sockPath)
		unixDialCh <- unixDialOutcome{clientConn, dialErr}
	}()
	server, err := listener.Accept()
	if err != nil {
		b.Fatalf("Accept error: %v", err)
	}
	defer server.Close()
	unixDial := <-unixDialCh
	if unixDial.err != nil {
		b.Fatalf("Dial error: %v", unixDial.err)
	}
	client := unixDial.conn
	defer client.Close()

	sharedPluginConn := NewPluginConn("bench-concurrent-shared", transport.NewConn(server))
	go sharedPluginConn.StartReadLoop()
	cancel := startMockPluginResponder(b, transport.NewConn(client))
	defer cancel()
	defer sharedPluginConn.Close()

	ctx := context.Background()
	cmd := &gcpc.CommandInfoV1{Name: "PING"}
	connectionInfo := &gcpc.ConnectionInfoV1{Id: "bench-concurrent"}
	for i := 0; i < 100; i++ {
		reqID := NextRequestID()
		requestEnvelope := gcpc.NewCommandRequest(reqID, cmd, connectionInfo, nil, nil)
		respCh, err := sharedPluginConn.Send(ctx, requestEnvelope, reqID)
		if err != nil {
			b.Fatalf("warmup Send error: %v", err)
		}
		<-respCh
	}

	origGOMAXPROCS := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(origGOMAXPROCS)

	goroutineCounts := []int{1, 2, 4, 8, 16, 32, 64}
	for _, goroutineCount := range goroutineCounts {
		b.Run(fmt.Sprintf("Goroutines_%d", goroutineCount), func(b *testing.B) {
			runtime.GOMAXPROCS(1)
			b.SetParallelism(goroutineCount)

			var requestIDCounter atomic.Uint64

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				ctx := context.Background()
				cmd := &gcpc.CommandInfoV1{Name: "PING"}
				connectionInfo := &gcpc.ConnectionInfoV1{Id: "bench-concurrent"}

				for pb.Next() {
					reqID := fmt.Sprintf("req-%d", requestIDCounter.Add(1))
					requestEnvelope := gcpc.NewCommandRequest(reqID, cmd, connectionInfo, nil, nil)
					respCh, err := sharedPluginConn.Send(ctx, requestEnvelope, reqID)
					if err != nil {
						panic(fmt.Sprintf("Send error: %v", err))
					}
					responseEnvelope, ok := <-respCh
					if !ok || responseEnvelope == nil {
						panic("Send response channel closed")
					}
				}
			})

			opsPerSec := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(opsPerSec, "ops/s")
		})
	}
}

// BenchmarkConcurrentCommandThroughput_PerGoroutine measures the no-contention
// parallelism ceiling. Each goroutine owns its own PluginConn pair, so there is
// no shared-resource contention. The delta between Shared and PerGoroutine is
// the lock plus fd-contention cost. GOMAXPROCS=1 plus SetParallelism(N) ensures
// exactly N goroutines, isolating goroutine-count effects from CPU-core scaling.
// ops/sec is b.N / b.Elapsed().Seconds(), where b.N is total operations across
// all goroutines per Go testing docs.
func BenchmarkConcurrentCommandThroughput_PerGoroutine(b *testing.B) {
	origGOMAXPROCS := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(origGOMAXPROCS)

	goroutineCounts := []int{1, 2, 4, 8, 16, 32, 64}
	for _, goroutineCount := range goroutineCounts {
		b.Run(fmt.Sprintf("Goroutines_%d", goroutineCount), func(b *testing.B) {
			runtime.GOMAXPROCS(1)
			b.SetParallelism(goroutineCount)

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				server, client := net.Pipe()
				pluginConn := NewPluginConn("bench-perg", transport.NewConn(server))
				go pluginConn.StartReadLoop()
				cancel := startMockPluginResponder(b, transport.NewConn(client))
				defer func() {
					pluginConn.Close()
					cancel()
					_ = server.Close()
					_ = client.Close()
				}()

				ctx := context.Background()
				cmd := &gcpc.CommandInfoV1{Name: "PING"}
				connectionInfo := &gcpc.ConnectionInfoV1{Id: "bench-perg"}
				for i := 0; i < 10; i++ {
					reqID := NextRequestID()
					requestEnvelope := gcpc.NewCommandRequest(reqID, cmd, connectionInfo, nil, nil)
					respCh, err := pluginConn.Send(ctx, requestEnvelope, reqID)
					if err != nil {
						panic(fmt.Sprintf("warmup Send error: %v", err))
					}
					responseEnvelope, ok := <-respCh
					if !ok || responseEnvelope == nil {
						panic("warmup Send response channel closed")
					}
				}

				var requestIDCounter atomic.Uint64
				for pb.Next() {
					reqID := fmt.Sprintf("req-%d", requestIDCounter.Add(1))
					requestEnvelope := gcpc.NewCommandRequest(reqID, cmd, connectionInfo, nil, nil)
					respCh, err := pluginConn.Send(ctx, requestEnvelope, reqID)
					if err != nil {
						panic(fmt.Sprintf("Send error: %v", err))
					}
					responseEnvelope, ok := <-respCh
					if !ok || responseEnvelope == nil {
						panic("Send response channel closed")
					}
				}
			})

			opsPerSec := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(opsPerSec, "ops/s")
		})
	}
}

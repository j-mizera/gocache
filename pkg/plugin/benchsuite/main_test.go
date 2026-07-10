package benchsuite

import (
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"
)

// TestMain keeps benchmark profiling opt-in: block and mutex profiling are
// disabled by default in the Go runtime, GOCACHE_BENCH_BLOCK_RATE and
// GOCACHE_BENCH_MUTEX_FRACTION enable them before benchmarks run, and normal
// runs keep zero profiling overhead.
func TestMain(m *testing.M) {
	blockRateValue := os.Getenv("GOCACHE_BENCH_BLOCK_RATE")
	if blockRateValue != "" {
		blockRate, err := strconv.Atoi(blockRateValue)
		if err != nil {
			log.Fatal(err)
		}
		if blockRate > 0 {
			runtime.SetBlockProfileRate(blockRate)
		}
	}

	mutexFractionValue := os.Getenv("GOCACHE_BENCH_MUTEX_FRACTION")
	if mutexFractionValue != "" {
		mutexFraction, err := strconv.Atoi(mutexFractionValue)
		if err != nil {
			log.Fatal(err)
		}
		if mutexFraction > 0 {
			runtime.SetMutexProfileFraction(mutexFraction)
		}
	}

	exitCode := m.Run()

	goroutineProfilePath := os.Getenv("GOCACHE_BENCH_GOROUTINE_PROFILE")
	if goroutineProfilePath != "" {
		goroutineProfileFile, err := os.Create(goroutineProfilePath)
		if err != nil {
			log.Printf("write goroutine profile: create %q: %v", goroutineProfilePath, err)
		} else {
			goroutineProfile := pprof.Lookup("goroutine")
			if goroutineProfile == nil {
				log.Printf("write goroutine profile: goroutine profile unavailable")
			} else if err := goroutineProfile.WriteTo(goroutineProfileFile, 0); err != nil {
				log.Printf("write goroutine profile: %v", err)
			}
			if err := goroutineProfileFile.Close(); err != nil {
				log.Printf("write goroutine profile: close %q: %v", goroutineProfilePath, err)
			}
		}
	}

	os.Exit(exitCode)
}

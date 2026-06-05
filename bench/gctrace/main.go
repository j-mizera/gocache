// gctrace fills the in-memory cache with a large mixed-shape workload and
// records runtime GC statistics before / after. It is intended to run under
// GODEBUG=gctrace=1 so the Go runtime's per-cycle mark-phase output can be
// collected on stderr alongside the program's summary on stdout.
//
// Usage:
//
//	GODEBUG=gctrace=1 go run ./bench/gctrace -keys=1000000 -mix=strings,hashes
//	GODEBUG=gctrace=1 go run ./bench/gctrace -keys=1000000 > gctrace.out 2> gctrace.err
//
// The program exits after forcing two GC cycles (one right after load, one
// after a brief live hold) so the resident heap is measured at a consistent
// point.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	commonobs "gocache/commons/observability"
	"gocache/pkg/cache"
)

type config struct {
	keys      int
	holdMs    int
	mix       string
	valueSize int
}

func main() {
	cfg := config{}
	flag.IntVar(&cfg.keys, "keys", 1_000_000, "total keys to load")
	flag.IntVar(&cfg.holdMs, "hold", 500, "milliseconds to hold the live set after load before the second GC")
	flag.StringVar(&cfg.mix, "mix", "strings", "workload mix: strings, hashes, mixed")
	flag.IntVar(&cfg.valueSize, "value", 64, "string value size in bytes (for strings + mixed)")
	flag.Parse()

	ctx := context.Background()
	c := cache.New()

	// Baseline memory before load.
	runtime.GC()
	var pre runtime.MemStats
	runtime.ReadMemStats(&pre)

	loadStart := time.Now()
	switch cfg.mix {
	case "strings":
		loadStrings(ctx, c, cfg.keys, cfg.valueSize)
	case "hashes":
		loadHashes(ctx, c, cfg.keys)
	case "mixed":
		half := cfg.keys / 2
		loadStrings(ctx, c, half, cfg.valueSize)
		loadHashes(ctx, c, cfg.keys-half)
	default:
		fmt.Fprintf(os.Stderr, "unknown mix: %s\n", cfg.mix)
		os.Exit(2)
	}
	loadDur := time.Since(loadStart)

	// First GC right after load.
	gcStart := time.Now()
	runtime.GC()
	gc1 := time.Since(gcStart)

	var afterLoad runtime.MemStats
	runtime.ReadMemStats(&afterLoad)

	// Hold the live set, then GC again — typical steady-state mark pass.
	time.Sleep(time.Duration(cfg.holdMs) * time.Millisecond)
	gcStart = time.Now()
	runtime.GC()
	gc2 := time.Since(gcStart)

	var afterHold runtime.MemStats
	runtime.ReadMemStats(&afterHold)

	fmt.Println("# gctrace summary")
	fmt.Printf("mix=%s keys=%d hold_ms=%d value_size=%d\n", cfg.mix, cfg.keys, cfg.holdMs, cfg.valueSize)
	fmt.Printf("load_duration=%s  load_rps=%.0f\n", loadDur, float64(cfg.keys)/loadDur.Seconds())
	fmt.Println()

	fmt.Println("## runtime.MemStats deltas (post-load - pre-load)")
	printDelta("HeapAlloc", pre.HeapAlloc, afterLoad.HeapAlloc)
	printDelta("HeapSys", pre.HeapSys, afterLoad.HeapSys)
	printDelta("HeapInuse", pre.HeapInuse, afterLoad.HeapInuse)
	printDelta("HeapObjects", pre.HeapObjects, afterLoad.HeapObjects)
	printDelta("StackInuse", pre.StackInuse, afterLoad.StackInuse)
	printDelta("Mallocs", pre.Mallocs, afterLoad.Mallocs)
	printDelta("Frees", pre.Frees, afterLoad.Frees)
	printDelta("NumGC", uint64(pre.NumGC), uint64(afterLoad.NumGC))
	fmt.Println()

	fmt.Println("## GC timing")
	fmt.Printf("gc1_after_load=%s\n", gc1)
	fmt.Printf("gc2_after_hold=%s\n", gc2)
	fmt.Printf("PauseTotalNs=%d (%.2f ms)\n", afterHold.PauseTotalNs, float64(afterHold.PauseTotalNs)/1e6)
	fmt.Printf("NumGC_total=%d\n", afterHold.NumGC)
	if afterHold.NumGC > 0 {
		// Recent pause distribution. PauseNs is a circular buffer of 256
		// most recent pauses. Report mean and max over the full run.
		var sum, max uint64
		n := uint64(afterHold.NumGC)
		if n > 256 {
			n = 256
		}
		for i := uint64(0); i < n; i++ {
			p := afterHold.PauseNs[i]
			sum += p
			if p > max {
				max = p
			}
		}
		fmt.Printf("pause_mean_ns=%.0f pause_max_ns=%d pause_max_ms=%.3f\n",
			float64(sum)/float64(n), max, float64(max)/1e6)
	}
	fmt.Println()

	fmt.Println("## heap snapshot (post-hold + forced GC)")
	fmt.Printf("HeapAlloc=%s  HeapSys=%s  HeapObjects=%d\n",
		humanBytes(afterHold.HeapAlloc), humanBytes(afterHold.HeapSys), afterHold.HeapObjects)
	fmt.Printf("cache.Len=%d  cache.UsedBytes=%s  cache.MaxBytes=%d\n",
		c.Len(), humanBytes(uint64(c.UsedBytes())), c.MaxBytes())

	// Slab accounting, if available.
	c.RLock()
	stats := c.SlabStats()
	c.RUnlock()
	fmt.Println()
	fmt.Println("## slab allocator")
	fmt.Printf("allocs=%d frees=%d live=%d\n", stats.TotalAllocs, stats.TotalFrees, stats.LiveEntries)
	fmt.Printf("capacity=%s allocated=%s\n",
		humanBytes(uint64(stats.CapacityBytes)), humanBytes(uint64(stats.AllocatedBytes)))
	fmt.Printf("huge_count=%d huge_bytes=%s\n", stats.HugeCount, humanBytes(uint64(stats.HugeBytes)))
	nonEmpty := make([]string, 0, 11)
	for i, inUse := range stats.PerClassInUse {
		if inUse > 0 {
			nonEmpty = append(nonEmpty, fmt.Sprintf("c%d=%d(%d slabs)", i, inUse, stats.PerClassSlabs[i]))
		}
	}
	if len(nonEmpty) > 0 {
		fmt.Printf("per_class: %s\n", strings.Join(nonEmpty, " "))
	}
}

func loadStrings(ctx context.Context, c *cache.Cache, n, valueSize int) {
	value := make([]byte, valueSize)
	for i := 0; i < valueSize; i++ {
		value[i] = byte('a' + (i % 26))
	}
	c.Lock()
	defer c.Unlock()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("s:%08d", i)
		if err := c.RawSet(commonobs.OperationScope{}, key, value, 0); err != nil {
			fmt.Fprintf(os.Stderr, "RawSet failed at i=%d: %v\n", i, err)
			os.Exit(1)
		}
	}
}

func loadHashes(ctx context.Context, c *cache.Cache, n int) {
	c.Lock()
	defer c.Unlock()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("h:%08d", i)
		h := map[string]string{
			"field1": "value1",
			"field2": "value2",
			"field3": fmt.Sprintf("v-%d", i),
		}
		if err := c.RawSet(commonobs.OperationScope{}, key, h, 0); err != nil {
			fmt.Fprintf(os.Stderr, "RawSet failed at i=%d: %v\n", i, err)
			os.Exit(1)
		}
	}
}

func printDelta(label string, before, after uint64) {
	delta := int64(after) - int64(before)
	sign := "+"
	if delta < 0 {
		sign = ""
	}
	fmt.Printf("%-12s %s -> %s  (%s%d)\n", label,
		humanBytes(before), humanBytes(after), sign, delta)
}

func humanBytes(n uint64) string {
	const (
		_  = iota
		KB = 1 << (10 * iota)
		MB
		GB
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

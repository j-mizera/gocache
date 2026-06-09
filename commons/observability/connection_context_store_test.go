package observability

import (
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	apiobs "gocache/api/observability"
)

func TestConnectionContextStoreConcurrentPinVisitRelease(t *testing.T) {
	var store connectionContextStore
	store.init()

	connection := apiobs.ConnectionIdentity(101)
	if version := store.updateStrings(connection, []string{"epoch", "0", "mirror", "0", "stable", "yes"}); version.IsZero() {
		t.Fatal("initial context version should be non-zero")
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	failures := make(chan string, 1)
	report := func(message string) {
		select {
		case failures <- message:
		default:
		}
		stop.Store(true)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 3000 && !stop.Load(); i++ {
			epoch := strconv.Itoa(i)
			version := store.updateStrings(connection, []string{
				"epoch", epoch,
				"mirror", epoch,
				"stable", "yes",
				"drop", epoch,
			})
			if version.IsZero() {
				report("mutation produced zero context version")
				return
			}
			if i%5 == 0 {
				store.removeStrings(connection, []string{"drop"})
			}
			if i%17 == 0 {
				store.reclaim()
			}
			runtime.Gosched()
		}
		stop.Store(true)
	}()

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				version := store.pinCurrent(connection)
				if version.IsZero() {
					runtime.Gosched()
					continue
				}

				pairs := make(map[string]string)
				visited := store.visit(version, func(key, value string) bool {
					pairs[key] = value
					return true
				})
				if !visited {
					report("pinned context version was reclaimed before visit")
				}
				if pairs["stable"] != "yes" {
					report("visited pinned context with unexpected stable field")
				}
				if pairs["epoch"] == "" || pairs["epoch"] != pairs["mirror"] {
					report("visited pinned context with torn epoch fields")
				}
				if !store.release(version) {
					report("release of pinned context version failed")
				}
				runtime.Gosched()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			store.reclaim()
			runtime.Gosched()
		}
	}()

	wg.Wait()
	select {
	case failure := <-failures:
		t.Fatal(failure)
	default:
	}
}

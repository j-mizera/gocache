package server

import (
	"strconv"
	"sync"
	"testing"
)

func TestIT_ConcurrentClients(t *testing.T) {
	_, addr := startTestServer(t, "")

	const clients = 10
	const opsPerClient = 50

	var wg sync.WaitGroup
	wg.Add(clients)

	for i := range clients {
		go func(id int) {
			defer wg.Done()
			conn := dial(t, addr)
			defer conn.Close()

			prefix := "c" + strconv.Itoa(id) + ":"
			for j := range opsPerClient {
				key := prefix + strconv.Itoa(j)
				val := "v" + strconv.Itoa(j)

				v := sendCommand(t, conn, "SET", key, val)
				if v.Str != "OK" {
					t.Errorf("client %d SET %s: got %q", id, key, v.Str)
					return
				}
				v = sendCommand(t, conn, "GET", key)
				if v.Str != val {
					t.Errorf("client %d GET %s: expected %q, got %q", id, key, val, v.Str)
					return
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestIT_SharedKeyContention(t *testing.T) {
	_, addr := startTestServer(t, "")

	assertOK(t, sendCommand(t, dial(t, addr), "SET", "counter", "0"))

	const goroutines = 5
	const incrs = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			conn := dial(t, addr)
			defer conn.Close()

			for range incrs {
				sendCommand(t, conn, "INCR", "counter")
			}
		}()
	}

	wg.Wait()

	conn := dial(t, addr)
	defer conn.Close()
	v := sendCommand(t, conn, "GET", "counter")
	expected := strconv.Itoa(goroutines * incrs)
	if v.Str != expected {
		t.Errorf("counter: expected %s, got %s", expected, v.Str)
	}
}

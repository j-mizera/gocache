package ophooks

import (
	"testing"

	ops "gocache/api/operations"
	"gocache/api/transport"
	"gocache/pkg/plugin/router"
	"net"
)

func testPipe() (*transport.Conn, *transport.Conn) {
	server, client := net.Pipe()
	return transport.NewConn(server), transport.NewConn(client)
}

func TestRegistry_RegisterAndMatch(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("gobservability", s)
	defer pc.Close()

	reg.Register("gobservability", 10, pc, []string{"*"})

	if !reg.HasAny() {
		t.Error("expected HasAny=true")
	}

	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].PluginName != "gobservability" {
		t.Errorf("expected gobservability, got %s", matches[0].PluginName)
	}
}

func TestRegistry_MatchByType(t *testing.T) {
	reg := NewRegistry()
	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()
	s2, c2 := testPipe()
	defer c2.Close()
	defer s2.Close()

	pc1 := router.NewPluginConn("cmdonly", s1)
	defer pc1.Close()
	pc2 := router.NewPluginConn("all", s2)
	defer pc2.Close()

	reg.Register("cmdonly", 5, pc1, []string{"command"})
	reg.Register("all", 10, pc2, []string{"*"})

	// Command matches both
	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches for command, got %d", len(matches))
	}

	// Cleanup matches only wildcard
	matches = reg.Match(ops.TypeCleanup)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match for cleanup, got %d", len(matches))
	}
	if matches[0].PluginName != "all" {
		t.Errorf("expected 'all', got %s", matches[0].PluginName)
	}
}

func TestRegistry_PriorityOrder(t *testing.T) {
	reg := NewRegistry()
	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()
	s2, c2 := testPipe()
	defer c2.Close()
	defer s2.Close()
	s3, c3 := testPipe()
	defer c3.Close()
	defer s3.Close()

	pc1 := router.NewPluginConn("low", s1)
	defer pc1.Close()
	pc2 := router.NewPluginConn("high", s2)
	defer pc2.Close()
	pc3 := router.NewPluginConn("mid", s3)
	defer pc3.Close()

	reg.Register("low", 100, pc1, []string{"*"})
	reg.Register("high", 1, pc2, []string{"*"})
	reg.Register("mid", 50, pc3, []string{"*"})

	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 3 {
		t.Fatalf("expected 3, got %d", len(matches))
	}
	if matches[0].PluginName != "high" || matches[1].PluginName != "mid" || matches[2].PluginName != "low" {
		t.Errorf("expected priority order high,mid,low — got %s,%s,%s",
			matches[0].PluginName, matches[1].PluginName, matches[2].PluginName)
	}
}

func TestRegistry_Unregister(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("test", s)
	defer pc.Close()

	reg.Register("test", 10, pc, []string{"*"})
	if !reg.HasAny() {
		t.Fatal("expected hooks registered")
	}

	reg.Unregister("test")
	if reg.HasAny() {
		t.Error("expected no hooks after unregister")
	}
}

func TestRegistry_CaseInsensitive(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("test", s)
	defer pc.Close()

	reg.Register("test", 10, pc, []string{"Command"})

	// Match should be case-insensitive
	matches := reg.Match(ops.TypeCommand) // "command"
	if len(matches) != 1 {
		t.Errorf("expected case-insensitive match, got %d", len(matches))
	}
}

func TestRegistry_NoMatch(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("test", s)
	defer pc.Close()

	reg.Register("test", 10, pc, []string{"snapshot"})

	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 0 {
		t.Errorf("expected no matches, got %d", len(matches))
	}
}

func TestRegistry_Empty(t *testing.T) {
	reg := NewRegistry()
	if reg.HasAny() {
		t.Error("expected empty registry")
	}
	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 0 {
		t.Error("expected no matches from empty registry")
	}
}

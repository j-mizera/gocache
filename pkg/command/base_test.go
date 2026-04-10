package command_test

import (
	"testing"

	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
)

// TestSetup verifies the base test helpers compile and work.
func TestSetup(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })
	ctx := clientctx.New()

	if c == nil || e == nil || ctx == nil {
		t.Fatal("setup returned nil")
	}
}

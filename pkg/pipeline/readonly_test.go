package pipeline

import (
	"sort"
	"strings"
	"testing"

	"gocache/pkg/resp"
	resphandler "gocache/pkg/resp/handler"
)

// TestSpec_ReadOnly_Classification pins down which commands are routed
// through the engine-bypass read-lock path. The risk is mis-classification
// in two directions:
//
//  1. A mutating command marked ReadOnly: handler runs under cache.RLock,
//     which permits other readers concurrently — its mutation could be
//     observed mid-write by a parallel reader, or could race with another
//     concurrent mutation. -race would catch this for cache-state mutators
//     but not for client-state mutators (HELLO, SELECT, AUTH, WATCH).
//
//  2. A pure-read command marked NOT ReadOnly: takes the engine path and
//     pays the channel hop, but stays correct. The cost of getting it
//     wrong is throughput, not correctness.
//
// New handlers added to register.go must update the expected map below.
// The test fails noisily on either drift direction.
func TestSpec_ReadOnly_Classification(t *testing.T) {
	// expected lists every RESP command and asserts the ReadOnly bit.
	// Order matches register.go.
	expected := map[string]bool{
		// String / KV
		resp.CmdSet:     false,
		resp.CmdGet:     true,
		resp.CmdDelete:  false,
		resp.CmdExists:  true,
		resp.CmdExpire:  false,
		resp.CmdPExpire: false,
		resp.CmdTTL:     true,
		resp.CmdPTTL:    true,
		resp.CmdSetNX:   false,

		// List
		resp.CmdLPush:  false,
		resp.CmdRPush:  false,
		resp.CmdLPop:   false,
		resp.CmdRPop:   false,
		resp.CmdLLen:   true,
		resp.CmdLRange: true,
		resp.CmdBLPop:  false,
		resp.CmdBRPop:  false,

		// Hash
		resp.CmdHSet:    false,
		resp.CmdHGet:    true,
		resp.CmdHDel:    false,
		resp.CmdHExists: true,
		resp.CmdHGetAll: true,
		resp.CmdHKeys:   true,
		resp.CmdHVals:   true,
		resp.CmdHLen:    true,

		// Set
		resp.CmdSAdd:      false,
		resp.CmdSRem:      false,
		resp.CmdSMembers:  true,
		resp.CmdSIsMember: true,
		resp.CmdSCard:     true,
		resp.CmdSPop:      false,
		resp.CmdSInter:    true,
		resp.CmdSUnion:    true,
		resp.CmdSDiff:     true,

		// Sorted set
		resp.CmdZAdd:   false,
		resp.CmdZRem:   false,
		resp.CmdZScore: true,
		resp.CmdZCard:  true,
		resp.CmdZRange: true,
		resp.CmdZRank:  true,
		resp.CmdZCount: true,

		// Transactions / persistence
		resp.CmdMulti:    false,
		resp.CmdDiscard:  false,
		resp.CmdExec:     false,
		resp.CmdSnapshot: false,
		resp.CmdSave:     false,
		resp.CmdBgsave:   false,
		resp.CmdLastsave: true,

		// Server
		resp.CmdDBSize:   true,
		resp.CmdInfo:     true,
		resp.CmdHello:    false, // mutates client state
		resp.CmdPing:     true,
		resp.CmdEcho:     true,
		resp.CmdSelect:   false,
		resp.CmdFlushDB:  false,
		resp.CmdFlushAll: false,
		resp.CmdAuth:     false,

		// Counters
		resp.CmdIncr:        false,
		resp.CmdDecr:        false,
		resp.CmdIncrBy:      false,
		resp.CmdDecrBy:      false,
		resp.CmdIncrByFloat: false,
		resp.CmdAppend:      false,
		resp.CmdStrlen:      true,

		// Multi-key
		resp.CmdMGet: true,
		resp.CmdMSet: false,

		// Key management
		resp.CmdType:      true,
		resp.CmdRename:    false,
		resp.CmdRenameNX:  false,
		resp.CmdKeys:      true,
		resp.CmdScan:      true,
		resp.CmdRandomKey: true,

		// Watch — mutates per-client watcher state
		resp.CmdWatch:   false,
		resp.CmdUnwatch: false,

		// Object introspection
		resp.CmdObject: true,
	}

	regs := resphandler.Registrations()

	// Drift check: every registered command must appear in the expected map.
	var missing []string
	for name := range regs {
		if _, ok := expected[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("commands registered but missing from ReadOnly classification table: %s",
			strings.Join(missing, ", "))
	}

	// Drift check: every expected entry must be a registered command.
	for name := range expected {
		if _, ok := regs[name]; !ok {
			t.Errorf("expected entry %q is not a registered command (rename or removal?)", name)
		}
	}

	// Match check: each command's Spec.ReadOnly matches the expected bit.
	names := make([]string, 0, len(expected))
	for n := range expected {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		want := expected[name]
		got := regs[name].Spec.ReadOnly
		if got != want {
			t.Errorf("Spec(%q).ReadOnly = %v, want %v", name, got, want)
		}
	}
}

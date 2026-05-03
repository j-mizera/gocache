package workers

import (
	"context"
	"sync"
	"time"

	"gocache/api/command"
	"gocache/api/events"
	"gocache/api/logger"
	ops "gocache/api/operations"
	"gocache/pkg/cache"
	pkgcommand "gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/evaluator"
	serverOps "gocache/pkg/operations"
)

const defaultInterval = 5 * time.Minute

type Worker interface {
	// Start begins the worker's ticker loop. parentCtx is the scheduler/server
	// lifecycle context; operations created each tick derive from it so
	// cancellation propagates.
	Start(parentCtx context.Context)
	Stop()
	UpdateInterval(d time.Duration)
}

type baseWorker struct {
	cache          *cache.Cache
	engine         *engine.Engine
	interval       time.Duration
	stopChan       chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	intervalChan   chan time.Duration
	tracker        *serverOps.Tracker
	emitter        events.Emitter
	opHookExecutor evaluator.OpHookExecutor
}

// SetTracker sets the operation tracker.
func (w *baseWorker) SetTracker(t *serverOps.Tracker) { w.tracker = t }

// SetEmitter sets the event emitter.
func (w *baseWorker) SetEmitter(e events.Emitter) { w.emitter = e }

// SetOpHookExecutor sets the operation hook executor.
func (w *baseWorker) SetOpHookExecutor(e evaluator.OpHookExecutor) { w.opHookExecutor = e }

// startOp creates an operation if tracker is set, runs start hooks, emits start
// event. Returns (op, ctx) where ctx is derived from parentCtx and carries op
// for log correlation downstream.
func (w *baseWorker) startOp(parentCtx context.Context, opType ops.Type) (*ops.Operation, context.Context) {
	if w.tracker == nil {
		return nil, parentCtx
	}
	op := w.tracker.Start(opType, "")
	op.Enrich(command.TriggerKey, "scheduled")
	opCtx := ops.WithContext(parentCtx, op)
	if w.opHookExecutor != nil && w.opHookExecutor.HasAny() {
		w.opHookExecutor.RunStartHooks(opCtx, op)
	}
	if w.emitter != nil {
		w.emitter.Emit(events.NewOperationStart(op.ID, string(op.Type), "", op.ContextSnapshot(false)))
	}
	return op, opCtx
}

// completeOp marks an operation as completed, runs complete hooks, emits events.
func (w *baseWorker) completeOp(op *ops.Operation) {
	if op == nil {
		return
	}
	op.Complete()
	if w.opHookExecutor != nil {
		w.opHookExecutor.RunCompleteHooks(op)
	}
	if w.emitter != nil {
		w.emitter.Emit(events.NewOperationComplete(op.ID, string(op.Type), uint64(op.Duration().Nanoseconds()), "completed", "", op.ContextSnapshot(false)))
	}
	w.tracker.Complete(op.ID)
}

// failOp marks an operation as failed.
func (w *baseWorker) failOp(op *ops.Operation, reason string) {
	if op == nil {
		return
	}
	op.Fail(reason)
	if w.opHookExecutor != nil {
		w.opHookExecutor.RunCompleteHooks(op)
	}
	if w.emitter != nil {
		w.emitter.Emit(events.NewOperationComplete(op.ID, string(op.Type), uint64(op.Duration().Nanoseconds()), "failed", reason, op.ContextSnapshot(false)))
	}
	w.tracker.Fail(op.ID, reason)
}

// Stop signals the worker to stop and waits for its goroutine to exit.
// After Stop returns it is safe to run operations that would otherwise
// race with the worker (e.g. a final snapshot on shutdown).
func (w *baseWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
	w.wg.Wait()
}

func (w *baseWorker) UpdateInterval(d time.Duration) {
	w.intervalChan <- d
}

func safeInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultInterval
	}
	return d
}

// SnapshotWorker periodically saves a snapshot of the cache to disk.
type SnapshotWorker struct {
	baseWorker
	// file is retained for op-enrichment and log correlation only — the
	// snapshotter owns the durable path. Hot-reload updates both via
	// UpdateFile (worker copy) + the wired SnapshotInvoker (the actual
	// path the on-disk write hits).
	file        string
	fileChan    chan string
	snapshotter pkgcommand.SnapshotInvoker
}

func NewSnapshotWorker(c *cache.Cache, e *engine.Engine, interval time.Duration, file string) *SnapshotWorker {
	return &SnapshotWorker{
		baseWorker: baseWorker{
			cache:        c,
			engine:       e,
			interval:     safeInterval(interval),
			stopChan:     make(chan struct{}),
			intervalChan: make(chan time.Duration, 1),
		},
		file:     file,
		fileChan: make(chan string, 1),
	}
}

// SetSnapshotInvoker wires the persistence coordinator's SAVE entry point
// into the worker. Each tick calls Snapshot through this invoker. Pass
// nil to disable scheduled saves (operation is failed with a clear
// reason — never a silent no-op).
func (w *SnapshotWorker) SetSnapshotInvoker(s pkgcommand.SnapshotInvoker) {
	w.snapshotter = s
}

// UpdateFile updates the snapshot file path at runtime. The worker copy
// is used only for log/op enrichment — the actual durable path lives on
// the registered Snapshotter and must be updated independently (config
// reload calls both in main.go).
func (w *SnapshotWorker) UpdateFile(file string) {
	w.fileChan <- file
}

func (w *SnapshotWorker) Start(parentCtx context.Context) {
	ticker := time.NewTicker(w.interval)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-ticker.C:
				file := w.file
				op, opCtx := w.startOp(parentCtx, ops.TypeSnapshot)
				if op != nil {
					op.Enrich(command.FileKey, file)
				}
				if w.snapshotter == nil {
					logger.Warn(opCtx).Msg("snapshot scheduled but no snapshotter registered")
					w.failOp(op, "no snapshotter registered")
					continue
				}
				if err := w.engine.Dispatch(opCtx, func() {
					if err := w.snapshotter.Snapshot(opCtx, w.cache); err != nil {
						logger.Warn(opCtx).Err(err).Msg("snapshot save failed")
						w.failOp(op, err.Error())
					} else {
						logger.Debug(opCtx).Str("file", file).Msg("snapshot saved")
						w.completeOp(op)
					}
				}); err != nil {
					logger.Warn(opCtx).Err(err).Msg("snapshot dispatch failed")
					w.failOp(op, err.Error())
				}
			case d := <-w.intervalChan:
				ticker.Reset(safeInterval(d))
			case f := <-w.fileChan:
				w.file = f
			case <-w.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

// CleanupWorker periodically removes expired keys from the cache.
type CleanupWorker struct {
	baseWorker
	feed pkgcommand.MutationEmitter
}

func NewCleanupWorker(c *cache.Cache, e *engine.Engine, interval time.Duration) *CleanupWorker {
	return &CleanupWorker{
		baseWorker: baseWorker{
			cache:        c,
			engine:       e,
			interval:     safeInterval(interval),
			stopChan:     make(chan struct{}),
			intervalChan: make(chan time.Duration, 1),
		},
	}
}

// SetPersistenceFeed wires the persistence coordinator's mutation-feed
// hook so TTL-driven deletes emit DEL mutations under the same lock that
// performs the deletion. Replay parity with snapshot-time state requires
// these emissions; without them, a recovered cache could diverge from a
// snapshot+log replay because expired entries would not appear as deletes
// in the mutation log.
func (w *CleanupWorker) SetPersistenceFeed(f pkgcommand.MutationEmitter) {
	w.feed = f
}

func (w *CleanupWorker) Start(parentCtx context.Context) {
	ticker := time.NewTicker(w.interval)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-ticker.C:
				op, opCtx := w.startOp(parentCtx, ops.TypeCleanup)
				if err := w.engine.Dispatch(opCtx, func() {
					now := time.Now().UnixNano()
					emit := w.feed != nil && w.feed.HasSinks()
					w.cache.Range(func(key string, _ cache.Entry, expiration int64) bool {
						if expiration > 0 && now > expiration {
							w.cache.RawDelete(key)
							if emit {
								// Inside engine.Dispatch's bulk lock — LSN
								// allocation order matches deletion order.
								w.feed.AllocateAndEmit("DEL", key, [][]byte{[]byte(key)})
							}
						}
						return true
					})
					logger.Debug(opCtx).Msg("cleanup sweep completed")
					w.completeOp(op)
				}); err != nil {
					logger.Warn(opCtx).Err(err).Msg("cleanup dispatch failed")
					w.failOp(op, err.Error())
				}
			case d := <-w.intervalChan:
				ticker.Reset(safeInterval(d))
			case <-w.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

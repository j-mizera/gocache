package workers

import (
	"context"
	"sync"
	"time"

	"gocache/api/events"
	"gocache/api/logger"
	ops "gocache/api/operations"
	"gocache/pkg/cache"
	"gocache/pkg/engine"
	"gocache/pkg/evaluator"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/persistence"
)

const defaultInterval = 5 * time.Minute

type Worker interface {
	Start()
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

// startOp creates an operation if tracker is set, runs start hooks, emits start event.
func (w *baseWorker) startOp(opType ops.Type) *ops.Operation {
	if w.tracker == nil {
		return nil
	}
	op := w.tracker.Start(opType, "")
	op.Enrich("_trigger", "scheduled")
	if w.opHookExecutor != nil && w.opHookExecutor.HasAny() {
		w.opHookExecutor.RunStartHooks(context.Background(), op)
	}
	if w.emitter != nil {
		w.emitter.Emit(events.NewOperationStart(op.ID, string(op.Type), "", op.ContextSnapshot(false)))
	}
	return op
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
	file     string
	fileChan chan string
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

// UpdateFile updates the snapshot file path at runtime.
func (w *SnapshotWorker) UpdateFile(file string) {
	w.fileChan <- file
}

func (w *SnapshotWorker) Start() {
	ticker := time.NewTicker(w.interval)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-ticker.C:
				file := w.file
				op := w.startOp(ops.TypeSnapshot)
				if op != nil {
					op.Enrich("_file", file)
				}
				w.engine.Dispatch(func() {
					if err := persistence.SaveSnapshot(file, w.cache); err != nil {
						logger.Warn().Err(err).Msg("snapshot save failed")
						w.failOp(op, err.Error())
					} else {
						logger.Debug().Str("file", file).Msg("snapshot saved")
						w.completeOp(op)
					}
				})
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

func (w *CleanupWorker) Start() {
	ticker := time.NewTicker(w.interval)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-ticker.C:
				op := w.startOp(ops.TypeCleanup)
				w.engine.Dispatch(func() {
					now := time.Now().UnixNano()
					w.cache.Range(func(key string, entry *cache.Entry, expiration int64) bool {
						if expiration > 0 && now > expiration {
							w.cache.RawDelete(key)
						}
						return true
					})
					logger.Debug().Msg("cleanup sweep completed")
					w.completeOp(op)
				})
			case d := <-w.intervalChan:
				ticker.Reset(safeInterval(d))
			case <-w.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

package workers

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gocache/api/command"
	apiobs "gocache/api/observability"
	ops "gocache/api/operations"
	apipersistence "gocache/api/persistence"
	commonobs "gocache/commons/observability"
	"gocache/pkg/cache"
	pkgcommand "gocache/pkg/command"
	"gocache/pkg/engine"
)

const defaultInterval = 5 * time.Minute

const workerOperationIdentityBase apiobs.InternalOperationIdentity = 1 << 58

var workerOperationSequence atomic.Uint64

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
	operationScope *commonobs.SlotOperationTrackerManager
}

// SetOperationTrackerManager sets the sidecar tracker used for worker telemetry.
func (w *baseWorker) SetOperationTrackerManager(manager *commonobs.SlotOperationTrackerManager) {
	w.operationScope = manager
}

type workerOperation struct {
	scope  commonobs.OperationScope
	ctx    context.Context
	opType string
	start  time.Time
}

// startOp creates a sidecar operation. The returned context is the parent
// lifecycle context only; telemetry correlation is explicit through scope.
func (w *baseWorker) startOp(parentCtx context.Context, opType ops.Type) workerOperation {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	started := time.Now()
	manager := w.operationScope
	if manager == nil {
		return workerOperation{ctx: parentCtx, opType: string(opType), start: started}
	}
	sequence := workerOperationSequence.Add(1)
	if sequence == 0 {
		sequence = workerOperationSequence.Add(1)
	}
	operation := workerOperationIdentityBase + apiobs.InternalOperationIdentity(sequence)
	opTypeString := string(opType)
	operationID := opTypeString + ":" + strconv.FormatUint(sequence, 10)
	ref := apiobs.NewOperationRef(operationID, "")
	handle, ok := manager.StartOperationWithMetadata(operation, apiobs.ParentRef{}, 0, commonobs.OperationSnapshotMetadata{
		Type:          opTypeString,
		Ref:           ref,
		StartUnixNano: started.UnixNano(),
	}, nil)
	if !ok {
		return workerOperation{ctx: parentCtx, opType: opTypeString, start: started}
	}
	scope := commonobs.NewOperationScope(manager, handle, operation, ref)
	scope.ContextUpdateStrings(command.OperationID, operationID, command.TriggerKey, "scheduled")
	scope.OperationStartString(opTypeString,
		command.OperationID, operationID,
		command.TriggerKey, "scheduled",
		"_operation_type", opTypeString,
	)
	return workerOperation{scope: scope, ctx: parentCtx, opType: opTypeString, start: started}
}

// completeOp marks a sidecar operation as completed.
func (w *baseWorker) completeOp(op workerOperation) {
	if op.scope.IsZero() {
		return
	}
	elapsedNs := uint64(time.Since(op.start).Nanoseconds())
	op.scope.OperationFinishString(op.opType, elapsedNs,
		command.OperationID, op.scope.Ref().ID.String(),
		"_operation_type", op.opType,
		"_status", "completed",
		command.ElapsedNs, strconv.FormatUint(elapsedNs, 10),
	)
	op.scope.Finish(commonobs.SlotTerminalFinished)
}

// failOp marks a sidecar operation as failed.
func (w *baseWorker) failOp(op workerOperation, reason string) {
	if op.scope.IsZero() {
		return
	}
	elapsedNs := uint64(time.Since(op.start).Nanoseconds())
	op.scope.OperationFinishString(op.opType, elapsedNs,
		command.OperationID, op.scope.Ref().ID.String(),
		"_operation_type", op.opType,
		"_status", "failed",
		command.ElapsedNs, strconv.FormatUint(elapsedNs, 10),
		command.ErrorKey, reason,
	)
	op.scope.Finish(commonobs.SlotTerminalFailed)
}

func (w *baseWorker) logOp(op workerOperation, level apiobs.TelemetryLogLevel, message string, err error) {
	if op.scope.IsZero() {
		return
	}
	record := apiobs.NewLogRecordString(op.scope.Operation(), level, message)
	record.TimestampUnixNano = time.Now().UnixNano()
	if err != nil {
		record.AddFieldString("error", err.Error())
	}
	op.scope.Record(record)
}

// Stop signals the worker to stop and waits for its goroutine to exit.
// After Stop returns it is safe to run operations that would otherwise
// race with the worker (e.g. a final snapshot on shutdown).
func (w *baseWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
	w.wg.Wait()
}

func (w *baseWorker) UpdateInterval(d time.Duration) {
	select {
	case w.intervalChan <- d:
	case <-w.stopChan:
	}
}

func safeInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultInterval
	}
	return d
}

// SnapshotWorker periodically calls the persistence coordinator's
// snapshot entry point. The worker owns scheduling (interval ticker)
// and operation lifecycle; the plugin owns where the snapshot lands.
type SnapshotWorker struct {
	baseWorker
	snapshotter apipersistence.PersistenceAPI
}

func NewSnapshotWorker(c *cache.Cache, e *engine.Engine, interval time.Duration) *SnapshotWorker {
	return &SnapshotWorker{
		baseWorker: baseWorker{
			cache:        c,
			engine:       e,
			interval:     safeInterval(interval),
			stopChan:     make(chan struct{}),
			intervalChan: make(chan time.Duration, 1),
		},
	}
}

// SetPersistenceAPI wires the persistence coordinator into the worker.
// Each tick calls Snapshot through it. Pass nil to disable scheduled
// saves (operation is failed with a clear reason — never a silent no-op).
func (w *SnapshotWorker) SetPersistenceAPI(api apipersistence.PersistenceAPI) {
	w.snapshotter = api
}

func (w *SnapshotWorker) Start(parentCtx context.Context) {
	ticker := time.NewTicker(w.interval)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-ticker.C:
				op := w.startOp(parentCtx, ops.TypeSnapshot)
				if w.snapshotter == nil {
					w.logOp(op, apiobs.TelemetryLogLevelWarn, "snapshot scheduled but no snapshotter registered", nil)
					w.failOp(op, "no snapshotter registered")
					continue
				}
				if err := w.engine.Dispatch(op.ctx, func() {
					if err := w.snapshotter.Snapshot(op.ctx); err != nil {
						w.logOp(op, apiobs.TelemetryLogLevelWarn, "snapshot save failed", err)
						w.failOp(op, err.Error())
					} else {
						w.logOp(op, apiobs.TelemetryLogLevelDebug, "snapshot saved", nil)
						w.completeOp(op)
					}
				}); err != nil {
					w.logOp(op, apiobs.TelemetryLogLevelWarn, "snapshot dispatch failed", err)
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
				op := w.startOp(parentCtx, ops.TypeCleanup)
				if err := w.engine.Dispatch(op.ctx, func() {
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
					w.logOp(op, apiobs.TelemetryLogLevelDebug, "cleanup sweep completed", nil)
					w.completeOp(op)
				}); err != nil {
					w.logOp(op, apiobs.TelemetryLogLevelWarn, "cleanup dispatch failed", err)
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

package workers

import (
	"gocache/pkg/cache"
	"gocache/pkg/engine"
	"gocache/pkg/logger"
	"gocache/pkg/persistence"
	"sync"
	"time"
)

const defaultInterval = 5 * time.Minute

type Worker interface {
	Start()
	Stop()
	UpdateInterval(d time.Duration)
}

type baseWorker struct {
	cache        *cache.Cache
	engine       *engine.Engine
	interval     time.Duration
	stopChan     chan struct{}
	stopOnce     sync.Once
	intervalChan chan time.Duration
}

func (w *baseWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
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
	go func() {
		for {
			select {
			case <-ticker.C:
				file := w.file
				w.engine.Dispatch(func() {
					if err := persistence.SaveSnapshot(file, w.cache); err != nil {
						logger.Warn().Err(err).Msg("snapshot save failed")
					} else {
						logger.Debug().Str("file", file).Msg("snapshot saved")
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
	go func() {
		for {
			select {
			case <-ticker.C:
				w.engine.Dispatch(func() {
					now := time.Now().UnixNano()
					w.cache.Range(func(key string, entry *cache.Entry, expiration int64) bool {
						if expiration > 0 && now > expiration {
							w.cache.RawDelete(key)
						}
						return true
					})
					logger.Debug().Msg("cleanup sweep completed")
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

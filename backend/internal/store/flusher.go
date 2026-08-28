// Package store — Flusher handles the B5 write-behind cold tier.
//
// The WAL (B4) is the source of truth. Every accepted transition is fsynced
// before ack. The cold tier (SQLite) is a secondary index for history and
// player stats, populated asynchronously in batches. After a match is
// successfully copied to SQLite, its WAL file is checkpointed (removed) so a
// later restart does not replay it again.
//
// Crash safety: a crash between WAL append and SQLite flush is recovered by
// replaying the WAL into memory; the next flush (either the explicit recovery
// flush on startup or the periodic ticker) copies the same finished match to
// SQLite idempotently. WAL checkpoint is idempotent and only happens after the
// SQLite transaction commits, so durability is WAL's job until the cold copy
// is stable.
package store

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/sbidhya/tessera/backend/internal/room"
	"github.com/sbidhya/tessera/backend/internal/wal"
)

// Flusher batches finished matches to SQLite and checkpoints their WALs.
type Flusher struct {
	store   *Store
	wal     *wal.Store
	manager *room.Manager
	logger  *slog.Logger

	mu      sync.Mutex
	pending map[string]struct{}

	queue chan string
	done  chan struct{}
	wg    sync.WaitGroup

	batchSize int
	interval  time.Duration
}

// NewFlusher builds a write-behind flusher. store, wal, and manager must be
// non-nil. logger may be nil (defaults to slog.Default).
//
// batchSize controls how many finished matches are flushed together; interval
// is the periodic flush ticker. Either trigger causes a batch write.
// Defaults (10, 100ms) are used if non-positive.
func NewFlusher(store *Store, walStore *wal.Store, manager *room.Manager, logger *slog.Logger) *Flusher {
	if logger == nil {
		logger = slog.Default()
	}
	batchSize := 10
	interval := 100 * time.Millisecond
	return &Flusher{
		store:     store,
		wal:       walStore,
		manager:   manager,
		logger:    logger,
		pending:   make(map[string]struct{}),
		queue:     make(chan string, 64),
		done:      make(chan struct{}),
		batchSize: batchSize,
		interval:  interval,
	}
}

// Start launches the background flush loop. Stop must be called to shut it
// down cleanly.
func (f *Flusher) Start() {
	f.wg.Add(1)
	go f.loop()
}

// Stop terminates the background loop, does a final flush, and returns.
// It is idempotent.
func (f *Flusher) Stop() {
	select {
	case <-f.done:
		// already stopped
	default:
		close(f.done)
	}
	f.wg.Wait()
}

// Enqueue schedules roomID for the next batch flush. It is non-blocking and
// coalesces duplicate ids. Enqueue is safe to call from any goroutine, including
// the transport move handler or the room manager hook.
func (f *Flusher) Enqueue(roomID string) {
	if roomID == "" {
		return
	}
	select {
	case f.queue <- roomID:
	default:
		// Queue full: merge directly into pending under lock. This prevents a
		// slow flusher from dropping a finished match while still bounding memory.
		f.mu.Lock()
		f.pending[roomID] = struct{}{}
		f.mu.Unlock()
	}
}

// Flush synchronously flushes all pending finished matches plus any additional
// finished rooms discovered by scanning the manager. It is what tests and the
// startup recovery path call to force durability.
//
// Flush is safe to call concurrently with the background loop and with Enqueue.
func (f *Flusher) Flush(ctx context.Context) error {
	return f.flush(ctx)
}

// loop is the background goroutine for periodic and batch-size-triggered
// flushes.
func (f *Flusher) loop() {
	defer f.wg.Done()
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case id := <-f.queue:
			f.mu.Lock()
			f.pending[id] = struct{}{}
			n := len(f.pending)
			f.mu.Unlock()
			if n >= f.batchSize {
				if err := f.flush(context.Background()); err != nil {
					f.logger.Warn("store batch flush failed", "err", err)
				}
			}
		case <-ticker.C:
			if err := f.flush(context.Background()); err != nil {
				f.logger.Warn("store periodic flush failed", "err", err)
			}
		case <-f.done:
			// Final flush before exit.
			if err := f.flush(context.Background()); err != nil {
				f.logger.Warn("store final flush failed", "err", err)
			}
			return
		}
	}
}

func (f *Flusher) flush(ctx context.Context) error {
	// Snapshot pending and include any finished rooms from the manager that
	// haven't been enqueued yet (covers recovery where WAL replay recreated a
	// finished match but no Enqueue happened).
	f.mu.Lock()
	pendingCopy := make(map[string]struct{}, len(f.pending))
	for k, v := range f.pending {
		pendingCopy[k] = v
	}
	f.pending = make(map[string]struct{})
	f.mu.Unlock()

	// Merge manager's finished rooms.
	for _, r := range f.manager.List() {
		snap, err := r.Snapshot(ctx, "")
		if err != nil {
			continue
		}
		if snap.Status != room.StatusFinished {
			continue
		}
		pendingCopy[r.ID()] = struct{}{}
	}

	if len(pendingCopy) == 0 {
		return nil
	}
	ids := make([]string, 0, len(pendingCopy))
	for id := range pendingCopy {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Collect snapshots, skipping non-finished (defensive) and missing rooms.
	var snaps []room.Snapshot
	for _, id := range ids {
		r, ok := f.manager.Get(id)
		if !ok {
			continue
		}
		snap, err := r.Snapshot(ctx, "")
		if err != nil {
			continue
		}
		if snap.Status != room.StatusFinished {
			continue
		}
		snaps = append(snaps, snap)
	}
	if len(snaps) == 0 {
		return nil
	}

	if err := f.store.SaveBatch(ctx, snaps); err != nil {
		// Re-queue on failure so the next tick retries.
		f.mu.Lock()
		for _, snap := range snaps {
			f.pending[snap.RoomID] = struct{}{}
		}
		f.mu.Unlock()
		return err
	}

	// Checkpoint each WAL after the SQLite transaction has committed.
	for _, snap := range snaps {
		if err := f.wal.Checkpoint(snap.RoomID); err != nil {
			f.logger.Warn("wal checkpoint failed", "room", snap.RoomID, "err", err)
		} else {
			f.logger.Info("WAL checkpointed after cold flush", "room", snap.RoomID, "seq", snap.Seq)
		}
	}
	return nil
}

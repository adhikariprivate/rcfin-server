package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

// NewMediaOperationWorkerID returns a process-unique lease owner. Random
// suffixes prevent two server instances on different hosts from sharing a
// lease identity.
func NewMediaOperationWorkerID(prefix string) string {
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(suffix[:])
	}
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano())
}

// MediaOperationLeaseKeeper renews only operations this worker has successfully
// claimed. A lost claim cancels the worker context so it stops issuing writes.
type MediaOperationLeaseKeeper struct {
	ctx    context.Context
	cancel context.CancelFunc
	repo   MediaOperationRepo
	worker string
	lease  time.Duration
	mu     sync.Mutex
	active map[string]time.Time
}

func StartMediaOperationLeaseKeeper(ctx context.Context, repo MediaOperationRepo, worker string, lease time.Duration) (context.Context, *MediaOperationLeaseKeeper, func()) {
	workerCtx, cancel := context.WithCancel(ctx)
	keeper := &MediaOperationLeaseKeeper{ctx: workerCtx, cancel: cancel, repo: repo, worker: worker, lease: lease, active: map[string]time.Time{}}
	stop := func() { cancel() }
	if repo == nil || worker == "" || lease <= 0 {
		return workerCtx, keeper, stop
	}
	interval := lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				keeper.renew()
			}
		}
	}()
	return workerCtx, keeper, stop
}

func (k *MediaOperationLeaseKeeper) Track(id string) {
	if k == nil || id == "" {
		return
	}
	k.mu.Lock()
	k.active[id] = time.Now().UTC().Add(k.lease)
	k.mu.Unlock()
}

func (k *MediaOperationLeaseKeeper) Done(id string) {
	if k == nil || id == "" {
		return
	}
	k.mu.Lock()
	delete(k.active, id)
	k.mu.Unlock()
}

func (k *MediaOperationLeaseKeeper) renew() {
	k.mu.Lock()
	now := time.Now().UTC()
	ids := make([]string, 0, len(k.active))
	for id, leaseUntil := range k.active {
		if !leaseUntil.After(now.Add(k.lease / 3)) {
			ids = append(ids, id)
		}
	}
	k.mu.Unlock()
	for _, id := range ids {
		if err := k.ctx.Err(); err != nil {
			return
		}
		renewed, err := k.repo.RenewMediaOperationLease(k.ctx, id, k.worker, now, now.Add(k.lease))
		if err != nil {
			k.cancel()
			return
		}
		if !renewed {
			k.mu.Lock()
			_, stillActive := k.active[id]
			k.mu.Unlock()
			if stillActive {
				k.cancel()
				return
			}
		} else {
			k.mu.Lock()
			if _, stillActive := k.active[id]; stillActive {
				k.active[id] = now.Add(k.lease)
			}
			k.mu.Unlock()
		}
	}
}

// ListAllMediaOperations retrieves every operation using a stable descending
// timestamp/ID cursor. Callers that mutate UpdatedAt should fetch the full
// result before applying those mutations.
func ListAllMediaOperations(ctx context.Context, repo MediaOperationRepo, collectionID string) ([]model.MediaOperation, error) {
	const pageSize = 1000
	var operations []model.MediaOperation
	var beforeUpdatedAt time.Time
	var beforeID string
	for {
		page, err := repo.ListMediaOperationsPage(ctx, collectionID, pageSize, beforeUpdatedAt, beforeID)
		if err != nil {
			return nil, err
		}
		operations = append(operations, page...)
		if len(page) < pageSize {
			return operations, nil
		}
		last := page[len(page)-1]
		beforeUpdatedAt, beforeID = last.UpdatedAt, last.ID
	}
}

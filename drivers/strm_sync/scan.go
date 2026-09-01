package strm_sync

import (
	"context"
	"errors"
	"fmt"
	stdpath "path"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/cron"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

const (
	// defaultMaxDeletePerDir bounds how many entries one sync pass may remove
	// from a single directory.
	defaultMaxDeletePerDir = 50

	// deleteBudgetFactor turns the per-directory cap into a whole-pass budget.
	// The per-directory cap alone is close to useless: real media directories
	// hold a handful of files, so a listing failure spread over a thousand
	// directories produces a thousand small, individually plausible deletions.
	deleteBudgetFactor = 4

	// maxScanDepth stops a malformed listing from recursing until the stack
	// blows, which is a fatal error Go cannot recover from.
	maxScanDepth = 64

	// scanExitTimeout bounds how long Drop waits for a cancelled pass to
	// unwind. Cancellation is observed immediately, so hitting this means
	// something is structurally wrong rather than merely slow.
	scanExitTimeout = 2 * time.Second
)

// errDeleteBudgetExhausted aborts the whole pass. Anything else that goes wrong
// in one directory is recorded and the siblings carry on.
var errDeleteBudgetExhausted = errors.New("delete budget exhausted")

type scanStats struct {
	scannedDirs int
	skippedDirs int
	failedDirs  int
	listedObjs  int
}

// checkDeletion applies the per-directory rules to a sync deletion batch.
//
// It deliberately does not compare a remote/local ratio: a ratio only measures
// cardinality, so a listing that returns the right number of entries under the
// wrong names scores perfectly while every local entry is removed.
//
// Known limitation, covered by the pass budget rather than by these rules: a
// small directory whose listing comes back under entirely different names is
// still deleted, because three entries is far below any useful cap.
func checkDeletion(d *StrmSync, remoteCount, localCount, pending int) (skip bool, reason string) {
	if d.DisableDeleteProtect {
		return false, ""
	}
	if remoteCount == 0 && localCount > 0 {
		return true, fmt.Sprintf("remote listing is empty while %d local entries exist", localCount)
	}
	limit := d.MaxDeletePerDir
	if limit <= 0 {
		limit = defaultMaxDeletePerDir
	}
	if pending > limit {
		return true, fmt.Sprintf("deletion batch of %d exceeds the per-directory cap of %d", pending, limit)
	}
	return false, ""
}

// reserveDeleteBudget draws from what one pass is allowed to delete in total.
//
// Outside a pass there is no budget: browsing a directory is a deliberate user
// action. Inside a pass the budget is what keeps many individually plausible
// deletions from adding up to a wiped library.
func (d *StrmSync) reserveDeleteBudget(n int) (ok bool, reason string) {
	if d.DisableDeleteProtect || !d.scanning.Load() {
		return true, ""
	}
	if remaining := d.deleteBudget.Add(-int64(n)); remaining < 0 {
		return false, fmt.Sprintf("pass exhausted its deletion budget by %d entries", -remaining)
	}
	return true, ""
}

func (d *StrmSync) passDeleteBudget() int64 {
	limit := d.MaxDeletePerDir
	if limit <= 0 {
		limit = defaultMaxDeletePerDir
	}
	return int64(limit) * deleteBudgetFactor
}

// startScan wires up the periodic scan. Called at the end of Init.
func (d *StrmSync) startScan() {
	ctx, cancel := context.WithCancel(context.Background())
	var lim *rate.Limiter
	if d.ScanRateLimitPerSec > 0 {
		lim = rate.NewLimiter(rate.Limit(d.ScanRateLimitPerSec), 1)
	}

	d.scanMu.Lock()
	d.scanCtx, d.scanCancel = ctx, cancel
	if d.ScanIntervalMinutes > 0 {
		c := cron.NewCron(time.Duration(d.ScanIntervalMinutes) * time.Minute)
		// The callback has to return immediately. pkg/cron.Stop sends on an
		// unbuffered channel that is only received between ticks, so a callback
		// running for minutes blocks Stop -- and Drop, which calls Stop, runs
		// synchronously inside the storage HTTP handlers.
		c.Do(func() { d.spawnScan(ctx, lim, "cron") })
		d.cron = c
	}
	d.scanMu.Unlock()

	if d.ScanOnInit {
		go func() {
			// Storages load serially at boot; scanning before the source
			// storage is up makes every path fail with StorageNotFound.
			select {
			case <-conf.StoragesLoadSignal():
			case <-ctx.Done():
				return
			}
			d.spawnScan(ctx, lim, "init")
		}()
	}
}

// stopScan is called at the start of Drop.
func (d *StrmSync) stopScan() {
	// Take the handles under the lock and clear them. internal/op/storage.go
	// serialises nothing, so two concurrent Drops on the same pointer are
	// reachable; stopping pkg/cron twice panics with "send on closed channel",
	// and the load_all goroutine in server/handles/storage.go has no recover.
	d.scanMu.Lock()
	cancel, c := d.scanCancel, d.cron
	d.scanCancel, d.scanCtx, d.cron = nil, nil, nil
	d.scanMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if c != nil {
		c.Stop()
	}
	d.awaitScanExit()
}

// awaitScanExit blocks until an in-flight pass has returned, so the Init that
// follows a Drop can rebuild pathMap without racing a walk that still reads it.
func (d *StrmSync) awaitScanExit() {
	deadline := time.Now().Add(scanExitTimeout)
	for d.scanning.Load() {
		if time.Now().After(deadline) {
			log.Errorf("[strm-sync] mount=%s pass did not unwind within %s; leaving it running",
				d.MountPath, scanExitTimeout)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// spawnScan claims the running flag on the caller's goroutine and only then
// hands the work off. Claiming it inside the new goroutine instead would let
// Drop observe "no pass running" for a pass that is about to start, which is
// exactly the window awaitScanExit exists to close.
func (d *StrmSync) spawnScan(ctx context.Context, lim *rate.Limiter, reason string) {
	if utils.IsCanceled(ctx) {
		return
	}
	if !d.scanning.CompareAndSwap(false, true) {
		log.Infof("[strm-sync] mount=%s reason=%s skipped: previous pass still running", d.MountPath, reason)
		return
	}
	d.deleteBudget.Store(d.passDeleteBudget())
	go d.runScan(ctx, lim, reason)
}

func (d *StrmSync) runScan(ctx context.Context, lim *rate.Limiter, reason string) {
	defer d.scanning.Store(false)
	defer func() {
		// An unrecovered panic here would take the whole process down:
		// op.initStorage's recover does not cover goroutines we spawn.
		if r := recover(); r != nil {
			log.Errorf("[strm-sync] mount=%s panicked: %v", d.MountPath, r)
		}
	}()

	start := time.Now()
	before := d.counters()
	st := &scanStats{}

	err := d.walk(ctx, "/", 0, d.listMount, lim, st)

	after := d.counters()
	log.Infof("[strm-sync] mount=%s reason=%s duration=%s scanned_dirs=%d skipped_dirs=%d failed_dirs=%d "+
		"listed_objs=%d strm_written=%d deleted_files=%d deleted_dirs=%d delete_blocked=%d",
		d.MountPath, reason, time.Since(start).Truncate(time.Second),
		st.scannedDirs, st.skippedDirs, st.failedDirs, st.listedObjs,
		after.written-before.written, after.filesDeleted-before.filesDeleted,
		after.dirsDeleted-before.dirsDeleted, after.deleteBlocked-before.deleteBlocked)

	switch {
	case err == nil, errors.Is(err, context.Canceled):
	case errors.Is(err, errDeleteBudgetExhausted):
		log.Errorf("[strm-sync] mount=%s reason=%s aborted: %v", d.MountPath, reason, err)
	default:
		log.Errorf("[strm-sync] mount=%s reason=%s ended early: %v", d.MountPath, reason, err)
	}
}

// listFn is how walk reaches the tree. Production passes listMount; tests pass
// a fake so the traversal rules can be exercised without a live storage. The
// deletion rules are not reached through this seam -- they are tested directly
// against deleteExtra -- so faking a listing cannot make a deletion assertion
// pass vacuously.
type listFn func(ctx context.Context, mountPath string) ([]model.Obj, error)

func (d *StrmSync) listMount(ctx context.Context, mountPath string) ([]model.Obj, error) {
	return d.List(ctx, &model.Object{Path: mountPath, IsFolder: true}, model.ListArgs{Refresh: true})
}

// walk lists one mount directory, writes it out, then descends. Returning an
// error aborts the whole pass; per-directory problems go into stats instead.
//
// Directory entries from convert2strmObjs carry an empty Path, so children are
// addressed by joining names onto the mount path rather than by obj.GetPath().
func (d *StrmSync) walk(ctx context.Context, mountPath string, depth int, list listFn, lim *rate.Limiter, st *scanStats) error {
	if utils.IsCanceled(ctx) {
		return ctx.Err()
	}
	if depth > maxScanDepth {
		st.skippedDirs++
		log.Warnf("[strm-sync] mount=%s path=%s exceeded the depth limit of %d", d.MountPath, mountPath, maxScanDepth)
		return nil
	}

	objs, err := list(ctx, mountPath)
	if err != nil {
		st.failedDirs++
		log.Warnf("[strm-sync] mount=%s path=%s failed to list: %v", d.MountPath, mountPath, err)
		return nil
	}
	st.scannedDirs++
	st.listedObjs += len(objs)

	if err := d.writeLocal(ctx, mountPath, objs); err != nil {
		return err
	}

	// Collect child names and drop the objs before recursing: otherwise every
	// ancestor's full listing stays pinned on the stack for the whole descent.
	childNames := childDirNames(objs, st, d.MountPath, mountPath)
	objs = nil

	for _, name := range childNames {
		if lim != nil {
			if err := lim.Wait(ctx); err != nil {
				return err
			}
		}
		if utils.IsCanceled(ctx) {
			return ctx.Err()
		}
		if err := d.walk(ctx, stdpath.Join(mountPath, name), depth+1, list, lim, st); err != nil {
			return err
		}
	}
	return nil
}

// childDirNames picks the sub-directories worth descending into. A source that
// hands back "." , ".." or a name containing a separator would otherwise send
// the walk somewhere outside the subtree, or round in circles.
func childDirNames(objs []model.Obj, st *scanStats, mount, mountPath string) []string {
	out := make([]string, 0, len(objs))
	for _, obj := range objs {
		if !obj.IsDir() {
			continue
		}
		name := obj.GetName()
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			st.skippedDirs++
			log.Warnf("[strm-sync] mount=%s path=%s skipped a child with an unusable name %q", mount, mountPath, name)
			continue
		}
		out = append(out, name)
	}
	return out
}

// scanCounters is a snapshot of the per-storage counters.
type scanCounters struct {
	written       int64
	filesDeleted  int64
	dirsDeleted   int64
	deleteBlocked int64
}

func (d *StrmSync) counters() scanCounters {
	return scanCounters{
		written:       d.strmWritten.Load(),
		filesDeleted:  d.filesDeleted.Load(),
		dirsDeleted:   d.dirsDeleted.Load(),
		deleteBlocked: d.deleteBlocked.Load(),
	}
}

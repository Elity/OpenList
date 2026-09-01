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
	// unwind. Giving up is safe rather than merely tolerable: a pass works
	// against the immutable scanConfig it started with, so one that outlives
	// its Drop keeps using the old settings and cannot be corrupted by the
	// Init that follows.
	scanExitTimeout = 2 * time.Second
)

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
func checkDeletion(cfg *scanConfig, remoteCount, localCount, pending int) (skip bool, reason string) {
	if cfg.disableDeleteProtect {
		return false, ""
	}
	if remoteCount == 0 && localCount > 0 {
		return true, fmt.Sprintf("remote listing is empty while %d local entries exist", localCount)
	}
	if limit := cfg.maxDeletePerDir(); pending > limit {
		return true, fmt.Sprintf("deletion batch of %d exceeds the per-directory cap of %d", pending, limit)
	}
	return false, ""
}

// reserveDeleteBudget draws from what one pass is allowed to delete in total.
// It is what keeps many individually plausible deletions from adding up to a
// wiped library when a source misbehaves for a whole pass.
func (d *StrmSync) reserveDeleteBudget(cfg *scanConfig, n int) bool {
	if cfg.disableDeleteProtect {
		return true
	}
	return d.deleteBudget.Add(-int64(n)) >= 0
}

func (d *StrmSync) refundDeleteBudget(cfg *scanConfig, n int) {
	if cfg.disableDeleteProtect || n <= 0 {
		return
	}
	d.deleteBudget.Add(int64(n))
}

func passDeleteBudget(cfg *scanConfig) int64 {
	return int64(cfg.maxDeletePerDir()) * deleteBudgetFactor
}

// startScan wires up the periodic scan, replacing any scheduler already
// installed on this driver. It returns the context governing the new one, so a
// caller can tell when that scheduler is gone.
func (d *StrmSync) startScan(cfg *scanConfig) context.Context {
	// Stop whatever a previous call left running. op invokes Drop and Init on
	// the same pointer without holding a lock (internal/op/storage.go:204/249/272
	// and the bare goroutine in server/handles/storage.go:196), so two racing
	// updates can both find the handles already taken and nil. Without this the
	// losing side's cron ticks forever against a storage nobody can stop, and
	// goes on writing to disk after the storage has been disabled.
	d.stopScan()

	ctx, cancel := context.WithCancel(context.Background())
	var lim *rate.Limiter
	if cfg.scanRate > 0 {
		lim = rate.NewLimiter(rate.Limit(cfg.scanRate), 1)
	}

	d.scanMu.Lock()
	d.scanCancel = cancel
	if cfg.scanInterval > 0 {
		c := cron.NewCron(cfg.scanInterval)
		// The callback has to return immediately. pkg/cron.Stop sends on an
		// unbuffered channel that is only received between ticks, so a callback
		// running for minutes blocks Stop -- and Drop, which calls Stop, runs
		// synchronously inside the storage HTTP handlers.
		c.Do(func() { d.spawnScan(ctx, cfg, lim, "cron") })
		d.cron = c
	}
	d.scanMu.Unlock()

	if cfg.scanOnInit {
		go func() {
			// Storages load serially at boot; scanning before the source
			// storage is up makes every path fail with StorageNotFound.
			select {
			case <-conf.StoragesLoadSignal():
			case <-ctx.Done():
				return
			}
			d.spawnScan(ctx, cfg, lim, "init")
		}()
	}
	return ctx
}

// stopScan is called at the start of Drop, and by startScan before it installs
// a replacement.
func (d *StrmSync) stopScan() {
	// Take the handles under the lock and clear them. internal/op/storage.go
	// serialises nothing, so two concurrent Drops on the same pointer are
	// reachable. Sequential Stop calls on pkg/cron are safe, but concurrent
	// ones are not: both can miss the closed channel and one then sends on it,
	// panicking with "send on closed channel" inside the load_all goroutine in
	// server/handles/storage.go, which has no recover.
	d.scanMu.Lock()
	cancel, c := d.scanCancel, d.cron
	d.scanCancel, d.cron = nil, nil
	d.scanMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if c != nil {
		c.Stop()
	}
	d.awaitScanExit()
}

// awaitScanExit gives an in-flight pass a moment to notice the cancellation, so
// the common case leaves nothing running behind a Drop.
func (d *StrmSync) awaitScanExit() {
	deadline := time.Now().Add(scanExitTimeout)
	for d.scanning.Load() {
		if time.Now().After(deadline) {
			log.Warnf("[strm-sync] a pass did not unwind within %s; it will finish against its own configuration snapshot",
				scanExitTimeout)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// spawnScan claims the running flag on the caller's goroutine and only then
// hands the work off. Claiming it inside the new goroutine instead would let
// Drop observe "no pass running" for a pass that is about to start, which is
// exactly the window awaitScanExit exists to close.
func (d *StrmSync) spawnScan(ctx context.Context, cfg *scanConfig, lim *rate.Limiter, reason string) {
	if utils.IsCanceled(ctx) {
		return
	}
	if !d.scanning.CompareAndSwap(false, true) {
		log.Infof("[strm-sync] mount=%s reason=%s skipped: previous pass still running", cfg.mountPath, reason)
		return
	}
	d.deleteBudget.Store(passDeleteBudget(cfg))
	d.deletionsOff.Store(false)
	go d.runScan(ctx, cfg, lim, reason)
}

func (d *StrmSync) runScan(ctx context.Context, cfg *scanConfig, lim *rate.Limiter, reason string) {
	defer d.scanning.Store(false)
	defer func() {
		// An unrecovered panic here would take the whole process down:
		// op.initStorage's recover does not cover goroutines we spawn.
		if r := recover(); r != nil {
			log.Errorf("[strm-sync] mount=%s panicked: %v", cfg.mountPath, r)
		}
	}()

	start := time.Now()
	before := d.counters()
	st := &scanStats{}

	err := d.walk(ctx, cfg, "/", 0, d.listMount(cfg), lim, st)

	after := d.counters()
	log.Infof("[strm-sync] mount=%s reason=%s duration=%s scanned_dirs=%d skipped_dirs=%d failed_dirs=%d "+
		"listed_objs=%d strm_written=%d deleted_files=%d deleted_dirs=%d delete_blocked=%d deletions_off=%v",
		cfg.mountPath, reason, time.Since(start).Truncate(time.Second),
		st.scannedDirs, st.skippedDirs, st.failedDirs, st.listedObjs,
		after.written-before.written, after.filesDeleted-before.filesDeleted,
		after.dirsDeleted-before.dirsDeleted, after.deleteBlocked-before.deleteBlocked,
		d.deletionsOff.Load())

	switch {
	case err == nil, errors.Is(err, context.Canceled):
	default:
		log.Errorf("[strm-sync] mount=%s reason=%s ended early: %v", cfg.mountPath, reason, err)
	}
}

// listFn is how walk reaches the tree. Production passes listMount; tests pass
// a fake so the traversal rules can be exercised without a live storage. The
// deletion rules are not reached through this seam -- they are tested directly
// against deleteExtra -- so faking a listing cannot make a deletion assertion
// pass vacuously.
type listFn func(ctx context.Context, mountPath string) ([]model.Obj, error)

// listMount is the production seam. It reports an error when any configured
// source failed, which is what stops a rate-limited source from being read as
// "the file is gone" by the deletion pass.
func (d *StrmSync) listMount(cfg *scanConfig) listFn {
	return func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		objs, failed, err := cfg.listPaths(ctx, mountPath, true)
		if err != nil {
			return nil, err
		}
		if failed > 0 {
			return nil, fmt.Errorf("%d of the configured sources failed to list", failed)
		}
		return objs, nil
	}
}

// walk lists one mount directory, writes it out, then descends. Returning an
// error aborts the whole pass; per-directory problems go into stats instead.
//
// Directory entries from convert2strmObjs carry an empty Path, so children are
// addressed by joining names onto the mount path rather than by obj.GetPath().
func (d *StrmSync) walk(ctx context.Context, cfg *scanConfig, mountPath string, depth int, list listFn, lim *rate.Limiter, st *scanStats) error {
	if utils.IsCanceled(ctx) {
		return ctx.Err()
	}
	if depth > maxScanDepth {
		st.skippedDirs++
		log.Warnf("[strm-sync] mount=%s path=%s exceeded the depth limit of %d", cfg.mountPath, mountPath, maxScanDepth)
		return nil
	}

	objs, err := list(ctx, mountPath)
	if err != nil {
		st.failedDirs++
		log.Warnf("[strm-sync] mount=%s path=%s failed to list: %v", cfg.mountPath, mountPath, err)
		return nil
	}
	st.scannedDirs++
	st.listedObjs += len(objs)

	// Orphan pruning needs objs to be a real directory listing. It is, at every
	// depth -- except at the mount root of a multi-source storage, where objs is
	// the synthetic list of configured source names. A single-source storage is
	// flattened onto its root, so there the root is a real listing too, and
	// refusing to prune it would leave every dropped top-level folder behind
	// forever: for the common one-source-per-storage layout that is the whole
	// feature.
	if err := d.writeLocal(ctx, cfg, mountPath, objs, depth > 0 || cfg.autoFlatten); err != nil {
		return err
	}

	// Collect child names and drop the objs before recursing: otherwise every
	// ancestor's full listing stays pinned on the stack for the whole descent.
	childNames := childDirNames(objs, st, cfg.mountPath, mountPath)
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
		if err := d.walk(ctx, cfg, stdpath.Join(mountPath, name), depth+1, list, lim, st); err != nil {
			return err
		}
	}
	return nil
}

// safeLocalName rejects names that would not stay inside the directory they
// were listed in. filepath.Join cleans "..", so without this a crafted or
// mis-decoded name escapes localPath entirely.
func safeLocalName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`)
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
		if !safeLocalName(name) {
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

package strm_sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

const (
	// tempMarker and tempSuffix bracket the unique part of a temporary name.
	// Together with the leading dot they make a file recognisably ours without
	// putting it in the managed set, which is reserved for .strm.
	tempMarker = ".strm-sync-"
	tempSuffix = ".tmp"

	// staleTempAge is how long a leftover temporary has to survive before a
	// sync pass will clear it away. A write takes milliseconds, so anything
	// this old was orphaned by a process that died mid-write -- while the delay
	// still keeps us from deleting one that another instance is writing right
	// now.
	staleTempAge = time.Hour
)

// tempSeq disambiguates two temporaries created for the same target in the same
// process, which two driver instances writing the same localPath can do: op
// replaces a storage with a fresh instance while the old one's pass may still
// be unwinding.
var tempSeq atomic.Uint64

func tempNameFor(base string) string {
	return fmt.Sprintf(".%s%s%d-%d%s", base, tempMarker, os.Getpid(), tempSeq.Add(1), tempSuffix)
}

// isOurTemp reports whether a local file is a leftover from writeFileAtomic.
func isOurTemp(name string) bool {
	return strings.HasPrefix(name, ".") &&
		strings.HasSuffix(name, tempSuffix) &&
		strings.Contains(name, tempMarker)
}

// writeLocal materialises one directory listing. The local directory is created
// lazily so that source directories holding nothing we care about do not leave
// empty shells behind.
//
// allowPrune says whether a local directory the listing does not mention may be
// cleaned out. It is false only when objs is not a real directory listing --
// that is, at the mount root of a multi-source storage, where objs is the set
// of configured source names. Treating a sibling tree there as something the
// source dropped would eat an unrelated library, which is exactly what a
// migration looks like when localPath is pointed at an existing strm tree.
func (d *StrmSync) writeLocal(ctx context.Context, cfg *scanConfig, mountPath string, objs []model.Obj, allowPrune bool) error {
	localDir := cfg.localDirFor(mountPath)
	ensured := false
	ensure := func() bool {
		if ensured {
			return true
		}
		if err := os.MkdirAll(localDir, 0o777); err != nil {
			log.Warnf("[strm-sync] failed to create %s: %v", localDir, err)
			return false
		}
		ensured = true
		return true
	}

	for _, obj := range objs {
		if utils.IsCanceled(ctx) {
			return ctx.Err()
		}
		if obj.IsDir() {
			continue
		}
		name := obj.GetName()
		if !safeLocalName(name) {
			// filepath.Join cleans "..", so a crafted name would otherwise
			// resolve outside localDir and truncate a real file. childDirNames
			// applies the same rule to directories.
			log.Warnf("[strm-sync] mount=%s path=%s skipped a file with an unusable name %q", cfg.mountPath, mountPath, name)
			continue
		}
		if !ensure() {
			return nil
		}
		target := filepath.Join(localDir, name)
		if obj.GetID() == "strm" {
			d.writeStrm(ctx, cfg, obj, target)
		} else {
			d.downloadAttachment(ctx, cfg, obj, target)
		}
	}

	if cfg.localMode != LocalModeSync {
		return nil
	}
	return d.deleteExtra(ctx, cfg, localDir, objs, allowPrune)
}

// writeStrm writes the link body. getLink is pure string work, so this costs no
// network at all -- and because a strm file is a few hundred bytes, comparing
// the whole thing is cheaper than the size-plus-hash dance a large file needs.
func (d *StrmSync) writeStrm(ctx context.Context, cfg *scanConfig, obj model.Obj, target string) {
	if cfg.localMode == LocalModeInsert && utils.Exists(target) {
		return
	}
	content := cfg.getLink(ctx, obj.GetPath())
	if existing, err := os.ReadFile(target); err == nil && string(existing) == content {
		return
	}
	err := writeFileAtomic(target, func(w io.Writer) error {
		_, err := w.Write([]byte(content))
		return err
	})
	if err != nil {
		log.Warnf("[strm-sync] failed to write %s: %v", target, err)
		return
	}
	d.strmWritten.Add(1)
}

// writeFileAtomic writes through a hidden sibling and renames it into place.
//
// os.Rename within a directory is atomic, so a media server that opens the file
// while a pass is running sees either the whole old body or the whole new one,
// never a half-written line -- and an interrupted write leaves the previous
// content intact instead of a truncated or zero-byte stub, which the
// size-and-existence staleness checks would then happily accept forever.
//
// The temporary is created with 0666 so that the process umask applies exactly
// as it would to os.WriteFile on a new file; when the target already exists its
// mode is carried over instead, because a rename would otherwise quietly reset
// permissions an administrator had set. Hard links and xattrs on the target do
// not survive a rename -- that is inherent to the technique, and worth it.
func writeFileAtomic(target string, write func(io.Writer) error) error {
	dir, base := filepath.Dir(target), filepath.Base(target)
	tmp := filepath.Join(dir, tempNameFor(base))

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(tmp) // a no-op once the rename below has succeeded
	}()

	if err := write(f); err != nil {
		return err
	}
	if info, err := os.Stat(target); err == nil {
		if err := f.Chmod(info.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// downloadAttachment fetches a subtitle/image/nfo alongside the strm files.
// This is the only part of a pass that spends real API quota: on Aliyun every
// call goes through a 0.9/s limiter shared with playback, which is why all the
// attachment switches default to off.
//
// Staleness is judged by size alone. A subtitle edited without changing length
// will not be picked up; paying for a full re-download of every attachment on
// every pass to catch that is a bad trade.
func (d *StrmSync) downloadAttachment(ctx context.Context, cfg *scanConfig, obj model.Obj, target string) {
	if info, err := os.Stat(target); err == nil {
		if cfg.localMode == LocalModeInsert || info.Size() == obj.GetSize() {
			return
		}
	}
	link, err := d.attachmentLink(ctx, obj.GetPath())
	if err != nil {
		log.Warnf("[strm-sync] failed to link %s: %v", target, err)
		return
	}
	defer link.Close()
	size := link.ContentLength
	if size <= 0 {
		size = obj.GetSize()
	}
	rrf, err := stream.GetRangeReaderFromLink(size, link)
	if err != nil {
		log.Warnf("[strm-sync] failed to read %s: %v", target, err)
		return
	}
	rc, err := rrf.RangeRead(ctx, http_range.Range{Length: -1})
	if err != nil {
		log.Warnf("[strm-sync] failed to read %s: %v", target, err)
		return
	}
	defer rc.Close()
	// Through the atomic writer: staleness here is judged by size and existence
	// alone, so a download cut off half way would otherwise leave a short file
	// that insert mode never revisits and update mode only fixes if the length
	// happens to differ.
	if err := writeFileAtomic(target, func(w io.Writer) error {
		_, err := utils.CopyWithBuffer(w, rc)
		return err
	}); err != nil {
		log.Warnf("[strm-sync] failed to write %s: %v", target, err)
	}
}

// attachmentLink resolves a source object to a link. It goes straight to the
// op layer rather than through d.Link so that a pass never reads the driver's
// current configuration pointer: a pass works against the snapshot it started
// with, and Drop can swap that pointer while it is still unwinding.
func (d *StrmSync) attachmentLink(ctx context.Context, path string) (*model.Link, error) {
	if d.linkFn != nil {
		return d.linkFn(ctx, path)
	}
	link, _, err := linkTo(ctx, path, model.LinkArgs{})
	return link, err
}

// deleteExtra removes what this storage generated and the source no longer has.
//
// Two properties are structural rather than judgemental:
//   - only .strm files and our own leftover temporaries are candidates, so
//     scraper output is untouchable regardless of how the storage is configured;
//   - directories go through os.Remove, never os.RemoveAll, so a directory that
//     still holds anything survives whatever the guards decide.
//
// A local directory the remote listing no longer contains is an "orphan". The
// scan cannot reach it on its own -- walk only descends into what the source
// still returns -- so it is planned here, in full, before anything is removed:
// the batch has to be sized before it can be judged, and every unit charged
// against the budget has to be either performed or refunded.
func (d *StrmSync) deleteExtra(ctx context.Context, cfg *scanConfig, localDir string, objs []model.Obj, allowPrune bool) error {
	if utils.IsCanceled(ctx) {
		return ctx.Err()
	}
	if d.deletionsOff.Load() {
		return nil
	}
	entries, err := os.ReadDir(localDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Errorf("[strm-sync] failed to read %s: %v", localDir, err)
		}
		return nil
	}

	remoteFiles := make(map[string]struct{})
	remoteDirs := make(map[string]struct{})
	for _, obj := range objs {
		if obj.IsDir() {
			remoteDirs[obj.GetName()] = struct{}{}
		} else {
			remoteFiles[obj.GetName()] = struct{}{}
		}
	}

	var delFiles, delDirs, orphanDirs []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if _, ok := remoteDirs[name]; ok {
				continue
			}
			if isEmptyDir(filepath.Join(localDir, name)) {
				delDirs = append(delDirs, name)
			} else if allowPrune {
				orphanDirs = append(orphanDirs, name)
			}
			continue
		}
		if isStaleTemp(entry) {
			delFiles = append(delFiles, name)
			continue
		}
		if !cfg.isManaged(name) {
			continue
		}
		if _, ok := remoteFiles[name]; !ok {
			delFiles = append(delFiles, name)
		}
	}

	batch := &orphanBatch{}
	for _, name := range orphanDirs {
		if err := cfg.planOrphan(ctx, filepath.Join(localDir, name), 0, batch); err != nil {
			return err
		}
	}

	// Two different quantities, because the two guards answer two different
	// questions.
	//
	// The per-directory cap asks "did this listing lose an implausible number
	// of entries?", so it counts what is missing from *this* directory: an
	// orphaned subtree is one missing entry no matter how deep it goes.
	// Charging its contents here would refuse a legitimately removed ten-season
	// show while doing nothing extra about a bad listing.
	//
	// The budget asks "is this pass removing too much overall?", so it counts
	// actual work. Every unit charged to it is either performed or refunded
	// below, which is what keeps an orphan nobody can remove from burning the
	// allowance on every pass forever.
	capPending := len(delFiles) + len(delDirs) + len(orphanDirs)
	budgetUnits := len(delFiles) + len(delDirs) + batch.files + len(batch.dirs)
	if capPending == 0 && budgetUnits == 0 {
		return nil
	}
	if skip, reason := checkDeletion(cfg, len(objs), len(entries), capPending); skip {
		d.deleteBlocked.Add(int64(budgetUnits))
		log.Warnf("[strm-sync] refused to delete %d entries (%d files, %d directories, %d orphaned trees holding %d files) under %s: %s; for example %v",
			budgetUnits, len(delFiles), len(delDirs), len(orphanDirs), batch.files, localDir, reason,
			deletionSample(delFiles, delDirs, orphanDirs))
		return nil
	}
	if !d.reserveDeleteBudget(cfg, budgetUnits) {
		d.deleteBlocked.Add(int64(budgetUnits))
		d.deletionsOff.Store(true)
		log.Errorf("[strm-sync] mount=%s spent its deletion budget at %s; deletions are off for the rest of this pass, writing continues",
			cfg.mountPath, localDir)
		return nil
	}

	undone := d.removeEntries(localDir, delFiles, delDirs)
	undone += batch.execute(d)
	// Anything charged but not actually removed destroyed nothing, so it must
	// not consume the allowance. Otherwise a directory the process cannot write
	// to -- or an orphan a scraper keeps alive -- burns budget on every pass and
	// the library slowly stops being written at all.
	d.refundDeleteBudget(cfg, undone)
	return nil
}

// isStaleTemp reports whether an entry is one of our temporaries, left behind
// by a process that died between creating it and renaming it into place, and
// old enough that nobody can still be writing it.
func isStaleTemp(entry os.DirEntry) bool {
	if !isOurTemp(entry.Name()) {
		return false
	}
	info, err := entry.Info()
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > staleTempAge
}

// orphanBatch is a planned prune: the managed files found under orphaned
// directories, deepest directory first so that a subtree of nothing but strm
// files collapses in one pass instead of one level per pass.
type orphanBatch struct {
	dirs  []string
	names [][]string
	files int
}

func (b *orphanBatch) add(dir string, files []string) {
	b.dirs = append(b.dirs, dir)
	b.names = append(b.names, files)
	b.files += len(files)
}

// execute performs the planned removals and returns how many planned units did
// not happen, so the caller can refund exactly what it over-charged.
func (b *orphanBatch) execute(d *StrmSync) int {
	undone := 0
	for i, dir := range b.dirs {
		undone += d.removeEntries(dir, b.names[i], nil)
		if !isEmptyDir(dir) {
			// Something unmanaged is still in there -- a scraper's nfo, or a
			// subtree the depth limit stopped us reaching. The directory was
			// charged for, so give it back.
			undone++
			continue
		}
		// Hand the directory to removeEntries rather than removing it here:
		// that is the one place that knows a directory must go through
		// os.Remove, so anything that appeared in the meantime survives.
		undone += d.removeEntries(filepath.Dir(dir), nil, []string{filepath.Base(dir)})
	}
	return undone
}

// planOrphan walks an orphaned directory and records what could be removed,
// deepest first. It deletes nothing.
func (c *scanConfig) planOrphan(ctx context.Context, dir string, depth int, batch *orphanBatch) error {
	if utils.IsCanceled(ctx) {
		return ctx.Err()
	}
	if depth > maxScanDepth {
		log.Warnf("[strm-sync] %s exceeded the depth limit of %d while planning a prune", dir, maxScanDepth)
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warnf("[strm-sync] failed to read %s while planning a prune: %v", dir, err)
		return nil
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			if err := c.planOrphan(ctx, filepath.Join(dir, entry.Name()), depth+1, batch); err != nil {
				return err
			}
			continue
		}
		if c.isManaged(entry.Name()) || isStaleTemp(entry) {
			files = append(files, entry.Name())
		}
	}
	batch.add(dir, files)
	return nil
}

// removeEntries performs the deletions the guards approved and returns how many
// of them failed.
//
// Directories go through os.Remove and never os.RemoveAll. Callers only ever
// nominate a directory that looked empty, but that check and this call are
// separated by a window in which a scraper can write into one -- and os.Remove
// is what makes losing that race harmless.
func (d *StrmSync) removeEntries(localDir string, delFiles, delDirs []string) int {
	failed := 0
	for _, name := range delFiles {
		target := filepath.Join(localDir, name)
		if err := os.Remove(target); err != nil {
			log.Errorf("[strm-sync] failed to delete file %s: %v", target, err)
			failed++
			continue
		}
		d.filesDeleted.Add(1)
	}
	for _, name := range delDirs {
		target := filepath.Join(localDir, name)
		if err := os.Remove(target); err != nil {
			log.Debugf("[strm-sync] left directory %s in place: %v", target, err)
			failed++
			continue
		}
		d.dirsDeleted.Add(1)
	}
	return failed
}

func isEmptyDir(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err != nil
}

// deletionSample returns a handful of names so a refusal is actionable rather
// than just noisy.
func deletionSample(groups ...[]string) []string {
	const max = 5
	sample := make([]string, 0, max)
	for _, group := range groups {
		for _, name := range group {
			if len(sample) == max {
				return sample
			}
			sample = append(sample, name)
		}
	}
	return sample
}

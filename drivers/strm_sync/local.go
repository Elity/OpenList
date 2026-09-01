package strm_sync

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

// writeLocal materialises one directory listing. The local directory is created
// lazily so that source directories holding nothing we care about do not leave
// empty shells behind.
//
// allowPrune is false at the mount root. There, objs is the set of configured
// source roots rather than a real directory listing, so a sibling tree sitting
// next to ours under localPath is simply not described by it -- and must not be
// mistaken for something the source dropped. This matters most during a
// migration, when localPath is pointed at an existing strm tree whose every
// file looks managed.
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
// The temporary is created with 0666 and no explicit chmod so that the process
// umask still applies, matching what os.WriteFile would have done.
func writeFileAtomic(target string, write func(io.Writer) error) error {
	dir, base := filepath.Dir(target), filepath.Base(target)
	// A dotfile: media servers ignore it if a pass dies between create and
	// rename, and the suffix keeps it out of the managed set either way.
	tmp := filepath.Join(dir, "."+base+".strm-sync.tmp")

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
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
	link, err := d.Link(ctx, obj, model.LinkArgs{})
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

// deleteExtra removes what this storage generated and the source no longer has.
//
// Two properties are structural rather than judgemental:
//   - only .strm files are candidates, so scraper output is untouchable
//     regardless of how the storage is configured;
//   - directories go through os.Remove, never os.RemoveAll, so a directory that
//     still holds anything survives whatever the guards decide.
//
// A local directory the remote listing no longer contains is an "orphan". The
// scan cannot reach it on its own -- walk only descends into what the source
// still returns -- so it is planned here, in full, before anything is removed:
// the batch has to be sized before it can be judged.
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

	// The cap is measured in entries, orphan contents included. Counting a
	// whole subtree as one would let the number the operator configured mean
	// something entirely different on the one path with no remote listing to
	// check against.
	pending := len(delFiles) + len(delDirs) + len(orphanDirs) + batch.files
	if pending == 0 {
		return nil
	}
	if skip, reason := checkDeletion(cfg, len(objs), len(entries), pending); skip {
		d.deleteBlocked.Add(int64(pending))
		log.Warnf("[strm-sync] refused to delete %d entries (%d files, %d directories, %d orphaned trees holding %d files) under %s: %s; for example %v",
			pending, len(delFiles), len(delDirs), len(orphanDirs), batch.files, localDir, reason,
			deletionSample(delFiles, delDirs, orphanDirs))
		return nil
	}
	if !d.reserveDeleteBudget(cfg, pending) {
		d.deleteBlocked.Add(int64(pending))
		d.deletionsOff.Store(true)
		log.Errorf("[strm-sync] mount=%s spent its deletion budget at %s; deletions are off for the rest of this pass, writing continues",
			cfg.mountPath, localDir)
		return nil
	}

	failed := d.removeEntries(localDir, delFiles, delDirs)
	failed += batch.execute(d)
	// A removal that could not happen destroyed nothing, so it must not consume
	// the allowance. Otherwise one unwritable directory burns the budget every
	// pass and the library slowly stops being written at all.
	d.refundDeleteBudget(cfg, failed)
	return nil
}

// orphanBatch is a planned prune: the managed files found under orphaned
// directories, deepest directory first so that a subtree of nothing but strm
// files collapses in one pass.
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

func (b *orphanBatch) execute(d *StrmSync) int {
	failed := 0
	for i, dir := range b.dirs {
		failed += d.removeEntries(dir, b.names[i], nil)
		if !isEmptyDir(dir) {
			continue
		}
		// Hand the directory to removeEntries rather than removing it here:
		// that is the one place that knows a directory must go through
		// os.Remove, so anything that appeared in the meantime survives.
		d.removeEntries(filepath.Dir(dir), nil, []string{filepath.Base(dir)})
	}
	return failed
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
		if c.isManaged(entry.Name()) {
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

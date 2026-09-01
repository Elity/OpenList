package strm_sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

// localDirFor maps a mount path onto the local tree. Mount paths are POSIX, the
// local side is whatever the OS uses, so the conversion is explicit rather than
// relying on the two happening to agree.
//
// The layout is LocalPath + the mount-relative path, which is what makes the
// output line up with autofilm's target_dir. It also means two source paths
// sharing a basename land in different directories, unlike the upstream driver
// where both collapse onto SaveStrmLocalPath/<basename>.
func (d *StrmSync) localDirFor(mountPath string) string {
	rel := strings.TrimPrefix(mountPath, "/")
	if rel == "" {
		return d.LocalPath
	}
	return filepath.Join(d.LocalPath, filepath.FromSlash(rel))
}

// isManaged reports whether a local file is something this storage could have
// created. Anything else -- a scraper's .nfo, a poster, a file the user dropped
// in by hand -- is never a deletion candidate.
func (d *StrmSync) isManaged(name string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" {
		return false
	}
	_, ok := d.managedSuffix[ext]
	return ok
}

// writeLocal materialises one directory listing. The local directory is created
// lazily so that source directories holding nothing we care about do not leave
// empty shells behind.
func (d *StrmSync) writeLocal(ctx context.Context, mountPath string, objs []model.Obj) error {
	localDir := d.localDirFor(mountPath)
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
		if !ensure() {
			return nil
		}
		target := filepath.Join(localDir, obj.GetName())
		if obj.GetID() == "strm" {
			d.writeStrm(ctx, obj, target)
		} else {
			d.downloadAttachment(ctx, obj, target)
		}
	}

	if d.LocalMode != LocalModeSync {
		return nil
	}
	return d.deleteExtra(ctx, localDir, objs)
}

// writeStrm writes the link body. getLink is pure string work, so this costs no
// network at all -- and because a strm file is a few hundred bytes, comparing
// the whole thing is cheaper than the size-plus-hash dance a large file needs.
func (d *StrmSync) writeStrm(ctx context.Context, obj model.Obj, target string) {
	if d.LocalMode == LocalModeInsert && utils.Exists(target) {
		return
	}
	content := d.getLink(ctx, obj.GetPath())
	if existing, err := os.ReadFile(target); err == nil && string(existing) == content {
		return
	}
	if err := os.WriteFile(target, []byte(content), 0o666); err != nil {
		log.Warnf("[strm-sync] failed to write %s: %v", target, err)
		return
	}
	d.strmWritten.Add(1)
}

// downloadAttachment fetches a subtitle/image/nfo alongside the strm files.
// This is the only part of a pass that spends real API quota: on Aliyun every
// call goes through a 0.9/s limiter shared with playback, which is why all the
// attachment switches default to off.
//
// Staleness is judged by size alone. A subtitle edited without changing length
// will not be picked up; paying for a full re-download of every attachment on
// every pass to catch that is a bad trade.
func (d *StrmSync) downloadAttachment(ctx context.Context, obj model.Obj, target string) {
	if info, err := os.Stat(target); err == nil {
		if d.LocalMode == LocalModeInsert || info.Size() == obj.GetSize() {
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
	f, err := os.Create(target)
	if err != nil {
		log.Warnf("[strm-sync] failed to create %s: %v", target, err)
		return
	}
	defer f.Close()
	if _, err := utils.CopyWithBuffer(f, rc); err != nil {
		log.Warnf("[strm-sync] failed to copy %s: %v", target, err)
	}
}

// deleteExtra removes what this storage generated and the source no longer has.
//
// Two properties are structural rather than judgemental:
//   - only files whose suffix is in managedSuffix are candidates, so scraper
//     output is untouchable;
//   - directories go through os.Remove, never os.RemoveAll, so wiping a subtree
//     is not something this code can do regardless of what the guards decide.
func (d *StrmSync) deleteExtra(ctx context.Context, localDir string, objs []model.Obj) error {
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
			// An empty one goes straight away. A directory with something in
			// it is an orphan: the scan will never descend into it again,
			// because walk only follows what the remote listing still has.
			if isEmptyDir(filepath.Join(localDir, name)) {
				delDirs = append(delDirs, name)
			} else {
				orphanDirs = append(orphanDirs, name)
			}
			continue
		}
		if !d.isManaged(name) {
			continue
		}
		if _, ok := remoteFiles[name]; !ok {
			delFiles = append(delFiles, name)
		}
	}

	// Orphans count towards the cap even though pruning one may delete more
	// than a single entry. Their number is the signal that matters: a source
	// that returns a fraction of a directory makes every omitted child look
	// dropped, and that is precisely what the cap has to catch.
	pending := len(delFiles) + len(delDirs) + len(orphanDirs)
	if pending == 0 {
		return nil
	}
	if skip, reason := checkDeletion(d, len(objs), len(entries), pending); skip {
		d.deleteBlocked.Add(int64(pending))
		log.Warnf("[strm-sync] refused to delete %d entries (%d files, %d directories, %d orphaned trees) under %s: %s; for example %v",
			pending, len(delFiles), len(delDirs), len(orphanDirs), localDir, reason,
			deletionSample(delFiles, delDirs, orphanDirs))
		return nil
	}

	if batch := len(delFiles) + len(delDirs); batch > 0 {
		if ok, reason := d.reserveDeleteBudget(batch); !ok {
			d.deleteBlocked.Add(int64(batch))
			log.Errorf("[strm-sync] refused to delete %d entries under %s: %s", batch, localDir, reason)
			return errDeleteBudgetExhausted
		}
		d.removeEntries(localDir, delFiles, delDirs)
	}
	return d.pruneOrphans(ctx, localDir, orphanDirs)
}

// pruneOrphans cleans up under directories the source stopped listing.
//
// Without this, dropping a whole folder upstream leaves its strm files on disk
// permanently: walk never visits the local counterpart again, and the directory
// is not empty so deleteExtra will not remove it either. The result is an entry
// the media server still shows and that fails on play.
//
// The prune only ever deletes managed files, and a directory still only goes
// away once it is genuinely empty, so a scraper's nfo or poster both survives
// and keeps its directory alive.
func (d *StrmSync) pruneOrphans(ctx context.Context, parent string, orphanDirs []string) error {
	for _, name := range orphanDirs {
		if err := d.pruneOrphanTree(ctx, parent, name, 0); err != nil {
			return err
		}
	}
	return nil
}

// pruneOrphanTree walks one orphaned directory bottom-up. Children go first so
// that a subtree holding nothing but strm files disappears in a single pass
// instead of one level per pass.
func (d *StrmSync) pruneOrphanTree(ctx context.Context, parent, name string, depth int) error {
	if utils.IsCanceled(ctx) {
		return ctx.Err()
	}
	dir := filepath.Join(parent, name)
	if depth > maxScanDepth {
		log.Warnf("[strm-sync] %s exceeded the depth limit of %d while pruning", dir, maxScanDepth)
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warnf("[strm-sync] failed to read %s while pruning: %v", dir, err)
		return nil
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			if err := d.pruneOrphanTree(ctx, dir, entry.Name(), depth+1); err != nil {
				return err
			}
			continue
		}
		if d.isManaged(entry.Name()) {
			files = append(files, entry.Name())
		}
	}

	if len(files) > 0 {
		if ok, reason := d.reserveDeleteBudget(len(files)); !ok {
			d.deleteBlocked.Add(int64(len(files)))
			log.Errorf("[strm-sync] refused to prune %d entries under %s: %s", len(files), dir, reason)
			return errDeleteBudgetExhausted
		}
		d.removeEntries(dir, files, nil)
	}

	// Nominate the directory only once it looks empty, and hand it to
	// removeEntries rather than removing it here: that is the one place that
	// knows a directory must go through os.Remove and never os.RemoveAll, so
	// anything that appears in the meantime survives.
	if !isEmptyDir(dir) {
		return nil
	}
	if ok, reason := d.reserveDeleteBudget(1); !ok {
		d.deleteBlocked.Add(1)
		log.Errorf("[strm-sync] refused to prune %s: %s", dir, reason)
		return errDeleteBudgetExhausted
	}
	d.removeEntries(parent, nil, []string{name})
	return nil
}

// removeEntries performs the deletions the guards approved.
//
// Directories go through os.Remove and never os.RemoveAll. deleteExtra already
// filters the candidates down to empty directories, but that check and this
// call are separated by a window in which a scraper can write into one -- and
// os.Remove is what makes losing that race harmless.
func (d *StrmSync) removeEntries(localDir string, delFiles, delDirs []string) {
	for _, name := range delFiles {
		target := filepath.Join(localDir, name)
		if err := os.Remove(target); err != nil {
			log.Errorf("[strm-sync] failed to delete file %s: %v", target, err)
			continue
		}
		d.filesDeleted.Add(1)
	}
	for _, name := range delDirs {
		target := filepath.Join(localDir, name)
		if err := os.Remove(target); err != nil {
			log.Debugf("[strm-sync] left directory %s in place: %v", target, err)
			continue
		}
		d.dirsDeleted.Add(1)
	}
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

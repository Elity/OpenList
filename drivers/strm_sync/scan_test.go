package strm_sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// --- helpers ---------------------------------------------------------------

// syncDriver returns a driver wired for the deletion tests: sync mode, and the
// managed set the Init path would have produced for the given attachments.
func syncDriver(attachments ...string) *StrmSync {
	d := &StrmSync{}
	d.LocalMode = LocalModeSync
	d.managedSuffix = map[string]struct{}{"strm": {}}
	for _, ext := range attachments {
		d.managedSuffix[ext] = struct{}{}
	}
	return d
}

func seedLocal(t *testing.T, files, dirs []string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("seed dir %s: %v", dir, err)
		}
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed file %s: %v", f, err)
		}
	}
	return root
}

func remoteObjs(files, dirs []string) []model.Obj {
	objs := make([]model.Obj, 0, len(files)+len(dirs))
	for _, f := range files {
		objs = append(objs, &model.Object{Name: f, IsFolder: false})
	}
	for _, dir := range dirs {
		objs = append(objs, &model.Object{Name: dir, IsFolder: true})
	}
	return objs
}

func entriesOf(t *testing.T, root string) []string {
	t.Helper()
	des, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read dir %s: %v", root, err)
	}
	out := make([]string, 0, len(des))
	for _, de := range des {
		out = append(out, de.Name())
	}
	sort.Strings(out)
	return out
}

// names generates n distinct names. i < 676 keeps the second rune inside a-z.
func names(prefix string, n int, suffix string) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, prefix+string(rune('a'+i%26))+string(rune('a'+i/26))+suffix)
	}
	return out
}

func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := log.StandardLogger().Out
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// --- managed suffixes ------------------------------------------------------

func TestIsManagedOnlyCoversWhatThisStorageCreates(t *testing.T) {
	plain := syncDriver()
	withSubs := syncDriver("srt", "ass")

	cases := []struct {
		name         string
		wantPlain    bool
		wantWithSubs bool
	}{
		{name: "movie.strm", wantPlain: true, wantWithSubs: true},
		{name: "movie.nfo", wantPlain: false, wantWithSubs: false},
		{name: "poster.jpg", wantPlain: false, wantWithSubs: false},
		{name: "movie.srt", wantPlain: false, wantWithSubs: true},
		{name: "movie.STRM", wantPlain: true, wantWithSubs: true},
		{name: "README", wantPlain: false, wantWithSubs: false},
	}
	for _, tc := range cases {
		if got := plain.isManaged(tc.name); got != tc.wantPlain {
			t.Errorf("isManaged(%q) without attachments = %v, want %v", tc.name, got, tc.wantPlain)
		}
		if got := withSubs.isManaged(tc.name); got != tc.wantWithSubs {
			t.Errorf("isManaged(%q) with subtitles = %v, want %v", tc.name, got, tc.wantWithSubs)
		}
	}
}

// --- local layout ----------------------------------------------------------

// The layout is what makes two sources sharing a basename land apart, which is
// the collision the upstream driver has by construction.
func TestLocalDirForKeepsSameBasenameSourcesApart(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = filepath.Join("tmp", "library")

	aliyun := d.localDirFor("/aliyun/Movies/Some Film")
	quark := d.localDirFor("/quark/Movies/Some Film")

	if aliyun == quark {
		t.Fatalf("both sources mapped onto %s", aliyun)
	}
	if want := filepath.Join("tmp", "library", "aliyun", "Movies", "Some Film"); aliyun != want {
		t.Fatalf("localDirFor() = %s, want %s", aliyun, want)
	}
	if got := d.localDirFor("/"); got != d.LocalPath {
		t.Fatalf("localDirFor(\"/\") = %s, want %s", got, d.LocalPath)
	}
}

// --- checkDeletion ---------------------------------------------------------

func TestCheckDeletion(t *testing.T) {
	cases := []struct {
		name        string
		disable     bool
		maxPerDir   int
		remoteCount int
		localCount  int
		pending     int
		wantSkip    bool
	}{
		{name: "empty remote with local content is refused", remoteCount: 0, localCount: 3, pending: 3, wantSkip: true},
		{name: "empty remote with a single local entry is refused", remoteCount: 0, localCount: 1, pending: 1, wantSkip: true},
		{name: "empty remote and empty local is a no-op", remoteCount: 0, localCount: 0, pending: 0, wantSkip: false},
		{name: "batch within cap is allowed", remoteCount: 10, localCount: 40, pending: 30, wantSkip: false},
		{name: "batch at cap is allowed", remoteCount: 10, localCount: 60, pending: 50, wantSkip: false},
		{name: "batch over cap is refused", remoteCount: 100, localCount: 100, pending: 51, wantSkip: true},
		{name: "custom cap is honoured", maxPerDir: 5, remoteCount: 10, localCount: 20, pending: 6, wantSkip: true},
		{name: "protection disabled restores plain behaviour", disable: true, remoteCount: 0, localCount: 999, pending: 999, wantSkip: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &StrmSync{}
			d.DisableDeleteProtect = tc.disable
			d.MaxDeletePerDir = tc.maxPerDir
			skip, reason := checkDeletion(d, tc.remoteCount, tc.localCount, tc.pending)
			if skip != tc.wantSkip {
				t.Fatalf("checkDeletion() skip = %v (reason %q), want %v", skip, reason, tc.wantSkip)
			}
			if skip && reason == "" {
				t.Error("checkDeletion() refused the batch but gave no reason")
			}
		})
	}
}

func TestDeletionSample(t *testing.T) {
	got := deletionSample(
		[]string{"1.strm", "2.strm", "3.strm"},
		[]string{"d1", "d2", "d3"},
	)
	want := []string{"1.strm", "2.strm", "3.strm", "d1", "d2"} // files first, capped at 5
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("deletionSample() = %v, want %v", got, want)
	}
}

// --- deleteExtra -----------------------------------------------------------

// The headline property: whatever a scraper writes into the media directory is
// not ours, so it is never a candidate.
func TestDeleteExtraLeavesScraperOutputAlone(t *testing.T) {
	root := seedLocal(t, []string{
		"keep.strm", "stale.strm",
		"keep.nfo", "poster.jpg", "fanart.jpg", "movie.srt",
	}, nil)

	d := syncDriver() // no attachments enabled
	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	got := entriesOf(t, root)
	want := []string{"fanart.jpg", "keep.nfo", "keep.strm", "movie.srt", "poster.jpg"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries remaining = %v, want %v", got, want)
	}
	if d.filesDeleted.Load() != 1 {
		t.Errorf("filesDeleted = %d, want 1", d.filesDeleted.Load())
	}
}

// Enabling an attachment kind brings it into the managed set, and only then.
func TestDeleteExtraRemovesAttachmentsOnceTheyAreManaged(t *testing.T) {
	root := seedLocal(t, []string{"keep.strm", "stale.srt", "keep.nfo"}, nil)

	d := syncDriver("srt")
	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	got := entriesOf(t, root)
	want := []string{"keep.nfo", "keep.strm"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries remaining = %v, want %v", got, want)
	}
}

// os.RemoveAll does not appear in this package, so a directory holding anything
// at all survives no matter what the guards decide.
func TestDeleteExtraNeverRemovesANonEmptyDirectory(t *testing.T) {
	root := seedLocal(t, nil, []string{"Old Movie"})
	kept := filepath.Join(root, "Old Movie", "movie.nfo")
	if err := os.WriteFile(kept, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed nested file: %v", err)
	}

	d := syncDriver()
	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"other.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the subtree was wiped: %v", err)
	}
	if d.dirsDeleted.Load() != 0 {
		t.Errorf("dirsDeleted = %d, want 0", d.dirsDeleted.Load())
	}
}

func TestDeleteExtraRemovesAnEmptyDirectory(t *testing.T) {
	root := seedLocal(t, []string{"keep.strm"}, []string{"Gone"})

	d := syncDriver()
	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if got := entriesOf(t, root); strings.Join(got, ",") != "keep.strm" {
		t.Fatalf("entries remaining = %v, want [keep.strm]", got)
	}
	if d.dirsDeleted.Load() != 1 {
		t.Errorf("dirsDeleted = %d, want 1", d.dirsDeleted.Load())
	}
}

func TestDeleteExtraRefusesAnEmptyRemoteListing(t *testing.T) {
	root := seedLocal(t, []string{"a.strm", "b.strm", "c.strm"}, nil)

	d := syncDriver()
	if err := d.deleteExtra(context.Background(), root, nil); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if got := len(entriesOf(t, root)); got != 3 {
		t.Fatalf("files remaining = %d, want 3", got)
	}
	if d.deleteBlocked.Load() != 3 {
		t.Errorf("deleteBlocked = %d, want 3 (every refused entry counts)", d.deleteBlocked.Load())
	}
}

func TestDeleteExtraRefusesBatchesOverTheCap(t *testing.T) {
	root := seedLocal(t, names("local-", 100, ".strm"), nil)

	d := syncDriver()
	if err := d.deleteExtra(context.Background(), root, remoteObjs(names("remote-", 100, ".strm"), nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if got := len(entriesOf(t, root)); got != 100 {
		t.Fatalf("files remaining = %d, want 100", got)
	}
}

// Characterisation test for a known, accepted gap: the per-directory rules are
// cardinality based, so a small directory whose listing comes back under wholly
// different names IS emptied. The pass budget is what bounds the damage.
func TestDeleteExtraEmptiesSmallDisjointBatchesOutsideAPass(t *testing.T) {
	root := seedLocal(t, names("local-", 3, ".strm"), nil)

	d := syncDriver()
	if err := d.deleteExtra(context.Background(), root, remoteObjs(names("remote-", 3, ".strm"), nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if got := len(entriesOf(t, root)); got != 0 {
		t.Fatalf("files remaining = %d, want 0: the per-directory rules do not compare names", got)
	}
}

func TestDeleteExtraHonoursDisableDeleteProtect(t *testing.T) {
	root := seedLocal(t, names("movie-", 60, ".strm"), nil)

	d := syncDriver()
	d.DisableDeleteProtect = true
	if err := d.deleteExtra(context.Background(), root, nil); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if got := len(entriesOf(t, root)); got != 0 {
		t.Fatalf("files remaining = %d, want 0 (protection is disabled)", got)
	}
}

// --- per-pass delete budget ------------------------------------------------

// Each directory on its own is plausible; together they are a wipe. Only the
// budget sees the aggregate, and exhausting it aborts the pass.
func TestDeleteBudgetStopsAWipeSpreadAcrossDirectories(t *testing.T) {
	d := syncDriver()
	d.scanning.Store(true) // pretend a pass is in progress
	d.deleteBudget.Store(10)

	emptied, aborted := 0, false
	for i := 0; i < 20; i++ {
		root := seedLocal(t, names("local-", 3, ".strm"), nil)
		err := d.deleteExtra(context.Background(), root, remoteObjs(names("remote-", 3, ".strm"), nil))
		if errors.Is(err, errDeleteBudgetExhausted) {
			aborted = true
			break
		}
		if len(entriesOf(t, root)) == 0 {
			emptied++
		}
	}

	if emptied > 4 { // budget 10 / 3 per directory
		t.Fatalf("%d directories were emptied, a budget of 10 should have stopped it after 3", emptied)
	}
	if !aborted {
		t.Fatal("the budget never aborted the pass")
	}
}

// Outside a pass the budget must not apply: browsing one directory is a
// deliberate user action, not an unattended sweep.
func TestDeleteBudgetDoesNotApplyOutsideAPass(t *testing.T) {
	d := syncDriver()
	d.deleteBudget.Store(0) // exhausted by an earlier pass

	root := seedLocal(t, []string{"a.strm"}, nil)
	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"b.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if got := len(entriesOf(t, root)); got != 0 {
		t.Fatalf("files remaining = %d, want 0: the budget only applies inside a pass", got)
	}
}

// --- writeLocal ------------------------------------------------------------

func strmObj(name, sourcePath string) model.Obj {
	return &model.Object{ID: "strm", Name: name, Path: sourcePath, IsFolder: false}
}

func TestWriteLocalWritesTheSameBodyGetLinkProduces(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	d.LocalMode = LocalModeUpdate
	d.SiteUrl = "https://pan.example.com"
	d.EncodePath = true

	obj := strmObj("Some Film.strm", "/aliyun/Movies/Some Film/Some Film.mkv")
	if err := d.writeLocal(context.Background(), "/Movies/Some Film", []model.Obj{obj}); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}

	target := filepath.Join(d.LocalPath, "Movies", "Some Film", "Some Film.strm")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written strm: %v", err)
	}
	want := d.getLink(context.Background(), obj.GetPath())
	if string(body) != want {
		t.Fatalf("strm body = %q, want %q", body, want)
	}
	if d.strmWritten.Load() != 1 {
		t.Errorf("strmWritten = %d, want 1", d.strmWritten.Load())
	}
}

// A pass that rewrites every strm file it already wrote would wake the disk on
// every interval for nothing, and report work it did not do.
func TestWriteLocalSkipsFilesThatAreAlreadyCorrect(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	d.LocalMode = LocalModeUpdate
	d.SiteUrl = "https://pan.example.com"

	objs := []model.Obj{strmObj("Some Film.strm", "/aliyun/Movies/Some Film.mkv")}
	for i := 0; i < 3; i++ {
		if err := d.writeLocal(context.Background(), "/Movies", objs); err != nil {
			t.Fatalf("writeLocal() error = %v", err)
		}
	}

	if got := d.strmWritten.Load(); got != 1 {
		t.Fatalf("strmWritten = %d after three identical passes, want 1", got)
	}
}

// The sync gate lives in writeLocal, and every deleteExtra test above calls
// deleteExtra directly -- so without these two the gate could be removed
// entirely and nothing would fail.
func TestWriteLocalOnlyDeletesInSyncMode(t *testing.T) {
	for _, mode := range []string{LocalModeInsert, LocalModeUpdate} {
		d := &StrmSync{}
		d.LocalPath = t.TempDir()
		d.LocalMode = mode
		d.SiteUrl = "https://pan.example.com"
		d.managedSuffix = map[string]struct{}{"strm": {}}

		dir := filepath.Join(d.LocalPath, "Movies")
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatalf("seed dir: %v", err)
		}
		stale := filepath.Join(dir, "Gone.strm")
		if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}

		objs := []model.Obj{strmObj("Kept.strm", "/aliyun/Movies/Kept.mkv")}
		if err := d.writeLocal(context.Background(), "/Movies", objs); err != nil {
			t.Fatalf("mode %s: writeLocal() error = %v", mode, err)
		}

		if _, err := os.Stat(stale); err != nil {
			t.Fatalf("mode %s deleted a stale file; only sync mode may delete: %v", mode, err)
		}
	}
}

func TestWriteLocalDeletesInSyncMode(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	d.LocalMode = LocalModeSync
	d.SiteUrl = "https://pan.example.com"
	d.managedSuffix = map[string]struct{}{"strm": {}}

	dir := filepath.Join(d.LocalPath, "Movies")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	stale := filepath.Join(dir, "Gone.strm")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	objs := []model.Obj{strmObj("Kept.strm", "/aliyun/Movies/Kept.mkv")}
	if err := d.writeLocal(context.Background(), "/Movies", objs); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("sync mode left a stale file behind (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Kept.strm")); err != nil {
		t.Fatalf("sync mode removed the file it had just written: %v", err)
	}
}

func TestWriteLocalInsertModeLeavesExistingFilesAlone(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	d.LocalMode = LocalModeInsert
	d.SiteUrl = "https://pan.example.com"

	dir := filepath.Join(d.LocalPath, "Movies")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	target := filepath.Join(dir, "Some Film.strm")
	if err := os.WriteFile(target, []byte("hand written"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	obj := strmObj("Some Film.strm", "/aliyun/Movies/Some Film.mkv")
	if err := d.writeLocal(context.Background(), "/Movies", []model.Obj{obj}); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(body) != "hand written" {
		t.Fatalf("insert mode overwrote an existing file: %q", body)
	}
}

// A source directory with nothing we care about must not leave an empty shell
// behind, otherwise a media library fills up with hollow folders.
func TestWriteLocalDoesNotCreateEmptyDirectories(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	d.LocalMode = LocalModeUpdate
	d.SiteUrl = "https://pan.example.com"

	// Only sub-directories in the listing: nothing to write at this level.
	if err := d.writeLocal(context.Background(), "/Movies", remoteObjs(nil, []string{"Some Film"})); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(d.LocalPath, "Movies")); !os.IsNotExist(err) {
		t.Fatalf("an empty directory was created (stat err = %v)", err)
	}
}

// --- walk ------------------------------------------------------------------

func newWalkDriver(t *testing.T) *StrmSync {
	t.Helper()
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	d.LocalMode = LocalModeUpdate
	d.SiteUrl = "https://pan.example.com"
	return d
}

// A source that keeps handing back one more sub-directory would recurse until
// the stack blows, which is a fatal error Go cannot recover from.
func TestWalkStopsAtTheDepthLimit(t *testing.T) {
	d := newWalkDriver(t)
	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		return remoteObjs(nil, []string{"deeper"}), nil
	}

	st := &scanStats{}
	if err := d.walk(context.Background(), "/", 0, list, nil, st); err != nil {
		t.Fatalf("walk() error = %v", err)
	}
	if st.scannedDirs != maxScanDepth+1 {
		t.Fatalf("scannedDirs = %d, want %d", st.scannedDirs, maxScanDepth+1)
	}
	if st.skippedDirs != 1 {
		t.Fatalf("skippedDirs = %d, want 1", st.skippedDirs)
	}
}

func TestWalkKeepsGoingAfterOneDirectoryFails(t *testing.T) {
	d := newWalkDriver(t)
	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		switch mountPath {
		case "/":
			return remoteObjs(nil, []string{"good", "bad", "also-good"}), nil
		case "/bad":
			return nil, errors.New("upstream hiccup")
		default:
			return nil, nil
		}
	}

	st := &scanStats{}
	if err := d.walk(context.Background(), "/", 0, list, nil, st); err != nil {
		t.Fatalf("walk() error = %v", err)
	}
	if st.failedDirs != 1 {
		t.Fatalf("failedDirs = %d, want 1", st.failedDirs)
	}
	if st.scannedDirs != 3 { // root plus the two siblings that worked
		t.Fatalf("scannedDirs = %d, want 3: siblings must survive one failure", st.scannedDirs)
	}
}

func TestWalkSkipsChildrenWithUnusableNames(t *testing.T) {
	d := newWalkDriver(t)
	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		if mountPath != "/" {
			return nil, nil
		}
		return remoteObjs(nil, []string{".", "..", "a/b", "", "fine"}), nil
	}

	st := &scanStats{}
	if err := d.walk(context.Background(), "/", 0, list, nil, st); err != nil {
		t.Fatalf("walk() error = %v", err)
	}
	if st.skippedDirs != 4 {
		t.Fatalf("skippedDirs = %d, want 4", st.skippedDirs)
	}
	if st.scannedDirs != 2 { // root plus "fine"
		t.Fatalf("scannedDirs = %d, want 2", st.scannedDirs)
	}
}

func TestWalkStopsWhenTheContextIsCancelled(t *testing.T) {
	d := newWalkDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		cancel()
		return remoteObjs(nil, []string{"child"}), nil
	}

	st := &scanStats{}
	err := d.walk(ctx, "/", 0, list, nil, st)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("walk() error = %v, want context.Canceled", err)
	}
	if st.scannedDirs != 1 {
		t.Fatalf("scannedDirs = %d, want 1: the walk should stop at once", st.scannedDirs)
	}
}

// Nominating directories that cannot actually be removed would inflate the
// pending count, and that count is what the cap and the budget are spent
// against. Here a single genuinely stale strm sits among sixty directories that
// still hold scraper output: counting those would push the batch over the cap
// and block a deletion that should have gone through.
// TestDeleteExtraCountsOrphanedDirectoriesTowardsTheCap pins a deliberate
// trade-off. These directories cannot be removed -- a scraper's nfo keeps every
// one of them alive -- so counting them costs a legitimate single-file deletion
// that would otherwise have gone through.
//
// They are counted anyway: 60 directories disappearing from one listing is the
// shape of a source returning a fraction of a directory, and that is exactly
// what the cap exists to catch. Deciding an orphan is harmless would mean
// walking it first, which is both expensive and racy.
func TestDeleteExtraCountsOrphanedDirectoriesTowardsTheCap(t *testing.T) {
	dirs := names("season-", 60, "")
	root := seedLocal(t, []string{"stale.strm"}, dirs)
	for _, dir := range dirs {
		if err := os.WriteFile(filepath.Join(root, dir, "movie.nfo"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed nested file: %v", err)
		}
	}

	d := syncDriver()
	logs := captureLogs(t)
	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "stale.strm")); err != nil {
		t.Errorf("the whole batch should have been refused, so the stale strm stays: %v", err)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(root, dir, "movie.nfo")); err != nil {
			t.Fatalf("scraper output under %s was touched: %v", dir, err)
		}
	}
	if got := d.deleteBlocked.Load(); got != 61 {
		t.Errorf("deleteBlocked = %d, want 61", got)
	}
	if !strings.Contains(logs.String(), "orphaned trees") {
		t.Errorf("the refusal did not mention orphans, got: %s", logs.String())
	}
}

// A handful of orphans is business as usual: the stale file goes, the orphaned
// strm goes with it, and the directory a scraper still owns survives.
func TestDeleteExtraPrunesASmallNumberOfOrphans(t *testing.T) {
	d := syncDriver()
	root := seedLocal(t,
		[]string{"stale.strm", "Gone/Movie.strm", "Gone/Movie.nfo"},
		[]string{"Gone"})

	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	for _, rel := range []string{"stale.strm", "Gone/Movie.strm"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed, stat error = %v", rel, err)
		}
	}
	for _, rel := range []string{"Gone", "Gone/Movie.nfo"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s should have survived: %v", rel, err)
		}
	}
	if got := d.deleteBlocked.Load(); got != 0 {
		t.Errorf("deleteBlocked = %d, want 0", got)
	}
}

// deleteExtra only ever nominates empty directories, but a scraper can write
// into one between that check and the removal. os.Remove is what makes losing
// that race harmless; os.RemoveAll would take the subtree with it.
func TestRemoveEntriesCannotWipeADirectoryThatGainedContent(t *testing.T) {
	root := seedLocal(t, nil, []string{"Raced"})
	kept := filepath.Join(root, "Raced", "movie.nfo")
	if err := os.WriteFile(kept, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed nested file: %v", err)
	}

	d := syncDriver()
	// Nominate it anyway, exactly as a lost race would.
	d.removeEntries(root, nil, []string{"Raced"})

	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the subtree was wiped: %v", err)
	}
	if d.dirsDeleted.Load() != 0 {
		t.Errorf("dirsDeleted = %d, want 0", d.dirsDeleted.Load())
	}
}

// --- scheduler -------------------------------------------------------------

// The cron callback has to return immediately. pkg/cron.Stop sends on an
// unbuffered channel that is only received between ticks, so a callback that
// runs for the length of a scan blocks Stop -- and Drop, which calls Stop, runs
// synchronously inside the storage HTTP handlers.
func TestSpawnScanDoesNotBlockTheCaller(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	// Two mount roots plus a limiter slow enough that the walk needs seconds.
	d.pathMap = map[string][]string{"a": {"/no-such-storage-a"}, "b": {"/no-such-storage-b"}}
	slow := rate.NewLimiter(rate.Limit(0.5), 1)

	start := time.Now()
	d.spawnScan(context.Background(), slow, "test")
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("spawnScan blocked for %s; the cron callback must return at once", elapsed)
	}
	if !d.scanning.Load() {
		t.Fatal("spawnScan returned before the pass had even started, so the timing above proves nothing")
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for d.scanning.Load() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestSpawnScanSkipsWhileAPassIsRunning(t *testing.T) {
	d := &StrmSync{}
	d.scanning.Store(true)

	d.spawnScan(context.Background(), nil, "test")

	if !d.scanning.Load() {
		t.Error("spawnScan cleared the running flag it did not acquire")
	}
}

func TestSpawnScanReleasesTheRunningFlag(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	for i := 0; i < 2; i++ {
		d.spawnScan(context.Background(), nil, "test")
		deadline := time.Now().Add(2 * time.Second)
		for d.scanning.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if d.scanning.Load() {
			t.Fatalf("pass %d left the running flag set; every later pass would be skipped forever", i)
		}
	}
}

// internal/op/storage.go serialises nothing: two concurrent storage updates, or
// an update racing load_all, land in Drop on the same pointer. Stopping
// pkg/cron twice panics with "send on closed channel", and the load_all
// goroutine has no recover, so that panic takes the process down.
func TestStopScanIsSafeUnderConcurrentDrop(t *testing.T) {
	for i := 0; i < 200; i++ {
		d := &StrmSync{}
		d.LocalPath = t.TempDir()
		d.ScanIntervalMinutes = 1
		d.startScan()

		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.stopScan()
			}()
		}
		wg.Wait()
	}
}

// Drop must not return while a pass is still reading driver state, otherwise
// the Init that follows rebuilds pathMap underneath it.
func TestStopScanWaitsForAnInFlightPass(t *testing.T) {
	d := &StrmSync{}
	d.LocalPath = t.TempDir()
	d.ScanIntervalMinutes = 1
	d.startScan()

	d.scanning.Store(true)
	go func() {
		time.Sleep(300 * time.Millisecond)
		d.scanning.Store(false)
	}()

	start := time.Now()
	d.stopScan()

	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("stopScan returned after %s without waiting for the in-flight pass", elapsed)
	}
	if d.scanning.Load() {
		t.Error("stopScan returned while a pass was still marked running")
	}
}

func TestAwaitScanExitReturnsOnceThePassClearsTheFlag(t *testing.T) {
	d := &StrmSync{}
	d.scanning.Store(true)
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.scanning.Store(false)
	}()

	start := time.Now()
	d.awaitScanExit()
	if elapsed := time.Since(start); elapsed >= scanExitTimeout {
		t.Fatalf("awaitScanExit() took %s, it hit the timeout instead of observing the flag", elapsed)
	}
}

func TestAwaitScanExitGivesUpAfterTheTimeout(t *testing.T) {
	buf := captureLogs(t)
	d := &StrmSync{}
	d.scanning.Store(true)
	t.Cleanup(func() { d.scanning.Store(false) })

	start := time.Now()
	d.awaitScanExit()

	if elapsed := time.Since(start); elapsed < scanExitTimeout {
		t.Fatalf("awaitScanExit() returned after %s, it should have waited out %s", elapsed, scanExitTimeout)
	}
	if !strings.Contains(buf.String(), "did not unwind") {
		t.Fatalf("awaitScanExit() gave up silently; log = %q", buf.String())
	}
}

// --- directories the source dropped ----------------------------------------

// TestSyncPrunesManagedFilesUnderADirectoryTheSourceDropped covers the case the
// scan cannot reach on its own: walk only descends into directories the remote
// listing still contains, so once a whole source directory disappears nothing
// ever visits its local counterpart again and the strm files under it go stale
// forever. Emby shows those as entries that fail on play.
func TestSyncPrunesManagedFilesUnderADirectoryTheSourceDropped(t *testing.T) {
	d := syncDriver()
	root := seedLocal(t,
		[]string{
			"Gone/Movie.strm",
			"Gone/Movie.nfo",
			"Gone/Extras/Bonus.strm",
			"Kept/Movie.strm",
		},
		[]string{"Gone/Extras", "Kept"})

	if err := d.deleteExtra(context.Background(), root, remoteObjs(nil, []string{"Kept"})); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	for _, rel := range []string{"Gone/Movie.strm", "Gone/Extras/Bonus.strm", "Gone/Extras"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned, stat error = %v", rel, err)
		}
	}
	// Gone itself survives because a scraper's nfo is still in it, and Kept is
	// still listed remotely so this call must not reach inside it at all.
	for _, rel := range []string{"Gone", "Gone/Movie.nfo", "Kept", "Kept/Movie.strm"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s should have survived: %v", rel, err)
		}
	}
	if got := d.filesDeleted.Load(); got != 2 {
		t.Errorf("filesDeleted = %d, want 2", got)
	}
	if got := d.dirsDeleted.Load(); got != 1 {
		t.Errorf("dirsDeleted = %d, want 1", got)
	}
}

// TestOrphanPruneRefusesWhenTooManyDirectoriesLookOrphaned is the guard that
// makes the prune safe to add at all. Pruning is the one deletion path with no
// remote listing to check against, so a source that hiccups and returns a
// fraction of a directory makes every omitted child look dropped. Counting the
// orphans towards the per-directory cap is what stops that from cascading.
func TestOrphanPruneRefusesWhenTooManyDirectoriesLookOrphaned(t *testing.T) {
	d := syncDriver()
	d.MaxDeletePerDir = 3

	var files, dirs []string
	for _, name := range names("dir", 4, "") {
		dirs = append(dirs, name)
		files = append(files, name+"/Movie.strm")
	}
	root := seedLocal(t, files, dirs)
	logs := captureLogs(t)

	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	for _, rel := range files {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s should have survived a refused batch: %v", rel, err)
		}
	}
	if got := d.deleteBlocked.Load(); got != 4 {
		t.Errorf("deleteBlocked = %d, want 4", got)
	}
	if !strings.Contains(logs.String(), "exceeds the per-directory cap") {
		t.Errorf("refusal was not logged, got: %s", logs.String())
	}
}

// TestOrphanPruneRespectsThePassBudget keeps the prune under the same whole-pass
// ceiling as every other deletion, so a wide orphan tree cannot spend an
// unbounded number of deletions just because each directory looks small.
func TestOrphanPruneRespectsThePassBudget(t *testing.T) {
	d := syncDriver()
	d.MaxDeletePerDir = 4
	d.scanning.Store(true)
	d.deleteBudget.Store(3)

	root := seedLocal(t,
		[]string{"aa/1.strm", "aa/2.strm", "bb/1.strm", "bb/2.strm"},
		[]string{"aa", "bb"})

	err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"anchor.strm"}, nil))
	if !errors.Is(err, errDeleteBudgetExhausted) {
		t.Fatalf("deleteExtra() error = %v, want errDeleteBudgetExhausted", err)
	}
	// The first directory fits (2 files + the now-empty directory = 3); the
	// second must be untouched rather than half-deleted.
	if _, err := os.Lstat(filepath.Join(root, "aa")); !os.IsNotExist(err) {
		t.Errorf("aa should have been pruned away, stat error = %v", err)
	}
	for _, rel := range []string{"bb/1.strm", "bb/2.strm"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s should have survived the exhausted budget: %v", rel, err)
		}
	}
}

// TestOrphanPruneStopsAtTheDepthLimit mirrors the bound walk already has. A
// local tree deep enough to blow the stack is not reachable from a sane source,
// but the prune recurses over whatever is on disk, which is not the same thing.
func TestOrphanPruneStopsAtTheDepthLimit(t *testing.T) {
	d := syncDriver()
	deep := "Gone"
	for i := 0; i <= maxScanDepth; i++ {
		deep = filepath.Join(deep, "d")
	}
	root := seedLocal(t, []string{filepath.Join(deep, "Deep.strm"), "Gone/Shallow.strm"}, []string{deep})
	logs := captureLogs(t)

	if err := d.deleteExtra(context.Background(), root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "Gone/Shallow.strm")); !os.IsNotExist(err) {
		t.Errorf("the shallow strm should have been pruned, stat error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, deep, "Deep.strm")); err != nil {
		t.Errorf("the strm past the depth limit should have been left alone: %v", err)
	}
	if !strings.Contains(logs.String(), "depth limit") {
		t.Errorf("the depth cut-off was not logged, got: %s", logs.String())
	}
}

// The flat part of a deletion is one ReadDir and a capped batch, but the prune
// recurses over whatever is on disk, so it is the part that has to notice a
// cancelled pass rather than run to completion after Drop.
func TestOrphanPruneStopsWhenTheContextIsCancelled(t *testing.T) {
	d := syncDriver()
	root := seedLocal(t, []string{"Gone/Movie.strm"}, []string{"Gone"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := d.deleteExtra(ctx, root, remoteObjs([]string{"anchor.strm"}, nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("deleteExtra() error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "Gone/Movie.strm")); err != nil {
		t.Errorf("a cancelled prune should not have deleted anything: %v", err)
	}
	if got := d.filesDeleted.Load(); got != 0 {
		t.Errorf("filesDeleted = %d, want 0", got)
	}
}

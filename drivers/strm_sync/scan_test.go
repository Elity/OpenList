package strm_sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/cron"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// --- helpers ---------------------------------------------------------------

// syncDriver returns a driver plus the configuration snapshot the deletion
// tests work against. Going through buildConfig rather than hand-filling the
// struct keeps the tests honest about what Init actually produces.
func syncDriver(t *testing.T, tweak ...func(*Addition)) (*StrmSync, *scanConfig) {
	t.Helper()
	a := &Addition{
		Paths:     "/src",
		SiteUrl:   "https://pan.example.com",
		LocalPath: t.TempDir(),
		LocalMode: LocalModeSync,
	}
	for _, f := range tweak {
		f(a)
	}
	cfg, err := buildConfig(a, "/mnt")
	if err != nil {
		t.Fatalf("buildConfig() error = %v", err)
	}
	d := &StrmSync{}
	// Every deletion in production happens inside a pass, and a pass always
	// starts with a full budget. Tests that care about exhaustion overwrite it.
	d.deleteBudget.Store(passDeleteBudget(cfg))
	return d, cfg
}

// deleteExtraAt runs a deletion with orphan pruning enabled, i.e. the way walk
// calls it for every directory below the mount root.
func (d *StrmSync) deleteExtraAt(ctx context.Context, cfg *scanConfig, dir string, objs []model.Obj) error {
	return d.deleteExtra(ctx, cfg, dir, objs, true)
}

func (d *StrmSync) writeLocalAt(ctx context.Context, cfg *scanConfig, mountPath string, objs []model.Obj) error {
	return d.writeLocal(ctx, cfg, mountPath, objs, true)
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

// The managed set is exactly {strm} and does not widen with the attachment
// switches. That is deliberate: it is what makes "a scraper's output is never a
// deletion candidate" a property of the code instead of a property of the
// configuration. Turning on image downloads must not put an Emby-generated
// poster.jpg -- which the cloud side never had -- into the firing line.
func TestIsManagedCoversStrmAndNothingElse(t *testing.T) {
	_, plain := syncDriver(t)
	_, withAttachments := syncDriver(t, func(a *Addition) {
		a.DownloadSubtitle = true
		a.DownloadImage = true
		a.DownloadNfo = true
		a.DownloadExtraTypes = "bif"
	})

	cases := []struct {
		name string
		want bool
	}{
		{name: "movie.strm", want: true},
		{name: "movie.STRM", want: true},
		{name: "movie.nfo", want: false},
		{name: "poster.jpg", want: false},
		{name: "movie.srt", want: false},
		{name: "movie.bif", want: false},
		{name: "README", want: false},
		{name: "strm", want: false},
	}
	for _, tc := range cases {
		if got := plain.isManaged(tc.name); got != tc.want {
			t.Errorf("isManaged(%q) = %v, want %v", tc.name, got, tc.want)
		}
		if got := withAttachments.isManaged(tc.name); got != tc.want {
			t.Errorf("isManaged(%q) with every attachment enabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- local layout ----------------------------------------------------------

// The layout is what makes two sources sharing a basename land apart, which is
// the collision the upstream driver has by construction.
func TestLocalDirForKeepsSameBasenameSourcesApart(t *testing.T) {
	base := t.TempDir()
	_, cfg := syncDriver(t, func(a *Addition) { a.LocalPath = base })

	aliyun := cfg.localDirFor("/aliyun/Movies/Some Film")
	quark := cfg.localDirFor("/quark/Movies/Some Film")

	if aliyun == quark {
		t.Fatalf("both sources mapped onto %s", aliyun)
	}
	if want := filepath.Join(base, "aliyun", "Movies", "Some Film"); aliyun != want {
		t.Fatalf("localDirFor() = %s, want %s", aliyun, want)
	}
	if got := cfg.localDirFor("/"); got != base {
		t.Fatalf("localDirFor(\"/\") = %s, want %s", got, base)
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
		{name: "batch one over cap is refused", remoteCount: 100, localCount: 100, pending: 51, wantSkip: true},
		{name: "custom cap is honoured", maxPerDir: 5, remoteCount: 10, localCount: 20, pending: 6, wantSkip: true},
		{name: "custom cap allows its exact value", maxPerDir: 5, remoteCount: 10, localCount: 20, pending: 5, wantSkip: false},
		{name: "protection disabled restores plain behaviour", disable: true, remoteCount: 0, localCount: 999, pending: 999, wantSkip: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg := syncDriver(t, func(a *Addition) {
				a.DisableDeleteProtect = tc.disable
				a.MaxDeletePerDir = tc.maxPerDir
			})
			skip, reason := checkDeletion(cfg, tc.remoteCount, tc.localCount, tc.pending)
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

	d, cfg := syncDriver(t) // no attachments enabled
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
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

// Enabling an attachment kind makes this storage write that file type. It does
// not make it deletable: a subtitle or poster on disk may equally well have come
// from a scraper, and there is no way to tell the two apart by name. The stale
// attachment is simply left behind, which is the same trade this driver makes
// for orphaned directories.
func TestDeleteExtraNeverRemovesAttachmentsEvenWhenTheyAreDownloaded(t *testing.T) {
	root := seedLocal(t, []string{"keep.strm", "stale.srt", "stale.jpg", "keep.nfo"}, nil)

	d, cfg := syncDriver(t, func(a *Addition) {
		a.DownloadSubtitle = true
		a.DownloadImage = true
		a.DownloadNfo = true
	})
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	got := entriesOf(t, root)
	want := []string{"keep.nfo", "keep.strm", "stale.jpg", "stale.srt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries remaining = %v, want %v", got, want)
	}
	if d.filesDeleted.Load() != 0 {
		t.Errorf("filesDeleted = %d, want 0", d.filesDeleted.Load())
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

	d, cfg := syncDriver(t)
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"other.strm"}, nil)); err != nil {
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

	d, cfg := syncDriver(t)
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
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

	d, cfg := syncDriver(t)
	if err := d.deleteExtraAt(context.Background(), cfg, root, nil); err != nil {
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

	d, cfg := syncDriver(t)
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs(names("remote-", 100, ".strm"), nil)); err != nil {
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

	d, cfg := syncDriver(t)
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs(names("remote-", 3, ".strm"), nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if got := len(entriesOf(t, root)); got != 0 {
		t.Fatalf("files remaining = %d, want 0: the per-directory rules do not compare names", got)
	}
}

func TestDeleteExtraHonoursDisableDeleteProtect(t *testing.T) {
	root := seedLocal(t, names("movie-", 60, ".strm"), nil)

	d, cfg := syncDriver(t, func(a *Addition) { a.DisableDeleteProtect = true })
	if err := d.deleteExtraAt(context.Background(), cfg, root, nil); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if got := len(entriesOf(t, root)); got != 0 {
		t.Fatalf("files remaining = %d, want 0 (protection is disabled)", got)
	}
}

// --- per-pass delete budget ------------------------------------------------

// Each directory on its own is plausible; together they are a wipe. Only the
// budget sees the aggregate.
func TestDeleteBudgetStopsAWipeSpreadAcrossDirectories(t *testing.T) {
	d, cfg := syncDriver(t)
	d.deleteBudget.Store(10)

	emptied := 0
	for i := 0; i < 20; i++ {
		root := seedLocal(t, names("local-", 3, ".strm"), nil)
		if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs(names("remote-", 3, ".strm"), nil)); err != nil {
			t.Fatalf("deleteExtra() error = %v", err)
		}
		if len(entriesOf(t, root)) == 0 {
			emptied++
		}
	}

	if emptied > 4 { // budget 10 / 3 per directory
		t.Fatalf("%d directories were emptied, a budget of 10 should have stopped it after 3", emptied)
	}
	if !d.deletionsOff.Load() {
		t.Fatal("the budget never latched deletions off")
	}
}

// Exhausting the budget must stop deleting without stopping the pass. An
// unwritable directory otherwise burns the whole allowance every pass and the
// library slowly stops being written at all -- a deletion-side guard is not
// allowed to take the write side down with it.
func TestBudgetExhaustionStopsDeletingButNotWriting(t *testing.T) {
	d, cfg := syncDriver(t)
	d.deleteBudget.Store(1)

	spent := seedLocal(t, []string{"a.strm", "b.strm", "c.strm"}, nil)
	if err := d.deleteExtraAt(context.Background(), cfg, spent, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if !d.deletionsOff.Load() {
		t.Fatal("deletions were not latched off")
	}
	if got := len(entriesOf(t, spent)); got != 3 {
		t.Errorf("entries remaining = %d, want 3: an over-budget batch deletes nothing", got)
	}

	// Writing carries on for the rest of the pass.
	obj := strmObj("Film.strm", "/src/Movies/Film.mkv")
	if err := d.writeLocalAt(context.Background(), cfg, "/Movies", []model.Obj{obj}); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.localPath, "Movies", "Film.strm")); err != nil {
		t.Errorf("writing stopped along with deleting: %v", err)
	}
	if d.strmWritten.Load() != 1 {
		t.Errorf("strmWritten = %d, want 1", d.strmWritten.Load())
	}
}

// A removal that could not happen destroyed nothing, so it must not consume the
// allowance. Otherwise one directory the process cannot write to burns the
// budget on every pass, and the library eventually stops being written at all.
//
// This goes through deleteExtra rather than calling refundDeleteBudget itself:
// asserting on the helper in isolation passes even if nothing ever calls it.
func TestABlockedRemovalDoesNotSpendTheBudget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so no removal would fail")
	}
	d, cfg := syncDriver(t)
	root := seedLocal(t, []string{"stale.strm"}, nil)

	// A file can only be unlinked if its *directory* is writable.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	before := d.deleteBudget.Load()
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "stale.strm")); err != nil {
		t.Fatalf("the removal succeeded after all, so this test proves nothing: %v", err)
	}
	if got := d.deleteBudget.Load(); got != before {
		t.Errorf("budget = %d, want %d: a removal that failed was still charged", got, before)
	}
}

// --- writeLocal ------------------------------------------------------------

func strmObj(name, sourcePath string) model.Obj {
	return &model.Object{ID: "strm", Name: name, Path: sourcePath, IsFolder: false}
}

// The body is asserted against a literal, not against a fresh getLink call:
// comparing the file to the very function that wrote it is what let every
// mutation of the URL format survive for so long. What this test is really for
// is the path mapping and the wiring between writeLocal and writeStrm.
func TestWriteLocalWritesToTheMirroredPath(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) {
		a.LocalMode = LocalModeUpdate
		a.EncodePath = true
		a.PathPrefix = "/d"
	})

	obj := strmObj("Some Film.strm", "/aliyun/Movies/Some Film/Some Film.mkv")
	if err := d.writeLocalAt(context.Background(), cfg, "/Movies/Some Film", []model.Obj{obj}); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}

	target := filepath.Join(cfg.localPath, "Movies", "Some Film", "Some Film.strm")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written strm: %v", err)
	}
	want := "https://pan.example.com/d/aliyun/Movies/Some%20Film/Some%20Film.mkv"
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
	d, cfg := syncDriver(t, func(a *Addition) { a.LocalMode = LocalModeUpdate })

	objs := []model.Obj{strmObj("Some Film.strm", "/aliyun/Movies/Some Film.mkv")}
	for i := 0; i < 3; i++ {
		if err := d.writeLocalAt(context.Background(), cfg, "/Movies", objs); err != nil {
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
		d, cfg := syncDriver(t, func(a *Addition) { a.LocalMode = mode })

		dir := filepath.Join(cfg.localPath, "Movies")
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatalf("seed dir: %v", err)
		}
		stale := filepath.Join(dir, "Gone.strm")
		if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}

		objs := []model.Obj{strmObj("Kept.strm", "/aliyun/Movies/Kept.mkv")}
		if err := d.writeLocalAt(context.Background(), cfg, "/Movies", objs); err != nil {
			t.Fatalf("mode %s: writeLocal() error = %v", mode, err)
		}

		if _, err := os.Stat(stale); err != nil {
			t.Fatalf("mode %s deleted a stale file; only sync mode may delete: %v", mode, err)
		}
	}
}

func TestWriteLocalDeletesInSyncMode(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) { a.LocalMode = LocalModeSync })

	dir := filepath.Join(cfg.localPath, "Movies")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	stale := filepath.Join(dir, "Gone.strm")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	objs := []model.Obj{strmObj("Kept.strm", "/aliyun/Movies/Kept.mkv")}
	if err := d.writeLocalAt(context.Background(), cfg, "/Movies", objs); err != nil {
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
	d, cfg := syncDriver(t, func(a *Addition) { a.LocalMode = LocalModeInsert })

	dir := filepath.Join(cfg.localPath, "Movies")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	target := filepath.Join(dir, "Some Film.strm")
	if err := os.WriteFile(target, []byte("hand written"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	obj := strmObj("Some Film.strm", "/aliyun/Movies/Some Film.mkv")
	if err := d.writeLocalAt(context.Background(), cfg, "/Movies", []model.Obj{obj}); err != nil {
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
	d, cfg := syncDriver(t, func(a *Addition) { a.LocalMode = LocalModeUpdate })

	// Only sub-directories in the listing: nothing to write at this level.
	if err := d.writeLocalAt(context.Background(), cfg, "/Movies", remoteObjs(nil, []string{"Some Film"})); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(cfg.localPath, "Movies")); !os.IsNotExist(err) {
		t.Fatalf("an empty directory was created (stat err = %v)", err)
	}
}

// --- walk ------------------------------------------------------------------

func newWalkDriver(t *testing.T) (*StrmSync, *scanConfig) {
	t.Helper()
	return syncDriver(t, func(a *Addition) { a.LocalMode = LocalModeUpdate })
}

// A source that keeps handing back one more sub-directory would recurse until
// the stack blows, which is a fatal error Go cannot recover from.
func TestWalkStopsAtTheDepthLimit(t *testing.T) {
	d, cfg := newWalkDriver(t)
	// The source keeps handing back one more sub-directory, but only up to a
	// bound well past the limit. An unbounded fake would make a walk with the
	// limit removed recurse until the test binary is killed, which reads as a
	// timeout rather than as a failed assertion.
	const runway = maxScanDepth * 4
	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		if strings.Count(mountPath, "/") > runway {
			return nil, nil
		}
		return remoteObjs(nil, []string{"deeper"}), nil
	}

	st := &scanStats{}
	if err := d.walk(context.Background(), cfg, "/", 0, list, nil, st); err != nil {
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
	d, cfg := newWalkDriver(t)
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
	if err := d.walk(context.Background(), cfg, "/", 0, list, nil, st); err != nil {
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
	d, cfg := newWalkDriver(t)
	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		if mountPath != "/" {
			return nil, nil
		}
		return remoteObjs(nil, []string{".", "..", "a/b", "", "fine"}), nil
	}

	st := &scanStats{}
	if err := d.walk(context.Background(), cfg, "/", 0, list, nil, st); err != nil {
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
	d, cfg := newWalkDriver(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Counting the calls is the point. Asserting only on the returned error
	// passes even with every check in walk deleted, because writeLocal has one
	// of its own and the three shadow each other.
	calls := 0
	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		calls++
		return remoteObjs(nil, []string{"a", "b"}), nil
	}

	st := &scanStats{}
	if err := d.walk(ctx, cfg, "/", 0, list, nil, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("walk() error = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("the source was listed %d times on an already-cancelled pass, want 0", calls)
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

	d, cfg := syncDriver(t)
	logs := captureLogs(t)
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
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
	d, cfg := syncDriver(t)
	root := seedLocal(t,
		[]string{"stale.strm", "Gone/Movie.strm", "Gone/Movie.nfo"},
		[]string{"Gone"})

	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
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

	d, _ := syncDriver(t)
	// Nominate it anyway, exactly as a lost race would.
	if failed := d.removeEntries(root, nil, []string{"Raced"}); failed != 1 {
		t.Errorf("removeEntries() failed = %d, want 1", failed)
	}

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
	d, cfg := syncDriver(t, func(a *Addition) {
		// Two mount roots, pointed at storages that do not exist so the pass
		// has work to do without writing anything.
		a.Paths = "a:/no-such-storage-a\nb:/no-such-storage-b"
		a.LocalMode = LocalModeUpdate
	})
	// A limiter slow enough that the walk needs seconds.
	slow := rate.NewLimiter(rate.Limit(0.5), 1)

	start := time.Now()
	d.spawnScan(context.Background(), cfg, slow, "test")
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
	d, cfg := syncDriver(t)
	d.scanning.Store(true) // a pass is in flight
	d.deleteBudget.Store(7)
	d.deletionsOff.Store(true)

	d.spawnScan(context.Background(), cfg, nil, "test")

	if !d.scanning.Load() {
		t.Error("spawnScan cleared the running flag it did not acquire")
	}
	// The give-away that a second pass really was refused: winning the CAS is
	// immediately followed by resetting the budget and the latch. Asserting on
	// the flag alone would pass even with no guard at all.
	if got := d.deleteBudget.Load(); got != 7 {
		t.Errorf("deleteBudget = %d, want 7: a second pass started while one was running", got)
	}
	if !d.deletionsOff.Load() {
		t.Error("the running pass's deletion latch was cleared by a pass that should not have started")
	}
}

func TestSpawnScanReleasesTheRunningFlag(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) { a.LocalMode = LocalModeUpdate })
	for i := 0; i < 2; i++ {
		d.spawnScan(context.Background(), cfg, nil, "test")
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
		d, cfg := syncDriver(t, func(a *Addition) { a.ScanIntervalMinutes = 1 })
		d.startScan(cfg)

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
	d, cfg := syncDriver(t, func(a *Addition) { a.ScanIntervalMinutes = 1 })
	d.startScan(cfg)

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
	d, cfg := syncDriver(t)
	root := seedLocal(t,
		[]string{
			"Gone/Movie.strm",
			"Gone/Movie.nfo",
			"Gone/Extras/Bonus.strm",
			"Kept/Movie.strm",
		},
		[]string{"Gone/Extras", "Kept"})

	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs(nil, []string{"Kept"})); err != nil {
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
	d, cfg := syncDriver(t, func(a *Addition) { a.MaxDeletePerDir = 3 })

	var files, dirs []string
	for _, name := range names("dir", 4, "") {
		dirs = append(dirs, name)
		files = append(files, name+"/Movie.strm")
	}
	root := seedLocal(t, files, dirs)
	logs := captureLogs(t)

	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	for _, rel := range files {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s should have survived a refused batch: %v", rel, err)
		}
	}
	// Four orphan directories plus the four strm files inside them: the cap is
	// measured in entries, so a subtree cannot hide behind counting as one.
	if got := d.deleteBlocked.Load(); got != 8 {
		t.Errorf("deleteBlocked = %d, want 8", got)
	}
	if !strings.Contains(logs.String(), "exceeds the per-directory cap") {
		t.Errorf("refusal was not logged, got: %s", logs.String())
	}
}

// TestOrphanPruneRespectsThePassBudget keeps the prune under the same whole-pass
// ceiling as every other deletion, so a wide orphan tree cannot spend an
// unbounded number of deletions just because each directory looks small.
func TestOrphanPruneRespectsThePassBudget(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) { a.MaxDeletePerDir = 10 })
	d.deleteBudget.Store(3)

	root := seedLocal(t,
		[]string{"aa/1.strm", "aa/2.strm", "bb/1.strm", "bb/2.strm"},
		[]string{"aa", "bb"})

	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if !d.deletionsOff.Load() {
		t.Fatal("an over-budget orphan batch did not latch deletions off")
	}
	// The batch is reserved in one piece, so an over-budget prune leaves the
	// tree exactly as it found it rather than half-deleted.
	for _, rel := range []string{"aa/1.strm", "aa/2.strm", "bb/1.strm", "bb/2.strm"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s should have survived the exhausted budget: %v", rel, err)
		}
	}
	if got := d.filesDeleted.Load(); got != 0 {
		t.Errorf("filesDeleted = %d, want 0", got)
	}
}

// TestOrphanPruneStopsAtTheDepthLimit mirrors the bound walk already has. A
// local tree deep enough to blow the stack is not reachable from a sane source,
// but the prune recurses over whatever is on disk, which is not the same thing.
func TestOrphanPruneStopsAtTheDepthLimit(t *testing.T) {
	d, cfg := syncDriver(t)
	deep := "Gone"
	for i := 0; i <= maxScanDepth; i++ {
		deep = filepath.Join(deep, "d")
	}
	root := seedLocal(t, []string{filepath.Join(deep, "Deep.strm"), "Gone/Shallow.strm"}, []string{deep})
	logs := captureLogs(t)

	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
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
//
// The cancellation has to land *during* the prune. Cancelling up front only
// exercises the guard at the top of deleteExtra, which is a different line: the
// earlier version of this test did exactly that and passed with planOrphan's
// own check deleted outright.
func TestOrphanPruneStopsWhenTheContextIsCancelled(t *testing.T) {
	d, cfg := syncDriver(t)

	// Wide enough that the walk is still going when the deadline lands.
	var files, dirs []string
	for _, name := range names("gone", 40, "") {
		dirs = append(dirs, name)
		for _, f := range names("ep", 20, ".strm") {
			files = append(files, name+"/"+f)
		}
	}
	root := seedLocal(t, files, dirs)

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := &cancelOnRead{dir: filepath.Join(root, dirs[0]), cancel: cancel}
	cancelled.arm(t)

	err := d.deleteExtra(ctx, cfg, root, remoteObjs([]string{"anchor.strm"}, nil), true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("deleteExtra() error = %v, want context.Canceled", err)
	}
	// Planning deletes nothing, so nothing may be gone regardless of where it
	// stopped -- and it must have stopped, not finished.
	if got := d.filesDeleted.Load(); got != 0 {
		t.Errorf("filesDeleted = %d, want 0: a cancelled prune must not delete", got)
	}
	for _, rel := range files {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("%s was removed by a cancelled pass: %v", rel, err)
		}
	}
}

// cancelOnRead trips the context the moment the prune reaches a chosen
// directory, so the cancellation lands mid-walk rather than before it starts.
type cancelOnRead struct {
	dir    string
	cancel context.CancelFunc
}

func (c *cancelOnRead) arm(t *testing.T) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(c.dir); err == nil {
				c.cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
		c.cancel()
	}()
}

// --- getLink ---------------------------------------------------------------

// These assert the URL against literals rather than against getLink's own
// output. The previous test compared the written file to a fresh getLink call,
// which is the same function on both sides of the equals sign: it proved
// writeStrm calls getLink and nothing whatsoever about the URL format. Every
// mutation of encodePath, pathPrefix and withoutUrl survived it.
//
// The withSign variant is deliberately not covered here: sign.Sign reaches
// through internal/setting into the settings table, which a driver unit test has
// no business bootstrapping. That path is checked end to end instead, by
// diffing the generated tree against the one autofilm produces for the same
// library.
func TestGetLinkURLFormat(t *testing.T) {
	const src = "/src/Movies/Some Film (2020)/Some Film.mkv"

	cases := []struct {
		name  string
		tweak func(*Addition)
		want  string
	}{
		{
			name:  "encoded with a prefix",
			tweak: func(a *Addition) { a.EncodePath = true; a.PathPrefix = "/d" },
			want:  "https://pan.example.com/d/src/Movies/Some%20Film%20%282020%29/Some%20Film.mkv",
		},
		{
			name:  "unencoded",
			tweak: func(a *Addition) { a.EncodePath = false; a.PathPrefix = "/d" },
			want:  "https://pan.example.com/d/src/Movies/Some Film (2020)/Some Film.mkv",
		},
		{
			name:  "no prefix",
			tweak: func(a *Addition) { a.EncodePath = true; a.PathPrefix = "" },
			want:  "https://pan.example.com/src/Movies/Some%20Film%20%282020%29/Some%20Film.mkv",
		},
		{
			name:  "path only",
			tweak: func(a *Addition) { a.EncodePath = true; a.PathPrefix = "/d"; a.WithoutUrl = true },
			want:  "/d/src/Movies/Some%20Film%20%282020%29/Some%20Film.mkv",
		},
		{
			name:  "a trailing slash on the site url is not doubled",
			tweak: func(a *Addition) { a.EncodePath = true; a.PathPrefix = "/d"; a.SiteUrl = "https://pan.example.com/" },
			want:  "https://pan.example.com/d/src/Movies/Some%20Film%20%282020%29/Some%20Film.mkv",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg := syncDriver(t, tc.tweak)
			if got := cfg.getLink(context.Background(), src); got != tc.want {
				t.Errorf("getLink()\n got = %s\nwant = %s", got, tc.want)
			}
		})
	}
}

// --- convert2strmObjs ------------------------------------------------------

func sourceObjs(t *testing.T, spec ...string) []model.Obj {
	t.Helper()
	objs := make([]model.Obj, 0, len(spec))
	for _, s := range spec {
		name, sizeStr, _ := strings.Cut(s, "|")
		if name == "" {
			continue
		}
		if sizeStr == "dir" {
			objs = append(objs, &model.Object{Name: name, IsFolder: true})
			continue
		}
		size := int64(1)
		if sizeStr != "" {
			var parsed int64
			if _, err := fmt.Sscanf(sizeStr, "%d", &parsed); err != nil {
				t.Fatalf("bad size %q: %v", sizeStr, err)
			}
			size = parsed
		}
		objs = append(objs, &model.Object{Name: name, Size: size})
	}
	return objs
}

func objNames(objs []model.Obj) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.GetName())
	}
	sort.Strings(out)
	return out
}

// A source directory is full of things that are neither media nor a requested
// attachment: .db sidecars, partial downloads, desktop.ini. Letting those
// through would put every one of them through a real getDownloadUrl call.
func TestConvert2StrmObjsKeepsOnlyMediaAndRequestedAttachments(t *testing.T) {
	src := sourceObjs(t, "Film.mkv", "Film.srt", "Film.nfo", "poster.jpg",
		"Film.partial", "desktop.ini", "Extras|dir")

	t.Run("attachments off", func(t *testing.T) {
		_, cfg := syncDriver(t)
		got := objNames(cfg.convert2strmObjs(context.Background(), "/src/Movies", src))
		want := []string{"Extras", "Film.strm"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("names = %v, want %v", got, want)
		}
	})

	t.Run("subtitles on", func(t *testing.T) {
		_, cfg := syncDriver(t, func(a *Addition) { a.DownloadSubtitle = true })
		got := objNames(cfg.convert2strmObjs(context.Background(), "/src/Movies", src))
		want := []string{"Extras", "Film.srt", "Film.strm"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("names = %v, want %v", got, want)
		}
	})
}

// The rename and the "strm" id are what tell writeLocal to emit a link file
// instead of downloading the source.
func TestConvert2StrmObjsTagsAndRenamesMedia(t *testing.T) {
	_, cfg := syncDriver(t, func(a *Addition) { a.DownloadSubtitle = true })
	objs := cfg.convert2strmObjs(context.Background(), "/src/Movies",
		sourceObjs(t, "Film.mkv", "Film.srt", "Extras|dir"))

	byName := map[string]model.Obj{}
	for _, o := range objs {
		byName[o.GetName()] = o
	}

	media, ok := byName["Film.strm"]
	if !ok {
		t.Fatalf("media file was not renamed to .strm, got %v", objNames(objs))
	}
	if media.GetID() != "strm" {
		t.Errorf("media id = %q, want %q", media.GetID(), "strm")
	}
	if want := "/src/Movies/Film.mkv"; media.GetPath() != want {
		t.Errorf("media path = %q, want the source path %q", media.GetPath(), want)
	}
	if body := cfg.getLink(context.Background(), "/src/Movies/Film.mkv"); media.GetSize() != int64(len(body)) {
		t.Errorf("media size = %d, want the link length %d", media.GetSize(), len(body))
	}

	att, ok := byName["Film.srt"]
	if !ok {
		t.Fatalf("attachment was dropped, got %v", objNames(objs))
	}
	if att.GetID() != "" {
		t.Errorf("attachment id = %q, want empty so it is downloaded rather than linked", att.GetID())
	}

	// Directories carry no path; walk addresses children by joining names.
	if dir := byName["Extras"]; dir == nil || !dir.IsDir() || dir.GetPath() != "" {
		t.Errorf("directory entry = %+v, want a folder with an empty path", dir)
	}
}

func TestConvert2StrmObjsHonoursMinFileSize(t *testing.T) {
	_, cfg := syncDriver(t, func(a *Addition) { a.MinFileSize = 1 }) // 1 MB

	got := objNames(cfg.convert2strmObjs(context.Background(), "/src/Movies",
		sourceObjs(t, "Sample.mkv|1024", "Feature.mkv|2097152")))
	want := []string{"Feature.strm"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v: a 1 KB sample is below the 1 MB floor", got, want)
	}
}

// --- configuration ---------------------------------------------------------

func TestBuildConfigRejectsUnusableSettings(t *testing.T) {
	base := func() *Addition {
		return &Addition{Paths: "/src", SiteUrl: "https://pan.example.com", LocalPath: "/tmp/strm"}
	}
	cases := []struct {
		name  string
		tweak func(*Addition)
		want  string
	}{
		{name: "no paths", tweak: func(a *Addition) { a.Paths = "" }, want: "paths"},
		{name: "no local path", tweak: func(a *Addition) { a.LocalPath = "" }, want: "localPath"},
		{name: "no site url", tweak: func(a *Addition) { a.SiteUrl = "" }, want: "siteUrl"},
		{name: "relative local path", tweak: func(a *Addition) { a.LocalPath = "strm-out" }, want: "absolute"},
		{name: "local path at the filesystem root", tweak: func(a *Addition) { a.LocalPath = "/" }, want: "filesystem root"},
		{name: "source inside our own mount", tweak: func(a *Addition) { a.Paths = "/mnt/loop" }, want: "own mount"},
		{name: "source is our own mount", tweak: func(a *Addition) { a.Paths = "/mnt" }, want: "own mount"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base()
			tc.tweak(a)
			_, err := buildConfig(a, "/mnt")
			if err == nil {
				t.Fatalf("buildConfig() accepted %+v", a)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// withoutUrl removes the reason siteUrl is required at all.
func TestBuildConfigAllowsAMissingSiteUrlWithoutUrl(t *testing.T) {
	a := &Addition{Paths: "/src", LocalPath: "/tmp/strm", WithoutUrl: true}
	if _, err := buildConfig(a, "/mnt"); err != nil {
		t.Fatalf("buildConfig() error = %v", err)
	}
}

// --- scheduler lifecycle ---------------------------------------------------

// op calls Init and Drop on the same pointer without holding a lock, so two
// racing updates can both find the handles already taken and nil. Without a
// stop inside startScan the losing side's cron keeps ticking forever against a
// storage nobody can reach any more -- and it goes on writing to disk after the
// storage has been disabled.
func TestStartScanStopsThePreviousScheduler(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) { a.ScanIntervalMinutes = 1 })

	first := d.startScan(cfg)
	if first.Err() != nil {
		t.Fatal("the first scheduler was born cancelled")
	}
	second := d.startScan(cfg)

	if first.Err() == nil {
		t.Fatal("startScan installed a second scheduler without stopping the first")
	}
	if second.Err() != nil {
		t.Fatal("the replacement scheduler was cancelled too")
	}
	d.stopScan()
	if second.Err() == nil {
		t.Error("stopScan left the live scheduler running")
	}
}

// Init is the real entry point. Nothing else in the suite observes that it
// publishes a snapshot or starts a scheduler, or that Drop stops one -- all
// three could be deleted from the driver and the rest of these tests would not
// notice.
func TestInitStartsAndDropStopsTheScheduler(t *testing.T) {
	d := &StrmSync{}
	d.Storage.MountPath = "/mnt"
	d.Addition = Addition{
		Paths:               "/src",
		SiteUrl:             "https://pan.example.com",
		LocalPath:           t.TempDir(),
		LocalMode:           LocalModeUpdate,
		ScanIntervalMinutes: 1,
	}

	handles := func() (*cron.Cron, context.CancelFunc) {
		d.scanMu.Lock()
		defer d.scanMu.Unlock()
		return d.cron, d.scanCancel
	}

	for i := 0; i < 2; i++ {
		if err := d.Init(context.Background()); err != nil {
			t.Fatalf("Init() #%d error = %v", i, err)
		}
		if d.cfg.Load() == nil {
			t.Fatalf("Init() #%d published no configuration snapshot; every request would report an uninitialised storage", i)
		}
		c, cancel := handles()
		if c == nil {
			t.Fatalf("Init() #%d installed no cron, so the scheduled scan would never run", i)
		}
		if cancel == nil {
			t.Fatalf("Init() #%d installed no cancel handle, so Drop could not stop the pass", i)
		}
	}

	if err := d.Drop(context.Background()); err != nil {
		t.Fatalf("Drop() error = %v", err)
	}
	if c, cancel := handles(); c != nil || cancel != nil {
		t.Errorf("Drop() left cron=%v cancel!=nil=%v; a disabled storage would keep writing", c != nil, cancel != nil)
	}
	// A second Drop must not panic on the already-stopped cron.
	if err := d.Drop(context.Background()); err != nil {
		t.Fatalf("second Drop() error = %v", err)
	}
}

// The cron is only installed when an interval is configured, and stopping it is
// what makes a disabled storage actually stop.
func TestStartScanInstallsACronOnlyWhenAnIntervalIsSet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval int
		wantCron bool
	}{
		{name: "no interval", interval: 0, wantCron: false},
		{name: "one minute", interval: 1, wantCron: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cfg := syncDriver(t, func(a *Addition) { a.ScanIntervalMinutes = tc.interval })
			d.startScan(cfg)

			d.scanMu.Lock()
			got := d.cron != nil
			d.scanMu.Unlock()
			if got != tc.wantCron {
				t.Fatalf("cron installed = %v, want %v", got, tc.wantCron)
			}

			d.stopScan()
			d.scanMu.Lock()
			left := d.cron
			d.scanMu.Unlock()
			if left != nil {
				t.Error("stopScan left the cron handle in place")
			}
			d.stopScan() // must not panic on an already-stopped cron

			// Not asserted here: that stopScan actually calls c.Stop(). It
			// cancels the context first, so a cron goroutine that outlived the
			// call has nothing left to do -- spawnScan returns immediately on a
			// cancelled context. Dropping the Stop would leak a goroutine and a
			// ticker without changing any observable behaviour, and a test that
			// counted goroutines would be flaky for a resource leak this small.
		})
	}
}

// --- listMount -------------------------------------------------------------

// A source that is merely rate-limited must not be read as "the file is gone".
// Upstream's List drops per-source errors on the floor, which is harmless when
// the result only feeds a browser listing and disastrous when it also decides
// what to delete.
func TestListMountReportsAFailedSource(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) { a.Paths = "/no-such-storage" })

	if _, err := d.listMount(cfg)(context.Background(), "/"); err == nil {
		t.Fatal("listMount() swallowed a failed source; a deletion pass would read it as an empty directory")
	}
}

// walk turns that error into a skipped directory rather than an empty listing,
// so nothing downstream ever sees the failure as "the source has no files".
func TestWalkCountsAFailedListingAndWritesNothing(t *testing.T) {
	d, cfg := newWalkDriver(t)
	st := &scanStats{}
	failing := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		return nil, errors.New("rate limited")
	}

	if err := d.walk(context.Background(), cfg, "/", 0, failing, nil, st); err != nil {
		t.Fatalf("walk() error = %v", err)
	}
	if st.failedDirs != 1 {
		t.Errorf("failedDirs = %d, want 1", st.failedDirs)
	}
	if st.scannedDirs != 0 {
		t.Errorf("scannedDirs = %d, want 0", st.scannedDirs)
	}
}

// --- the mount root --------------------------------------------------------

// At the mount root objs is the set of configured source roots, not a real
// directory listing, so a neighbouring tree under localPath is simply not
// described by it. Pruning there would eat an unrelated library -- and the most
// likely time for one to be sitting next to us is a migration, when localPath is
// pointed at an existing strm tree whose every file looks managed.
func TestSyncDoesNotPruneNeighboursAtTheMountRoot(t *testing.T) {
	d, cfg := syncDriver(t)
	root := seedLocal(t,
		[]string{"OtherLibrary/Film.strm", "Ours/Film.strm"},
		[]string{"OtherLibrary", "Ours"})

	if err := d.deleteExtra(context.Background(), cfg, root, remoteObjs(nil, []string{"Ours"}), false); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "OtherLibrary", "Film.strm")); err != nil {
		t.Errorf("a neighbouring tree was pruned at the mount root: %v", err)
	}
	if got := d.filesDeleted.Load(); got != 0 {
		t.Errorf("filesDeleted = %d, want 0", got)
	}
}

// --- unusable names --------------------------------------------------------

func TestSafeLocalName(t *testing.T) {
	for name, want := range map[string]bool{
		"Film.mkv": true, "": false, ".": false, "..": false,
		"../poster.jpg": false, `a\b`: false, "a/b": false, ".hidden": true,
	} {
		if got := safeLocalName(name); got != want {
			t.Errorf("safeLocalName(%q) = %v, want %v", name, got, want)
		}
	}
}

// filepath.Join cleans "..", so a source that hands back such a name would
// otherwise have writeLocal truncate a file outside the directory being synced.
func TestWriteLocalRefusesNamesThatEscapeTheDirectory(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) { a.LocalMode = LocalModeUpdate })

	victim := filepath.Join(cfg.localPath, "poster.jpg")
	if err := os.WriteFile(victim, []byte("scraped"), 0o644); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	objs := []model.Obj{
		&model.Object{ID: "strm", Name: "../poster.jpg", Path: "/src/Movies/x.mkv"},
		strmObj("Fine.strm", "/src/Movies/Fine.mkv"),
	}
	if err := d.writeLocal(context.Background(), cfg, "/Movies", objs, true); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}

	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(body) != "scraped" {
		t.Fatalf("a crafted name escaped the directory and truncated %s: %q", victim, body)
	}
	if _, err := os.Stat(filepath.Join(cfg.localPath, "Movies", "Fine.strm")); err != nil {
		t.Errorf("the well-formed sibling was not written: %v", err)
	}
}

// --- atomic writes ---------------------------------------------------------

// A write that dies half way must leave the previous file alone. Both staleness
// checks in this package are "does it exist" and "is the size right", so a
// truncated file written in place would be accepted as correct on every
// subsequent pass -- permanently, in insert mode.
func TestWriteFileAtomicLeavesTheTargetIntactWhenTheWriteFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Film.strm")
	if err := os.WriteFile(target, []byte("the old body"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	boom := errors.New("connection reset")
	err := writeFileAtomic(target, func(w io.Writer) error {
		_, _ = w.Write([]byte("half a "))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("writeFileAtomic() error = %v, want %v", err, boom)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(body) != "the old body" {
		t.Errorf("target = %q, want the untouched old body", body)
	}
	if got := entriesOf(t, dir); len(got) != 1 || got[0] != "Film.strm" {
		t.Errorf("directory holds %v, want just the target: the temporary was not cleaned up", got)
	}
}

func TestWriteFileAtomicReplacesTheBody(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Film.strm")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeFileAtomic(target, func(w io.Writer) error {
		_, err := w.Write([]byte("https://pan.example.com/d/x.mkv"))
		return err
	}); err != nil {
		t.Fatalf("writeFileAtomic() error = %v", err)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if want := "https://pan.example.com/d/x.mkv"; string(body) != want {
		t.Errorf("target = %q, want %q", body, want)
	}
	if got := entriesOf(t, dir); len(got) != 1 {
		t.Errorf("directory holds %v, want just the target", got)
	}
}

// The end-to-end shape: a pass leaves exactly the strm file behind, with no
// temporary next to it for a media server to trip over.
func TestWriteStrmLeavesNoTemporaryBehind(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) { a.LocalMode = LocalModeUpdate })

	objs := []model.Obj{strmObj("Film.strm", "/src/Movies/Film.mkv")}
	if err := d.writeLocalAt(context.Background(), cfg, "/Movies", objs); err != nil {
		t.Fatalf("writeLocal() error = %v", err)
	}

	got := entriesOf(t, filepath.Join(cfg.localPath, "Movies"))
	if len(got) != 1 || got[0] != "Film.strm" {
		t.Fatalf("directory holds %v, want just Film.strm", got)
	}
}

// --- the gaps the mutation audit found -------------------------------------

// update mode exists to rewrite a strm whose URL has changed. Nothing asserted
// that it ever does: degrading the content comparison to "the file exists" left
// the whole suite green, which in production means a changed siteUrl or
// pathPrefix never reaches the files already on disk.
func TestWriteLocalUpdateModeRewritesAStaleStrm(t *testing.T) {
	shared := t.TempDir()
	mk := func(site string) (*StrmSync, *scanConfig) {
		return syncDriver(t, func(a *Addition) {
			a.LocalMode = LocalModeUpdate
			a.LocalPath = shared
			a.SiteUrl = site
			a.PathPrefix = "/d"
			a.EncodePath = true
		})
	}
	objs := []model.Obj{strmObj("Film.strm", "/src/Movies/Film.mkv")}

	d, cfg := mk("https://old.example.com")
	if err := d.writeLocalAt(context.Background(), cfg, "/Movies", objs); err != nil {
		t.Fatalf("first writeLocal() error = %v", err)
	}

	d2, cfg2 := mk("https://new.example.com")
	if err := d2.writeLocalAt(context.Background(), cfg2, "/Movies", objs); err != nil {
		t.Fatalf("second writeLocal() error = %v", err)
	}

	body, err := os.ReadFile(filepath.Join(shared, "Movies", "Film.strm"))
	if err != nil {
		t.Fatalf("read strm: %v", err)
	}
	if want := "https://new.example.com/d/src/Movies/Film.mkv"; string(body) != want {
		t.Fatalf("strm body = %q, want %q: update mode did not rewrite a stale file", body, want)
	}
	if d2.strmWritten.Load() != 1 {
		t.Errorf("strmWritten = %d on the rewriting pass, want 1", d2.strmWritten.Load())
	}
}

// spawnScan arms a new pass. Neither Store was observed anywhere: without the
// budget one, sync mode silently deletes nothing at all; without the latch one,
// a single exhausted pass turns deletion off permanently.
func TestSpawnScanArmsTheBudgetForANewPass(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) {
		a.MaxDeletePerDir = 7
		a.LocalMode = LocalModeUpdate
	})
	d.deleteBudget.Store(0)
	d.deletionsOff.Store(true)

	d.spawnScan(context.Background(), cfg, nil, "test")
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for d.scanning.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	})

	if got, want := d.deleteBudget.Load(), passDeleteBudget(cfg); got != want {
		t.Errorf("deleteBudget = %d, want %d", got, want)
	}
	if d.deletionsOff.Load() {
		t.Error("the deletion latch from an earlier pass was never cleared")
	}
}

// The size of a pass budget is a safety property in its own right: it can be
// widened into uselessness without any test noticing, because every budget test
// sets the value itself.
func TestPassDeleteBudgetScalesWithTheConfiguredCap(t *testing.T) {
	// Literals, not expressions built from the constants: widening
	// deleteBudgetFactor to 1000 must fail here, and it cannot if the expected
	// value is derived from it.
	_, custom := syncDriver(t, func(a *Addition) { a.MaxDeletePerDir = 7 })
	if got := passDeleteBudget(custom); got != 28 {
		t.Errorf("passDeleteBudget() with a cap of 7 = %d, want 28", got)
	}
	_, def := syncDriver(t)
	if got := passDeleteBudget(def); got != 200 {
		t.Errorf("passDeleteBudget() with the default cap = %d, want 200", got)
	}
}

// The wiring between walk and writeLocal decides whether a neighbouring library
// under localPath survives. Every existing test called deleteExtra directly, so
// the argument walk passes was unguarded -- and flipping it to "always prune"
// is a data-loss bug.
func TestWalkPrunesBelowTheRootButNotAtAMultiSourceRoot(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) {
		a.Paths = "one:/src/one\ntwo:/src/two" // two sources => synthetic root
		a.LocalMode = LocalModeSync
	})
	root := cfg.localPath
	seed := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o777); err != nil {
			t.Fatalf("seed dir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}
	seed("OtherLibrary/Film.strm", "x") // a neighbour at the mount root
	seed("one/Gone/Film.strm", "x")     // an orphan one level down

	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		switch mountPath {
		case "/":
			return remoteObjs(nil, []string{"one", "two"}), nil
		case "/one":
			return remoteObjs(nil, []string{"Kept"}), nil
		}
		return nil, nil
	}

	if err := d.walk(context.Background(), cfg, "/", 0, list, nil, &scanStats{}); err != nil {
		t.Fatalf("walk() error = %v", err)
	}

	if _, err := os.Lstat(filepath.Join(root, "OtherLibrary", "Film.strm")); err != nil {
		t.Errorf("a neighbouring library was pruned at the mount root: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "one", "Gone", "Film.strm")); !os.IsNotExist(err) {
		t.Errorf("an orphan below the root was not pruned, stat error = %v", err)
	}
}

// A single-source storage is flattened onto its root, so there the root is a
// real listing and refusing to prune it would leave every dropped top-level
// folder behind forever -- which for the usual one-source-per-storage layout is
// the entire feature.
func TestWalkPrunesAtTheRootOfASingleSourceStorage(t *testing.T) {
	d, cfg := syncDriver(t)
	root := cfg.localPath
	for _, rel := range []string{"Gone", "Kept"} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o777); err != nil {
			t.Fatalf("seed dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, rel, "Film.strm"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}

	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		if mountPath == "/" {
			return remoteObjs(nil, []string{"Kept"}), nil
		}
		return remoteObjs([]string{"Film.strm"}, nil), nil
	}

	if err := d.walk(context.Background(), cfg, "/", 0, list, nil, &scanStats{}); err != nil {
		t.Fatalf("walk() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "Gone")); !os.IsNotExist(err) {
		t.Errorf("the dropped top-level folder survived, stat error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "Kept", "Film.strm")); err != nil {
		t.Errorf("a folder the source still lists was touched: %v", err)
	}
}

// An orphan the process cannot clean must not cost the pass anything, or one
// unwritable directory drains the allowance on every pass forever.
func TestABlockedOrphanPruneDoesNotSpendTheBudget(t *testing.T) {
	d, cfg := syncDriver(t)
	root := seedLocal(t, []string{"Gone/Film.strm", "Gone/movie.nfo"}, []string{"Gone"})

	before := d.deleteBudget.Load()
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	// The strm goes; the directory cannot, because the scraper's nfo holds it.
	if _, err := os.Lstat(filepath.Join(root, "Gone", "Film.strm")); !os.IsNotExist(err) {
		t.Fatalf("the orphaned strm survived, stat error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "Gone")); err != nil {
		t.Fatalf("the directory holding scraper output was removed: %v", err)
	}
	if got, want := d.deleteBudget.Load(), before-1; got != want {
		t.Errorf("budget = %d, want %d: exactly the one deletion that happened should have been charged", got, want)
	}
}

// The other half of the refund: a removal that fails *inside* an orphan tree.
// The directory-level case above is a different line of code, and covering only
// that one left the file-level accounting unguarded.
func TestABlockedRemovalInsideAnOrphanDoesNotSpendTheBudget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so no removal would fail")
	}
	d, cfg := syncDriver(t)
	root := seedLocal(t, []string{"Gone/Film.strm"}, []string{"Gone"})

	gone := filepath.Join(root, "Gone")
	if err := os.Chmod(gone, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(gone, 0o755) })

	before := d.deleteBudget.Load()
	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}

	if _, err := os.Lstat(filepath.Join(gone, "Film.strm")); err != nil {
		t.Fatalf("the removal succeeded after all, so this test proves nothing: %v", err)
	}
	if got := d.deleteBudget.Load(); got != before {
		t.Errorf("budget = %d, want %d: nothing was removed, so nothing should have been charged", got, before)
	}
}

// Deepest-first is what lets a subtree of nothing but strm files disappear in
// one pass. Shallowest-first leaves empty shells behind for the media server to
// show.
func TestOrphanPruneCollapsesAPureStrmSubtreeInOnePass(t *testing.T) {
	d, cfg := syncDriver(t)
	root := seedLocal(t, []string{"Gone/Extras/Bonus.strm"}, []string{"Gone/Extras"})

	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "Gone")); !os.IsNotExist(err) {
		t.Errorf("the whole subtree should have collapsed, but Gone survives (stat error = %v)", err)
	}
	if got := d.dirsDeleted.Load(); got != 2 {
		t.Errorf("dirsDeleted = %d, want 2 (Extras and Gone)", got)
	}
}

// Once a pass has given up on deleting it must stay given up, even if refunds
// push the budget back above zero.
func TestDeletionsStayOffEvenAfterARefundRestoresTheBudget(t *testing.T) {
	d, cfg := syncDriver(t)
	d.deleteBudget.Store(1)

	spent := seedLocal(t, []string{"a.strm", "b.strm", "c.strm"}, nil)
	if err := d.deleteExtraAt(context.Background(), cfg, spent, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if !d.deletionsOff.Load() {
		t.Fatal("deletions were not latched off")
	}

	d.deleteBudget.Store(100) // as a refund elsewhere in the pass could
	other := seedLocal(t, []string{"stale.strm"}, nil)
	if err := d.deleteExtraAt(context.Background(), cfg, other, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(other, "stale.strm")); err != nil {
		t.Errorf("deleting resumed after the latch was set: %v", err)
	}
}

// The operator asked for the safety net to be off. Leaving the pass budget in
// place would stop them at 200 entries with no way to say otherwise.
func TestDisableDeleteProtectAlsoBypassesThePassBudget(t *testing.T) {
	d, cfg := syncDriver(t, func(a *Addition) { a.DisableDeleteProtect = true })
	d.deleteBudget.Store(1)

	root := seedLocal(t, names("movie-", 60, ".strm"), nil)
	if err := d.deleteExtraAt(context.Background(), cfg, root, nil); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if got := len(entriesOf(t, root)); got != 0 {
		t.Fatalf("files remaining = %d, want 0: the budget still applied with protection disabled", got)
	}
}

// walk must not spend a listing call on every media file it passes.
func TestWalkDoesNotDescendIntoFiles(t *testing.T) {
	d, cfg := newWalkDriver(t)
	st := &scanStats{}
	list := func(ctx context.Context, mountPath string) ([]model.Obj, error) {
		if mountPath == "/" {
			return remoteObjs([]string{"a.strm", "b.strm"}, []string{"sub"}), nil
		}
		return nil, nil
	}

	if err := d.walk(context.Background(), cfg, "/", 0, list, nil, st); err != nil {
		t.Fatalf("walk() error = %v", err)
	}
	if st.scannedDirs != 2 {
		t.Errorf("scannedDirs = %d, want 2: walk listed something that is not a directory", st.scannedDirs)
	}
}

// The mount root of a multi-source storage is a folder per configured source.
func TestListPathsAtTheMountRootListsOneFolderPerSource(t *testing.T) {
	_, cfg := syncDriver(t, func(a *Addition) { a.Paths = "aliyun:/a\nquark:/q" })

	objs, failed, err := cfg.listPaths(context.Background(), "/", false)
	if err != nil {
		t.Fatalf("listPaths() error = %v", err)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	got := objNames(objs)
	if want := []string{"aliyun", "quark"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for _, o := range objs {
		if !o.IsDir() {
			t.Errorf("%s is not a folder", o.GetName())
		}
	}
}

// The predicate that decides what gets deleted. A source that lacks the path is
// ordinary when several sources are merged under one key; a source that could
// not answer is not, and reading one as the other deletes files that still
// exist upstream.
func TestCountsAsSourceFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error", err: nil, want: false},
		{name: "object not found", err: errs.ObjectNotFound, want: false},
		{name: "object not found, wrapped", err: errors.Join(errs.ObjectNotFound, errors.New("context")), want: false},
		{name: "storage not found", err: errs.StorageNotFound, want: true},
		{name: "rate limited", err: errors.New("429 too many requests"), want: true},
		{name: "not a folder", err: errs.NotFolder, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countsAsSourceFailure(tc.err); got != tc.want {
				t.Errorf("countsAsSourceFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// And end to end: a source whose storage does not exist at all is counted, so
// walk skips the directory rather than letting it drive a deletion.
func TestListPathsCountsAnUnreachableSourceAsFailed(t *testing.T) {
	_, cfg := syncDriver(t, func(a *Addition) { a.Paths = "movies:/no-such-a\nmovies:/no-such-b" })

	_, failed, err := cfg.listPaths(context.Background(), "/movies/Inception", false)
	if err != nil {
		t.Fatalf("listPaths() error = %v", err)
	}
	if failed != 2 {
		t.Errorf("failed = %d, want 2: both sources were unreachable", failed)
	}
}

// A sibling mount that shares a prefix is not inside us.
func TestBuildConfigAcceptsASiblingMountWithASharedPrefix(t *testing.T) {
	a := &Addition{Paths: "/mnt2/movies", SiteUrl: "https://pan.example.com", LocalPath: "/tmp/strm"}
	if _, err := buildConfig(a, "/mnt"); err != nil {
		t.Fatalf("buildConfig() rejected a sibling mount: %v", err)
	}
}

// A source that contains our own mount recurses just as surely as one inside it.
func TestBuildConfigRejectsASourceThatContainsOurMount(t *testing.T) {
	a := &Addition{Paths: "everything:/", SiteUrl: "https://pan.example.com", LocalPath: "/tmp/strm"}
	if _, err := buildConfig(a, "/strm"); err == nil {
		t.Fatal("buildConfig() accepted a source path that contains the storage's own mount")
	}
}

// --- boundaries ------------------------------------------------------------

func TestBoundaries(t *testing.T) {
	t.Run("a file exactly at the size floor is kept", func(t *testing.T) {
		_, cfg := syncDriver(t, func(a *Addition) { a.MinFileSize = 1 })
		got := objNames(cfg.convert2strmObjs(context.Background(), "/src", sourceObjs(t, "Exact.mkv|1048576")))
		if len(got) != 1 {
			t.Fatalf("names = %v, want the file at exactly 1 MB to be kept", got)
		}
	})

	t.Run("a batch that exactly exhausts the budget still runs", func(t *testing.T) {
		d, cfg := syncDriver(t)
		d.deleteBudget.Store(3)
		root := seedLocal(t, []string{"a.strm", "b.strm", "c.strm"}, nil)

		if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
			t.Fatalf("deleteExtra() error = %v", err)
		}
		if got := len(entriesOf(t, root)); got != 0 {
			t.Errorf("files remaining = %d, want 0: a batch equal to the remaining budget must be allowed", got)
		}
		if d.deletionsOff.Load() {
			t.Error("spending the budget exactly should not latch deletions off")
		}
	})

	t.Run("a strm exactly at the depth limit is pruned", func(t *testing.T) {
		d, cfg := syncDriver(t)
		deep := "Gone"
		for i := 0; i < maxScanDepth; i++ {
			deep = filepath.Join(deep, "d")
		}
		root := seedLocal(t, []string{filepath.Join(deep, "Edge.strm")}, []string{deep})

		if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"anchor.strm"}, nil)); err != nil {
			t.Fatalf("deleteExtra() error = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, deep, "Edge.strm")); !os.IsNotExist(err) {
			t.Errorf("the strm at exactly the depth limit survived, stat error = %v", err)
		}
	})

	t.Run("a source path without a leading slash still yields one", func(t *testing.T) {
		_, cfg := syncDriver(t, func(a *Addition) { a.PathPrefix = ""; a.EncodePath = false })
		got := cfg.getLink(context.Background(), "src/Movies/Film.mkv")
		if want := "https://pan.example.com/src/Movies/Film.mkv"; got != want {
			t.Errorf("getLink() = %s, want %s", got, want)
		}
	})
}

// --- attachments -----------------------------------------------------------

// downloadAttachment had no coverage at all: the entire function could be
// emptied out with the suite still green. It is also the only path that spends
// real API quota -- on Aliyun every call shares a 0.9/s limiter with playback --
// so "how often does it refetch" is a property worth pinning.
func TestDownloadAttachment(t *testing.T) {
	const body = "1\n00:00:01,000 --> 00:00:02,000\nhello\n"

	newDriver := func(t *testing.T, mode string) (*StrmSync, *scanConfig, *int) {
		t.Helper()
		d, cfg := syncDriver(t, func(a *Addition) {
			a.LocalMode = mode
			a.DownloadSubtitle = true
		})
		calls := 0
		d.linkFn = func(ctx context.Context, path string) (*model.Link, error) {
			calls++
			return &model.Link{
				ContentLength: int64(len(body)),
				RangeReader:   stream.GetRangeReaderFromMFile(int64(len(body)), strings.NewReader(body)),
			}, nil
		}
		return d, cfg, &calls
	}
	attachment := func() model.Obj {
		return &model.Object{Name: "Film.srt", Path: "/src/Movies/Film.srt", Size: int64(len(body))}
	}

	t.Run("downloads a missing attachment", func(t *testing.T) {
		d, cfg, calls := newDriver(t, LocalModeUpdate)
		if err := d.writeLocalAt(context.Background(), cfg, "/Movies", []model.Obj{attachment()}); err != nil {
			t.Fatalf("writeLocal() error = %v", err)
		}
		got, err := os.ReadFile(filepath.Join(cfg.localPath, "Movies", "Film.srt"))
		if err != nil {
			t.Fatalf("read attachment: %v", err)
		}
		if string(got) != body {
			t.Errorf("attachment = %q, want %q", got, body)
		}
		if *calls != 1 {
			t.Errorf("link calls = %d, want 1", *calls)
		}
	})

	t.Run("does not refetch when the size already matches", func(t *testing.T) {
		d, cfg, calls := newDriver(t, LocalModeUpdate)
		objs := []model.Obj{attachment()}
		for i := 0; i < 3; i++ {
			if err := d.writeLocalAt(context.Background(), cfg, "/Movies", objs); err != nil {
				t.Fatalf("writeLocal() error = %v", err)
			}
		}
		if *calls != 1 {
			t.Errorf("link calls = %d after three passes, want 1: every extra call is real API quota", *calls)
		}
	})

	t.Run("refetches a truncated attachment", func(t *testing.T) {
		d, cfg, calls := newDriver(t, LocalModeUpdate)
		dir := filepath.Join(cfg.localPath, "Movies")
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatalf("seed dir: %v", err)
		}
		target := filepath.Join(dir, "Film.srt")
		if err := os.WriteFile(target, []byte("half"), 0o644); err != nil {
			t.Fatalf("seed truncated file: %v", err)
		}

		if err := d.writeLocalAt(context.Background(), cfg, "/Movies", []model.Obj{attachment()}); err != nil {
			t.Fatalf("writeLocal() error = %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read attachment: %v", err)
		}
		if string(got) != body {
			t.Errorf("attachment = %q, want the refetched body", got)
		}
		if *calls != 1 {
			t.Errorf("link calls = %d, want 1", *calls)
		}
	})

	t.Run("insert mode never touches an existing attachment", func(t *testing.T) {
		d, cfg, calls := newDriver(t, LocalModeInsert)
		dir := filepath.Join(cfg.localPath, "Movies")
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatalf("seed dir: %v", err)
		}
		target := filepath.Join(dir, "Film.srt")
		if err := os.WriteFile(target, []byte("hand written"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}

		if err := d.writeLocalAt(context.Background(), cfg, "/Movies", []model.Obj{attachment()}); err != nil {
			t.Fatalf("writeLocal() error = %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read attachment: %v", err)
		}
		if string(got) != "hand written" {
			t.Errorf("insert mode overwrote an existing attachment: %q", got)
		}
		if *calls != 0 {
			t.Errorf("link calls = %d, want 0", *calls)
		}
	})

	t.Run("a failed link leaves nothing behind", func(t *testing.T) {
		d, cfg := syncDriver(t, func(a *Addition) {
			a.LocalMode = LocalModeUpdate
			a.DownloadSubtitle = true
		})
		d.linkFn = func(ctx context.Context, path string) (*model.Link, error) {
			return nil, errors.New("429 too many requests")
		}
		if err := d.writeLocalAt(context.Background(), cfg, "/Movies", []model.Obj{attachment()}); err != nil {
			t.Fatalf("writeLocal() error = %v", err)
		}
		if got := entriesOf(t, filepath.Join(cfg.localPath, "Movies")); len(got) != 0 {
			t.Errorf("directory holds %v, want nothing after a failed link", got)
		}
	})
}

// --- leftover temporaries --------------------------------------------------

// The temporary has to be hidden, and it must not look like something this
// storage manages: if the suffix landed in the managed set a later pass would
// count it against the cap and delete it as its own output.
func TestWriteFileAtomicTemporaryIsHiddenAndUnmanaged(t *testing.T) {
	_, cfg := syncDriver(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "Film.strm")

	var seen []string
	err := writeFileAtomic(target, func(w io.Writer) error {
		seen = entriesOf(t, dir)
		_, err := w.Write([]byte("x"))
		return err
	})
	if err != nil {
		t.Fatalf("writeFileAtomic() error = %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("directory held %v mid-write, want exactly the temporary", seen)
	}
	tmp := seen[0]
	if !strings.HasPrefix(tmp, ".") {
		t.Errorf("temporary %q is not hidden; a media server would index it", tmp)
	}
	if cfg.isManaged(tmp) {
		t.Errorf("temporary %q is in the managed set; a later pass would treat it as its own output", tmp)
	}
	if !isOurTemp(tmp) {
		t.Errorf("temporary %q is not recognisable as ours, so it could never be cleaned up", tmp)
	}
}

// A process killed between create and rename leaves a temporary behind. Nothing
// else in this package would ever remove it, and while it sits there the
// directory can never be seen as empty -- so it would pin an orphan forever.
func TestSyncClearsAStaleTemporary(t *testing.T) {
	d, cfg := syncDriver(t)
	root := seedLocal(t, nil, nil)

	stale := filepath.Join(root, tempNameFor("Film.strm"))
	if err := os.WriteFile(stale, []byte("half"), 0o644); err != nil {
		t.Fatalf("seed stale temporary: %v", err)
	}
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age the temporary: %v", err)
	}

	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale temporary survived, stat error = %v", err)
	}
}

// A temporary another instance is writing right now must survive.
func TestSyncLeavesAFreshTemporaryAlone(t *testing.T) {
	d, cfg := syncDriver(t)
	root := seedLocal(t, nil, nil)

	fresh := filepath.Join(root, tempNameFor("Film.strm"))
	if err := os.WriteFile(fresh, []byte("in flight"), 0o644); err != nil {
		t.Fatalf("seed temporary: %v", err)
	}

	if err := d.deleteExtraAt(context.Background(), cfg, root, remoteObjs([]string{"keep.strm"}, nil)); err != nil {
		t.Fatalf("deleteExtra() error = %v", err)
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Errorf("a temporary that could still be in flight was deleted: %v", err)
	}
}

// Two writers aiming at the same target must not share a temporary path.
func TestWriteFileAtomicTemporariesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		name := tempNameFor("Film.strm")
		if seen[name] {
			t.Fatalf("temporary name %q was handed out twice; two instances writing the same localPath would corrupt each other", name)
		}
		seen[name] = true
	}
}

// An existing file's permissions must survive an update: rename would otherwise
// quietly reset whatever an administrator had set.
func TestWriteFileAtomicKeepsTheTargetMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root makes the mode assertions meaningless")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "Film.strm")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := writeFileAtomic(target, func(w io.Writer) error {
		_, err := w.Write([]byte("new"))
		return err
	}); err != nil {
		t.Fatalf("writeFileAtomic() error = %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %v, want 0640: the rename reset permissions", got)
	}
}

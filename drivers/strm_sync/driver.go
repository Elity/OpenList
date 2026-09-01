package strm_sync

import (
	"context"
	"errors"
	"fmt"
	stdpath "path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/cron"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
)

// StrmSync mirrors one or more source paths into a local strm tree and keeps it
// up to date on a schedule.
//
// It is a fork-local sibling of the upstream Strm driver rather than a change to
// it. The important structural difference: upstream writes files from inside the
// ObjsUpdateHook, i.e. on a goroutine op spawns with context.WithoutCancel, so
// the write is uncancellable, unbounded, and races Init/Drop through a shared
// global trie. Here the scan lists its own mount path and writes what it gets,
// synchronously, on its own goroutine. No hook is registered.
//
// Everything derived from the stored configuration lives behind cfg, an
// immutable snapshot swapped wholesale by Init. See scanConfig for why.
type StrmSync struct {
	model.Storage
	Addition

	cfg atomic.Pointer[scanConfig]

	// Scan state. None of this is serialized.
	scanMu       sync.Mutex // guards cron / scanCancel
	cron         *cron.Cron
	scanCancel   context.CancelFunc
	scanning     atomic.Bool
	deleteBudget atomic.Int64
	// deletionsOff latches once a pass has spent its deletion budget. The pass
	// keeps walking and keeps writing; only deleting stops.
	deletionsOff atomic.Bool

	strmWritten   atomic.Int64
	filesDeleted  atomic.Int64
	dirsDeleted   atomic.Int64
	deleteBlocked atomic.Int64
}

func (d *StrmSync) Config() driver.Config {
	return config
}

func (d *StrmSync) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *StrmSync) Init(ctx context.Context) error {
	cfg, err := buildConfig(&d.Addition, d.MountPath)
	if err != nil {
		return err
	}
	d.cfg.Store(cfg)
	d.startScan(cfg)
	return nil
}

func (d *StrmSync) Drop(ctx context.Context) error {
	d.stopScan()
	return nil
}

// config returns the active snapshot, or an error if the storage is not
// initialised. Every exported method starts here and then reads only from the
// returned pointer.
func (d *StrmSync) config() (*scanConfig, error) {
	if cfg := d.cfg.Load(); cfg != nil {
		return cfg, nil
	}
	return nil, errors.New("storage is not initialised")
}

func (d *StrmSync) Get(ctx context.Context, path string) (model.Obj, error) {
	cfg, err := d.config()
	if err != nil {
		return nil, err
	}
	root, sub := cfg.getRootAndPath(path)
	dsts, ok := cfg.pathMap[root]
	if !ok {
		return nil, errs.ObjectNotFound
	}
	for _, dst := range dsts {
		reqPath := stdpath.Join(dst, sub)
		obj, err := fs.Get(ctx, reqPath, &fs.GetArgs{NoLog: true})
		if err != nil {
			continue
		}
		size := int64(0)
		if !obj.IsDir() {
			size = obj.GetSize()
			path = reqPath // point at the real path so Link can read it directly
		}
		return &model.Object{
			Path:     path,
			Name:     obj.GetName(),
			Size:     size,
			Modified: obj.ModTime(),
			IsFolder: obj.IsDir(),
			HashInfo: obj.GetHash(),
		}, nil
	}
	if strings.HasSuffix(path, ".strm") {
		// Nothing on disk and the caller wants a .strm: report NotSupport so
		// op.Get falls back to looking it up through op.List.
		return nil, errs.NotSupport
	}
	return nil, errs.ObjectNotFound
}

func (d *StrmSync) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	cfg, err := d.config()
	if err != nil {
		return nil, err
	}
	objs, _, err := cfg.listPaths(ctx, dir.GetPath(), args.Refresh)
	return objs, err
}

func (d *StrmSync) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	cfg, err := d.config()
	if err != nil {
		return nil, err
	}
	if file.GetID() == "strm" {
		link := cfg.getLink(ctx, file.GetPath())
		return &model.Link{
			RangeReader: stream.GetRangeReaderFromMFile(int64(len(link)), strings.NewReader(link)),
		}, nil
	}
	// ftp,s3
	if common.GetApiUrl(ctx) == "" {
		args.Redirect = false
	}
	reqPath := file.GetPath()
	link, _, err := linkTo(ctx, reqPath, args)
	if err != nil {
		return nil, err
	}
	if link == nil {
		return &model.Link{
			URL: fmt.Sprintf("%s/p%s?sign=%s",
				common.GetApiUrl(ctx),
				utils.EncodePath(reqPath, true),
				sign.Sign(reqPath)),
		}, nil
	}
	return link.Clone(), nil
}

var (
	_ driver.Driver = (*StrmSync)(nil)
	_ driver.Getter = (*StrmSync)(nil)
)

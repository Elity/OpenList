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
type StrmSync struct {
	model.Storage
	Addition

	pathMap     map[string][]string
	autoFlatten bool
	oneKey      string

	supportSuffix  map[string]struct{}
	downloadSuffix map[string]struct{}
	managedSuffix  map[string]struct{}
	minSizeBytes   int64

	// Scan state. None of this is serialized: only Addition is.
	scanMu       sync.Mutex // guards cron / scanCtx / scanCancel
	cron         *cron.Cron
	scanCtx      context.Context
	scanCancel   context.CancelFunc
	scanning     atomic.Bool
	deleteBudget atomic.Int64

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
	if d.Paths == "" {
		return errors.New("paths is required")
	}
	if d.LocalPath == "" {
		return errors.New("localPath is required")
	}
	// The required tag only drives the form; nothing enforces it server side.
	// Without SiteUrl a scheduled scan would emit host-less strm files, because
	// common.GetApiUrl reads the value off an HTTP request context that a
	// background scan does not have.
	if !d.WithoutUrl && d.SiteUrl == "" {
		return errors.New("siteUrl is required: a scheduled scan has no request context to derive the api url from")
	}

	d.pathMap = make(map[string][]string)
	for path := range strings.SplitSeq(d.Paths, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		k, v := getPair(path)
		d.pathMap[k] = append(d.pathMap[k], v)
	}
	if len(d.pathMap) == 0 {
		return errors.New("paths is required")
	}
	if len(d.pathMap) == 1 {
		for k := range d.pathMap {
			d.oneKey = k
		}
		d.autoFlatten = true
	} else {
		d.oneKey = ""
		d.autoFlatten = false
	}

	if d.FilterFileTypes == "" {
		d.FilterFileTypes = "mp4,mkv,flv,avi,wmv,ts,rmvb,webm,mp3,flac,aac,wav,ogg,m4a,wma,alac"
	}
	d.supportSuffix = parseSuffixes(d.FilterFileTypes)
	d.downloadSuffix = d.buildDownloadSuffix()

	// Everything this storage may create locally, and therefore everything it
	// is allowed to delete. A scraper's .nfo/.jpg never enter this set unless
	// the operator asked for them, so sync mode cannot touch them.
	d.managedSuffix = map[string]struct{}{"strm": {}}
	for ext := range d.downloadSuffix {
		d.managedSuffix[ext] = struct{}{}
	}

	if len(d.LocalMode) == 0 {
		d.LocalMode = LocalModeInsert
	}
	d.minSizeBytes = d.MinFileSize * 1024 * 1024

	d.startScan()
	return nil
}

func (d *StrmSync) Drop(ctx context.Context) error {
	d.stopScan()
	return nil
}

func parseSuffixes(list string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, ext := range strings.Split(list, ",") {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext != "" {
			out[ext] = struct{}{}
		}
	}
	return out
}

func (d *StrmSync) buildDownloadSuffix() map[string]struct{} {
	out := map[string]struct{}{}
	merge := func(list string) {
		for ext := range parseSuffixes(list) {
			out[ext] = struct{}{}
		}
	}
	if d.DownloadSubtitle {
		merge(subtitleTypes)
	}
	if d.DownloadImage {
		merge(imageTypes)
	}
	if d.DownloadNfo {
		merge(nfoTypes)
	}
	merge(d.DownloadExtraTypes)
	return out
}

func (d *StrmSync) Get(ctx context.Context, path string) (model.Obj, error) {
	root, sub := d.getRootAndPath(path)
	dsts, ok := d.pathMap[root]
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
	path := dir.GetPath()
	if utils.PathEqual(path, "/") && !d.autoFlatten {
		return d.listRoot(), nil
	}
	root, sub := d.getRootAndPath(path)
	dsts, ok := d.pathMap[root]
	if !ok {
		return nil, errs.ObjectNotFound
	}
	var objs []model.Obj
	fsArgs := &fs.ListArgs{NoLog: true, Refresh: args.Refresh}
	for _, dst := range dsts {
		tmp, err := d.list(ctx, dst, sub, fsArgs)
		if err == nil {
			objs = append(objs, tmp...)
		}
	}
	return objs, nil
}

func (d *StrmSync) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if file.GetID() == "strm" {
		link := d.getLink(ctx, file.GetPath())
		return &model.Link{
			RangeReader: stream.GetRangeReaderFromMFile(int64(len(link)), strings.NewReader(link)),
		}, nil
	}
	// ftp,s3
	if common.GetApiUrl(ctx) == "" {
		args.Redirect = false
	}
	reqPath := file.GetPath()
	link, _, err := d.link(ctx, reqPath, args)
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

var _ driver.Driver = (*StrmSync)(nil)

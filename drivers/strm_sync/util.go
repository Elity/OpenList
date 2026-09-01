package strm_sync

import (
	"context"
	"fmt"
	stdpath "path"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
)

// The path resolution here is adapted from drivers/strm/util.go (upstream
// revision 394bb8f8). The bodies are kept line-comparable with upstream so a
// later fix there can be read across; what changed is that the receiver is the
// immutable snapshot rather than the live driver, and that listPaths reports
// how many sources failed instead of swallowing it.
//
// The revision they were taken from is recorded in UPSTREAM_BASELINE, and
// .github/workflows/fork-upstream-drift.yml fails when upstream moves them past
// it. See README.md.

func (c *scanConfig) listRoot() []model.Obj {
	var objs []model.Obj
	for k := range c.pathMap {
		obj := model.Object{
			Path:     "/" + k,
			Name:     k,
			IsFolder: true,
		}
		objs = append(objs, &obj)
	}
	return objs
}

func getPair(path string) (string, string) {
	if strings.Contains(path, ":") {
		pair := strings.SplitN(path, ":", 2)
		if !strings.Contains(pair[0], "/") {
			return pair[0], pair[1]
		}
	}
	return stdpath.Base(path), path
}

func (c *scanConfig) getRootAndPath(path string) (string, string) {
	if c.autoFlatten {
		return c.oneKey, path
	}
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

// listPaths resolves one mount path and returns the converted objects plus the
// number of configured sources that failed to list.
//
// That count is the whole reason this is not inlined into List. Upstream drops
// per-source errors on the floor, which is harmless when the result only feeds a
// directory listing in a browser. Here the result also decides what gets
// deleted from disk, and a source that is merely rate-limited must never be
// mistaken for a source that no longer has the file.
func (c *scanConfig) listPaths(ctx context.Context, path string, refresh bool) ([]model.Obj, int, error) {
	if utils.PathEqual(path, "/") && !c.autoFlatten {
		return c.listRoot(), 0, nil
	}
	root, sub := c.getRootAndPath(path)
	dsts, ok := c.pathMap[root]
	if !ok {
		return nil, 0, errs.ObjectNotFound
	}

	var objs []model.Obj
	failed := 0
	args := &fs.ListArgs{NoLog: true, Refresh: refresh}
	for _, dst := range dsts {
		reqPath := stdpath.Join(dst, sub)
		tmp, err := fs.List(ctx, reqPath, args)
		if err != nil {
			if countsAsSourceFailure(err) {
				failed++
			}
			continue
		}
		objs = append(objs, c.convert2strmObjs(ctx, reqPath, tmp)...)
	}
	return objs, failed, nil
}

// countsAsSourceFailure reports whether a listing error means the source could
// not answer, rather than simply not having the path.
//
// The distinction matters because the answer decides what gets deleted from
// disk. Merging several sources under one key is exactly what pathMap's slice
// is for, and only one of them needs to hold any given subdirectory, so an
// absent object is ordinary. Anything else -- rate limiting, a revoked token, a
// storage that is down or gone -- must be counted, or the deletion pass reads
// it as "the file is gone".
func countsAsSourceFailure(err error) bool {
	return err != nil && !errs.IsObjectNotFound(err)
}

// convert2strmObjs turns a source listing into the objects this driver exposes:
// media files become <name>.strm carrying an ID of "strm", attachment files are
// passed through untouched, everything else is dropped.
//
// Note for callers: directory entries come back with an empty Path. Walk the
// tree by joining names onto the mount path, never by reading obj.GetPath().
func (c *scanConfig) convert2strmObjs(ctx context.Context, reqPath string, objs []model.Obj) []model.Obj {
	var validObjs []model.Obj
	for _, obj := range objs {
		id, name, path := "", obj.GetName(), ""
		size := int64(0)
		if !obj.IsDir() {
			path = stdpath.Join(reqPath, obj.GetName())
			sourceExt := utils.SourceExt(name)
			ext := strings.ToLower(sourceExt)
			if _, ok := c.downloadSuffix[ext]; ok {
				size = obj.GetSize()
			} else if _, ok := c.supportSuffix[ext]; ok {
				if c.minSizeBytes > 0 && obj.GetSize() < c.minSizeBytes {
					continue
				}
				id = "strm"
				name = strings.TrimSuffix(name, sourceExt) + "strm"
				size = int64(len(c.getLink(ctx, path)))
			} else {
				continue
			}
		}
		objRes := model.Object{
			ID:       id,
			Path:     path,
			Name:     name,
			Size:     size,
			Modified: obj.ModTime(),
			IsFolder: obj.IsDir(),
		}
		thumb, ok := model.GetThumb(obj)
		if !ok {
			validObjs = append(validObjs, &objRes)
			continue
		}
		validObjs = append(validObjs, &model.ObjThumb{
			Object:    objRes,
			Thumbnail: model.Thumbnail{Thumbnail: thumb},
		})
	}
	return validObjs
}

// getLink builds the strm file body. It is pure string work -- sign.Sign is a
// local HMAC -- so generating a strm costs no network at all.
//
// siteUrl is required by this driver precisely because of the fallback below:
// a scheduled scan has no HTTP request in its context, so common.GetApiUrl
// would return "" and every generated strm would lose its host prefix.
func (c *scanConfig) getLink(ctx context.Context, path string) string {
	finalPath := path
	if c.encodePath {
		finalPath = utils.EncodePath(path, true)
	}
	if c.withSign {
		signPath := sign.Sign(path)
		finalPath = fmt.Sprintf("%s?sign=%s", finalPath, signPath)
	}
	if len(c.pathPrefix) > 0 {
		finalPath = stdpath.Join(c.pathPrefix, finalPath)
	}
	if !strings.HasPrefix(finalPath, "/") {
		finalPath = "/" + finalPath
	}
	if c.withoutUrl {
		return finalPath
	}
	apiUrl := c.siteUrl
	if len(apiUrl) > 0 {
		apiUrl = strings.TrimSuffix(apiUrl, "/")
	} else {
		apiUrl = common.GetApiUrl(ctx)
	}
	return apiUrl + finalPath
}

func linkTo(ctx context.Context, reqPath string, args model.LinkArgs) (*model.Link, model.Obj, error) {
	storage, reqActualPath, err := op.GetStorageAndActualPath(reqPath)
	if err != nil {
		return nil, nil, err
	}
	if !args.Redirect {
		return op.Link(ctx, storage, reqActualPath, args)
	}
	obj, err := fs.Get(ctx, reqPath, &fs.GetArgs{NoLog: true})
	if err != nil {
		return nil, nil, err
	}
	if common.ShouldProxy(storage, stdpath.Base(reqPath)) {
		return nil, obj, nil
	}
	return op.Link(ctx, storage, reqActualPath, args)
}

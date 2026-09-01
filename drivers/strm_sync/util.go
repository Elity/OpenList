package strm_sync

import (
	"context"
	"fmt"
	stdpath "path"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
)

// This file is a copy of drivers/strm/util.go with the receiver renamed. The
// path resolution here is the fiddly part of the upstream driver and is worth
// keeping identical; see the fork notes in scan.go for what we did change.

func (d *StrmSync) listRoot() []model.Obj {
	var objs []model.Obj
	for k := range d.pathMap {
		obj := model.Object{
			Path:     "/" + k,
			Name:     k,
			IsFolder: true,
			Modified: d.Modified,
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

func (d *StrmSync) getRootAndPath(path string) (string, string) {
	if d.autoFlatten {
		return d.oneKey, path
	}
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func (d *StrmSync) list(ctx context.Context, dst, sub string, args *fs.ListArgs) ([]model.Obj, error) {
	reqPath := stdpath.Join(dst, sub)
	objs, err := fs.List(ctx, reqPath, args)
	if err != nil {
		return nil, err
	}
	return d.convert2strmObjs(ctx, reqPath, objs), nil
}

// convert2strmObjs turns a source listing into the objects this driver exposes:
// media files become <name>.strm carrying an ID of "strm", attachment files are
// passed through untouched, everything else is dropped.
//
// Note for callers: directory entries come back with an empty Path. Walk the
// tree by joining names onto the mount path, never by reading obj.GetPath().
func (d *StrmSync) convert2strmObjs(ctx context.Context, reqPath string, objs []model.Obj) []model.Obj {
	var validObjs []model.Obj
	for _, obj := range objs {
		id, name, path := "", obj.GetName(), ""
		size := int64(0)
		if !obj.IsDir() {
			path = stdpath.Join(reqPath, obj.GetName())
			sourceExt := utils.SourceExt(name)
			ext := strings.ToLower(sourceExt)
			if _, ok := d.downloadSuffix[ext]; ok {
				size = obj.GetSize()
			} else if _, ok := d.supportSuffix[ext]; ok {
				if d.minSizeBytes > 0 && obj.GetSize() < d.minSizeBytes {
					continue
				}
				id = "strm"
				name = strings.TrimSuffix(name, sourceExt) + "strm"
				size = int64(len(d.getLink(ctx, path)))
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
// SiteUrl is required by this driver precisely because of the fallback below:
// a scheduled scan has no HTTP request in its context, so common.GetApiUrl
// would return "" and every generated strm would lose its host prefix.
func (d *StrmSync) getLink(ctx context.Context, path string) string {
	finalPath := path
	if d.EncodePath {
		finalPath = utils.EncodePath(path, true)
	}
	if d.WithSign {
		signPath := sign.Sign(path)
		finalPath = fmt.Sprintf("%s?sign=%s", finalPath, signPath)
	}
	if len(d.PathPrefix) > 0 {
		finalPath = stdpath.Join(d.PathPrefix, finalPath)
	}
	if !strings.HasPrefix(finalPath, "/") {
		finalPath = "/" + finalPath
	}
	if d.WithoutUrl {
		return finalPath
	}
	apiUrl := d.SiteUrl
	if len(apiUrl) > 0 {
		apiUrl = strings.TrimSuffix(apiUrl, "/")
	} else {
		apiUrl = common.GetApiUrl(ctx)
	}
	return apiUrl + finalPath
}

func (d *StrmSync) link(ctx context.Context, reqPath string, args model.LinkArgs) (*model.Link, model.Obj, error) {
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

package strm_sync

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// scanConfig is an immutable view of one storage's configuration.
//
// Nothing outside buildConfig ever writes to it, and every read goes through a
// pointer loaded once at the top of an operation. That indirection is not
// decoration: internal/op/storage.go:111 unmarshals the stored Addition JSON
// straight over the live driver struct and only then calls Init, so a scan pass
// that has not finished unwinding would otherwise read those fields -- and the
// maps derived from them -- while they are being rewritten.
//
// For the maps that is not merely a torn read. A map read racing a map write is
// `fatal error: concurrent map read and map write`, which is not a panic and
// which no recover() can catch: the process dies.
type scanConfig struct {
	mountPath string

	pathMap     map[string][]string
	autoFlatten bool
	oneKey      string

	supportSuffix  map[string]struct{}
	downloadSuffix map[string]struct{}
	minSizeBytes   int64

	localPath string
	localMode string

	siteUrl    string
	pathPrefix string
	encodePath bool
	withSign   bool
	withoutUrl bool

	deleteCap            int
	disableDeleteProtect bool

	scanInterval time.Duration
	scanOnInit   bool
	scanRate     float64
}

func buildConfig(a *Addition, mountPath string) (*scanConfig, error) {
	if a.Paths == "" {
		return nil, errors.New("paths is required")
	}
	if a.LocalPath == "" {
		return nil, errors.New("localPath is required")
	}
	// The required tag only drives the form; nothing enforces it server side.
	// Without SiteUrl a scheduled scan would emit host-less strm files, because
	// common.GetApiUrl reads the value off an HTTP request context that a
	// background scan does not have.
	if !a.WithoutUrl && a.SiteUrl == "" {
		return nil, errors.New("siteUrl is required: a scheduled scan has no request context to derive the api url from")
	}
	if !filepath.IsAbs(a.LocalPath) {
		return nil, fmt.Errorf("localPath must be absolute, got %q", a.LocalPath)
	}
	if parent := filepath.Dir(filepath.Clean(a.LocalPath)); parent == filepath.Clean(a.LocalPath) {
		return nil, fmt.Errorf("localPath must not be a filesystem root, got %q", a.LocalPath)
	}

	cfg := &scanConfig{
		mountPath:            mountPath,
		pathMap:              make(map[string][]string),
		minSizeBytes:         a.MinFileSize * 1024 * 1024,
		localPath:            filepath.Clean(a.LocalPath),
		localMode:            a.LocalMode,
		siteUrl:              a.SiteUrl,
		pathPrefix:           a.PathPrefix,
		encodePath:           a.EncodePath,
		withSign:             a.WithSign,
		withoutUrl:           a.WithoutUrl,
		deleteCap:            a.MaxDeletePerDir,
		disableDeleteProtect: a.DisableDeleteProtect,
		scanInterval:         time.Duration(a.ScanIntervalMinutes) * time.Minute,
		scanOnInit:           a.ScanOnInit,
		scanRate:             a.ScanRateLimitPerSec,
	}
	if cfg.localMode == "" {
		cfg.localMode = LocalModeInsert
	}

	for path := range strings.SplitSeq(a.Paths, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		k, v := getPair(path)
		// A source path that points back into this storage's own mount makes
		// d.List recurse into itself. op.List keys singleflight by path, so the
		// inner call waits on the outer one on the same goroutine and the scan
		// deadlocks; maxScanDepth does not help, it bounds walk and not this.
		if mountPath != "" && (underMount(v, mountPath) || underMount(mountPath, v)) {
			return nil, fmt.Errorf("source path %q is inside this storage's own mount %q", v, mountPath)
		}
		cfg.pathMap[k] = append(cfg.pathMap[k], v)
	}
	if len(cfg.pathMap) == 0 {
		return nil, errors.New("paths is required")
	}
	if len(cfg.pathMap) == 1 {
		for k := range cfg.pathMap {
			cfg.oneKey = k
		}
		cfg.autoFlatten = true
	}

	filter := a.FilterFileTypes
	if filter == "" {
		filter = "mp4,mkv,flv,avi,wmv,ts,rmvb,webm,mp3,flac,aac,wav,ogg,m4a,wma,alac"
	}
	cfg.supportSuffix = parseSuffixes(filter)
	cfg.downloadSuffix = buildDownloadSuffix(a)
	return cfg, nil
}

// underMount reports whether path is mount, or sits inside it. Both sides are
// POSIX mount paths.
//
// The trailing separator matters: without it "/mnt2" would read as being inside
// "/mnt". Trimming a trailing slash first means "/" becomes "", which is why the
// prefix test uses mount+"/" -- for a root mount that is "/", and everything is
// inside it, which is the right answer.
func underMount(path, mount string) bool {
	path = strings.TrimSuffix(path, "/")
	mount = strings.TrimSuffix(mount, "/")
	return path == mount || strings.HasPrefix(path+"/", mount+"/")
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

func buildDownloadSuffix(a *Addition) map[string]struct{} {
	out := map[string]struct{}{}
	merge := func(list string) {
		for ext := range parseSuffixes(list) {
			out[ext] = struct{}{}
		}
	}
	if a.DownloadSubtitle {
		merge(subtitleTypes)
	}
	if a.DownloadImage {
		merge(imageTypes)
	}
	if a.DownloadNfo {
		merge(nfoTypes)
	}
	merge(a.DownloadExtraTypes)
	return out
}

// isManaged reports whether a local file is one this storage could have created
// and may therefore delete.
//
// The set is exactly {strm} and does not widen with the attachment switches.
// That is what makes "a scraper's output is never a deletion candidate" a
// property of the code rather than a property of the configuration: turning on
// image downloads must not put an Emby-generated poster.jpg -- which the cloud
// side never had -- into the firing line. The cost is that an attachment whose
// source file disappears is left behind, which is the same trade this driver
// already makes for orphaned directories.
func (c *scanConfig) isManaged(name string) bool {
	return strings.EqualFold(strings.TrimPrefix(filepath.Ext(name), "."), "strm")
}

// maxDeletePerDir is the configured cap, or the default when it is unset.
func (c *scanConfig) maxDeletePerDir() int {
	if c.deleteCap > 0 {
		return c.deleteCap
	}
	return defaultMaxDeletePerDir
}

// localDirFor maps a mount path onto the local tree. Mount paths are POSIX, the
// local side is whatever the OS uses, so the conversion is explicit rather than
// relying on the two happening to agree.
//
// The layout is localPath + the mount-relative path, which is what makes the
// output line up with autofilm's target_dir. It also means two source paths
// sharing a basename land in different directories, unlike the upstream driver
// where both collapse onto SaveStrmLocalPath/<basename>.
func (c *scanConfig) localDirFor(mountPath string) string {
	rel := strings.TrimPrefix(mountPath, "/")
	if rel == "" {
		return c.localPath
	}
	return filepath.Join(c.localPath, filepath.FromSlash(rel))
}

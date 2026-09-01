package strm_sync

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

const (
	LocalModeInsert = "insert"
	LocalModeUpdate = "update"
	LocalModeSync   = "sync"
)

// Attachment groups, mirroring autofilm's download block. Kept as constants
// rather than exposed suffix lists so the common cases stay one checkbox.
const (
	subtitleTypes = "ass,srt,vtt,sub,ssa,smi,idx"
	imageTypes    = "jpg,jpeg,png,webp,bmp,tbn"
	nfoTypes      = "nfo"
)

type Addition struct {
	Paths   string `json:"paths" required:"true" type:"text" help:"One source path per line"`
	SiteUrl string `json:"siteUrl" required:"false" type:"text" help:"URL prefix written into the strm files, e.g. https://pan.example.com. Required unless withoutUrl is set: a scheduled scan has no request to derive it from"`

	PathPrefix      string `json:"pathPrefix" type:"text" required:"false" default:"/d"`
	FilterFileTypes string `json:"filterFileTypes" type:"text" required:"false" default:"mp4,mkv,flv,avi,wmv,ts,rmvb,webm,mp3,flac,aac,wav,ogg,m4a,wma,alac" help:"Media suffixes that become strm files"`
	MinFileSize     int64  `json:"minFileSize" type:"number" required:"false" default:"0" help:"Skip files smaller than this (MB, 0 to disable)"`
	EncodePath      bool   `json:"encodePath" required:"false" default:"true"`
	WithoutUrl      bool   `json:"withoutUrl" required:"false" default:"false"`
	WithSign        bool   `json:"withSign" required:"false" default:"false"`

	LocalPath string `json:"localPath" required:"true" type:"text" help:"Directory the strm tree is written to"`
	LocalMode string `json:"localMode" type:"select" options:"insert,update,sync" default:"insert" help:"insert: never touch existing files. update: rewrite changed ones. sync: also delete files this storage generated that are gone upstream"`

	// One switch per attachment kind. All false means nothing extra is fetched,
	// which is the safe default: every attachment costs a real getDownloadUrl
	// call, and on Aliyun those are limited to 0.9/s and shared with playback.
	DownloadSubtitle   bool   `json:"downloadSubtitle" required:"false" default:"false"`
	DownloadImage      bool   `json:"downloadImage" required:"false" default:"false"`
	DownloadNfo        bool   `json:"downloadNfo" required:"false" default:"false"`
	DownloadExtraTypes string `json:"downloadExtraTypes" type:"text" required:"false" default:"" help:"Extra suffixes to download alongside the strm files"`

	ScanIntervalMinutes int     `json:"scanIntervalMinutes" type:"number" required:"false" default:"0" help:"Full scan interval in minutes, 0 to disable"`
	ScanOnInit          bool    `json:"scanOnInit" required:"false" default:"false" help:"Run one scan after storages finish loading"`
	ScanRateLimitPerSec float64 `json:"scanRateLimitPerSec" type:"float" required:"false" default:"0" help:"Directories listed per second, 0 to rely on the source driver's own limiter"`

	MaxDeletePerDir      int  `json:"maxDeletePerDir" type:"number" required:"false" default:"50" help:"Refuse a sync deletion batch larger than this in one directory, 0 uses the built-in default"`
	DisableDeleteProtect bool `json:"disableDeleteProtect" required:"false" default:"false" help:"Turn off every deletion safety check"`
}

func (Addition) GetRootPath() string {
	return "/"
}

var config = driver.Config{
	Name:        "StrmSync",
	LocalSort:   true,
	OnlyProxy:   true,
	NoCache:     true,
	NoUpload:    true,
	DefaultRoot: "/",
	NoLinkURL:   true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &StrmSync{
			Addition: Addition{
				EncodePath: true,
			},
		}
	})
}

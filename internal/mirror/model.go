// Package mirror 是同步的领域核心（mirror：让本地目录与云端目录互为镜像）：把「本地现状」与「云端记录」比对成一份动作清单，再执行它。
//
// 职责切分（本包最重要的约定）：
//   - plan.go：纯判定。只读本地文件系统与索引，产出 []Op，不写索引、不调云端 API、不改文件；
//   - apply.go：唯一有副作用的地方。建目录 / 上传 / 接管 / 清理 / 下载 / 归档都在这里；
//   - scan.go：把云端目录树拉成一份快照（CloudTree），交给 PlanCloud 判定；
//   - cloud.go：云端目录与批量增删移的封装；
//   - strm.go：.strm 文件的纯函数（解析 / 生成 / 路径换算）。
//
// 判定与执行分离带来两个直接收益：判定可以脱离 115 账号做表驱动单测（plan_test.go），
// 以及「预演（dry-run）」只需要跑 plan 不跑 apply。
package mirror

import (
	"path/filepath"
	"strings"
)

// videoThreshold 视频体积阈值：命中扩展名且 ≥10MB 才按视频处理（避免半成品文件被当正片）。
const videoThreshold = 10 * 1024 * 1024

// OpKind 一个待执行动作的类型。
type OpKind uint8

const (
	OpNormalize OpKind = iota // 修正本地 .strm 的直链（内容指向的 pickcode 未变，只是链接格式变了）
	OpMkdir                   // 创建云端目录
	OpUpload                  // 上传本地文件到云端
	OpAdopt                   // 本地新增 .strm：把 pickcode 指向的云端视频移入目标目录并改名
	OpDownload                // 云端文件落地到本地（视频按开关写 .strm）
	OpRetire                  // 云端视频移入回收目录（可找回）
	OpDrop                    // 删除云端文件或目录
	OpArchive                 // 顶层项移入云端回收目录（下载作用域的收尾动作）
)

// Label 返回动作的可读名称（预演面板与日志用）。
func (k OpKind) Label() string {
	switch k {
	case OpNormalize:
		return "修正 STRM 链接"
	case OpMkdir:
		return "创建云端目录"
	case OpUpload:
		return "上传"
	case OpAdopt:
		return "接管 STRM"
	case OpDownload:
		return "下载到本地"
	case OpRetire:
		return "移入回收站"
	case OpDrop:
		return "删除云端项"
	case OpArchive:
		return "归档到回收站"
	default:
		return "未知动作"
	}
}

// Danger 是否为不可逆或高风险动作（预演面板据此红色标记并二次确认）。
func (k OpKind) Danger() bool {
	return k == OpRetire || k == OpDrop || k == OpArchive
}

// phase 决定同类动作在 apply 中的执行次序（数字小的先做）。
// 先清场、再建目录、再传、最后归档，避免「刚传完就被当冗余删掉」这类自相残杀。
func (k OpKind) phase() int {
	switch k {
	case OpRetire, OpDrop:
		return 0
	case OpMkdir:
		return 1
	case OpUpload, OpAdopt:
		return 2
	case OpDownload, OpNormalize:
		return 3
	case OpArchive:
		return 4
	default:
		return 5
	}
}

// Op 一个待执行的动作。
//
// Fid / PickCode 属于云端内部标识，不回显给前端（json:"-"），也不许进日志。
type Op struct {
	Kind     OpKind `json:"kind"`
	Label    string `json:"label"`
	Path     string `json:"path"`
	Reason   string `json:"reason"`
	Size     int64  `json:"size,omitempty"`
	IsVideo  bool   `json:"-"`
	Fid      string `json:"-"` // 目标云端 FID（清理 / 归档用）
	PickCode string `json:"-"` // 目标云端 pickcode（接管 / 下载用）
	// ReplaceFid 上传完成后需要归档的旧云端视频 FID（同名视频覆盖：仅当确实产生了不同文件时才填）。
	ReplaceFid string `json:"-"`
}

// newOp 构造一个动作（自动补 Label）。
func newOp(kind OpKind, path, reason string) Op {
	return Op{Kind: kind, Label: kind.Label(), Path: path, Reason: reason}
}

// ──── 任务侧配置 ────

// LocalCfg 本地 → 云端方向的行为配置。
type LocalCfg struct {
	ToStrm     bool   // 视频上传后本地替换为 .strm（关 = 保留原视频，纯云端备份）
	ToCache    bool   // 上传后原件移入本地透传缓存（关 = 删除原件）
	ExcludeDir string // 本地扫描要跳过的目录（透传缓存根目录，避免把缓存当新增上传）
}

// CloudCfg 云端 → 本地方向的行为配置。
type CloudCfg struct {
	ToStrm    bool // 视频落地为 .strm（关 = 下载原视频）
	DropStale bool // 删除云端同名冗余（索引 FID 与云端 FID 不一致时）
	Archive   bool // 全部成功后把顶层项移入云端回收目录
}

// ──── 路径 ────

// Paths 一个任务的路径与运行时解析出的 FID。
type Paths struct {
	LocalDir string // 本地同步根（任务级）
	CloudDir string // 云端同步根，115 绝对路径（任务级，可与 LocalDir 不同名）
	CloudFid string // 云端同步根 FID（运行时解析）
	TempFid  string // 全局回收目录 FID（运行时解析一次）
	StrmURL  string // .strm 直链前缀（全局）
	CacheDir string // 本地透传缓存根目录（全局）
}

// RelToRoot 计算 path 相对 root 的部分（去掉前缀与分隔符）；二者相等时返回空串。
func RelToRoot(root, path string, sep byte) string {
	rel := strings.TrimPrefix(filepath.Clean(path), filepath.Clean(root))
	return strings.TrimPrefix(rel, string(sep))
}

// MapCloudToLocal 把云端路径映射为本地路径。
func MapCloudToLocal(localRoot, cloudRoot, cloudPath string) string {
	rel := RelToRoot(CleanCloudPath(cloudRoot), cloudPath, '/')
	if rel == "" {
		return localRoot
	}
	return filepath.Join(localRoot, rel)
}

// CleanCloudPath 规范化云端路径：去除尾斜杠（保留根 "/"）。
func CleanCloudPath(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	return p
}

// ──── 规则 ────

// Rules 文件分类规则（视频扩展名白名单 + 上传排除名单），构造后不可变。
type Rules struct {
	videoExts     map[string]struct{}
	uploadExclude []string // 已小写化的通配模式
}

// NewRules 从全局设置组装规则（名单统一小写，逐文件判定 O(1)）。
func NewRules(videoExts, uploadExclude []string) Rules {
	r := Rules{videoExts: make(map[string]struct{}, len(videoExts))}
	for _, e := range videoExts {
		r.videoExts[strings.ToLower(e)] = struct{}{}
	}
	for _, p := range uploadExclude {
		r.uploadExclude = append(r.uploadExclude, strings.ToLower(p))
	}
	return r
}

// IsVideoExt 按扩展名判断是否为视频（不检查体积）。
func (r Rules) IsVideoExt(path string) bool {
	_, ok := r.videoExts[strings.ToLower(filepath.Ext(path))]
	return ok
}

// IsVideo 判断是否为「值得按视频处理」的文件：命中扩展名且 ≥10MB。
func (r Rules) IsVideo(path string, size int64) bool {
	return r.IsVideoExt(path) && size >= videoThreshold
}

// Excluded 判断文件名是否命中上传排除名单（大小写不敏感，支持通配）。
func (r Rules) Excluded(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range r.uploadExclude {
		if match, err := filepath.Match(p, lower); err == nil && match {
			return true
		}
	}
	return false
}

// ──── 外部能力（由组合根注入，便于单测替换）────

// CacheMover 透传缓存写入接口：上传完成的视频移入缓存供 /download 直读。
// nil 表示缓存未启用（上传后退化为删除原件）。
type CacheMover interface {
	Move(src, pickCode string) (string, error)
}

// Progress 进度上报（由 engine 实现，驱动 SSE 广播）。
type Progress interface {
	Reset(total int64)
	Advance()
	SetCurrent(path string)
}

// NopProgress 空实现，便于单测与预演。
type NopProgress struct{}

func (NopProgress) Reset(int64)       {}
func (NopProgress) Advance()          {}
func (NopProgress) SetCurrent(string) {}

// ──── 云端树快照 ────

// CloudItem 云端目录树里的一个条目。
type CloudItem struct {
	Path     string // 云端绝对路径
	Fid      string
	PickCode string
	Size     int64
	IsDir    bool
	IsVideo  bool
}

// CloudTree 云端目录树的一次快照（扁平化，路径升序）。
// 由 ScanCloud 拉取后交给 PlanCloud 判定，使 PlanCloud 保持纯函数。
type CloudTree struct {
	RootPath string
	RootFid  string
	Items    []CloudItem
}

// TopDirs 返回同步根的直接子目录（用于「归档到回收目录」）。
// 只收目录不收散落文件：归档是「整部片子落地完了就把云端那一份挪走」的语义，
// 与 v2 的行为保持一致。
func (t CloudTree) TopDirs() []CloudItem {
	var out []CloudItem
	for _, it := range t.Items {
		if it.IsDir && depth(it.Path) == depth(t.RootPath)+1 {
			out = append(out, it)
		}
	}
	return out
}

// depth 计算云端路径的层级数（根 "/" 为 0）。
func depth(cloudPath string) int {
	p := strings.Trim(CleanCloudPath(cloudPath), "/")
	if p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}

// FileItems 返回全部文件项（判定只对文件做「缺不缺」的判断，目录的落地由 apply 的 MkdirAll 承担）。
func (t CloudTree) FileItems() []CloudItem {
	var out []CloudItem
	for _, it := range t.Items {
		if !it.IsDir {
			out = append(out, it)
		}
	}
	return out
}

// ──── 本地现状 ────

// LocalState 本地某一项的现状（判定只关心这三项，不关心具体 os.FileInfo）。
type LocalState struct {
	Exists bool
	IsDir  bool
	Size   int64
}

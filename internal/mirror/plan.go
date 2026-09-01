package mirror

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ytx-zhang/115tools/internal/store"
)

// PlanLocal 比对本地目录树与索引，产出需要执行的动作列表。
//
// root 是本次扫描的起点（可以是任务本地根，也可以是监听投进来的子目录）；
// paths.LocalDir 始终是**任务本地根**，用于云端路径映射，绝不在调用链里被改写。
//
// 只读：不写索引、不调云端 API、不改文件系统。因此可以直接用于「预演（dry-run）」，
// 也可以脱离 115 账号做单测（见 plan_test.go）。
//
// 判定规则集中在本文件的 decide 里，这是全项目唯一回答「该不该动作、动什么」的地方。
func PlanLocal(ctx context.Context, root string, paths Paths, idx store.Index, rules Rules, cfg LocalCfg) ([]Op, error) {
	p := &planner{ctx: ctx, idx: idx, rules: rules, cfg: cfg, paths: paths}
	return p.planDir(root), p.err
}

// PlanFile 对单个本地文件做判定（文件事件监听直传用），复用与全量扫描完全相同的收敛点。
//
// 注意：这里取的索引记录挂在 path 本身；视频在开启 STRM 时索引键是 .strm 路径，
// 由 decideVideo 内部再去查，因此两条路径的判定结果一致。
func PlanFile(ctx context.Context, paths Paths, path string, idx store.Index, rules Rules, cfg LocalCfg) ([]Op, error) {
	p := &planner{ctx: ctx, idx: idx, rules: rules, cfg: cfg, paths: paths}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			rec, _ := idx.Get(ctx, path)
			return p.decide(path, rec, LocalState{}), nil
		}
		return nil, err
	}
	rec, _ := idx.Get(ctx, path)
	return p.decide(path, rec, StateFromInfo(info)), nil
}

// PlanCloud 比对云端树快照、本地文件系统与索引，产出落地 / 清理 / 归档动作。
//
// 同样只读。云端树由 ScanCloud 预先拉成快照传进来，判定过程不发网络请求。
//
// 「本地不存在」的处理遵循产品的「以本地文件为准」规则：
// 索引里有记录而本地文件没了 = 本地已删除，交给上传作用域去清云端，这里不重新下载。
func PlanCloud(ctx context.Context, tree CloudTree, idx store.Index, rules Rules, paths Paths, cfg CloudCfg) ([]Op, error) {
	var ops []Op

	for _, it := range tree.FileItems() {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		localPath := MapCloudToLocal(paths.LocalDir, paths.CloudDir, it.Path)
		savePath, genStrm := localPath, false
		if (it.IsVideo || rules.IsVideoExt(it.Path)) && cfg.ToStrm {
			savePath, genStrm = VideoToStrmPath(localPath), true
		}

		rec, has := idx.Get(ctx, savePath)
		if _, err := os.Stat(savePath); err == nil {
			// 本地已有：只在需要清冗余且云端 FID 与索引记录不一致时动作
			if cfg.DropStale && has && rec.Fid != it.Fid {
				ops = append(ops, Op{Kind: OpDrop, Label: OpDrop.Label(), Path: savePath,
					Fid: it.Fid, Reason: "云端同名冗余副本"})
			}
			continue
		}
		if has {
			// 同步过、本地已删除 —— 尊重本地的删除，不重新下载
			continue
		}
		op := Op{Kind: OpDownload, Label: OpDownload.Label(), Path: savePath,
			Fid: it.Fid, PickCode: it.PickCode, Size: it.Size, IsVideo: genStrm,
			Reason: "云端新增，本地缺失"}
		ops = append(ops, op)
	}

	if cfg.Archive {
		for _, d := range tree.TopDirs() {
			ops = append(ops, Op{Kind: OpArchive, Label: OpArchive.Label(),
				Path: d.Path, Fid: d.Fid, Reason: "同步完成，归档到回收目录"})
		}
	}
	return ops, nil
}

// ──── 判定内部实现 ────

// planner 持有一次本地扫描的上下文（避免把五个参数在递归里来回传）。
type planner struct {
	ctx   context.Context
	idx   store.Index
	rules Rules
	cfg   LocalCfg
	paths Paths
	err   error // 首个致命错误（读取目录失败），扫描中止后由 PlanLocal 返回
}

// planDir 扫描单个目录：本地现状与索引子项取并集，逐项交给 decide 判定，目录递归下钻。
func (p *planner) planDir(dir string) []Op {
	if p.err != nil || context.Cause(p.ctx) != nil {
		return nil
	}
	entries, err := readLocalDir(dir, p.rules, p.cfg.ExcludeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 扫描期间目录被删：下一轮自然会走「本地已不存在」分支
		}
		p.err = err
		return nil
	}

	children := p.idx.Children(p.ctx, dir)

	// 索引有记录的：按索引顺序遍历，输出稳定可复现
	var ops []Op
	for _, ch := range children {
		full := filepath.Join(dir, ch.Name)
		e, exists := entries[ch.Name]
		if !exists && p.coveredByLocal(ch.Name, entries) {
			continue // 该索引键归另一个本地文件管（视频 → STRM 的键换算），交给它统一判定
		}
		delete(entries, ch.Name)
		st, ok := stateOf(e, exists)
		if !ok {
			slog.WarnContext(p.ctx, "读取文件信息失败，本轮跳过（绝不按本地已删除处理，防止误清云端）", "路径", full)
			continue
		}
		ops = append(ops, p.decide(full, ch.Rec, st)...)
	}

	// 剩下的都是索引无记录的本地新增；排序保证输出确定（map 遍历顺序随机）
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		full := filepath.Join(dir, name)
		st, ok := stateOf(entries[name], true)
		if !ok {
			continue
		}
		ops = append(ops, p.decide(full, store.Record{}, st)...)
	}
	return ops
}

// decide 是「该不该动作、动什么」的唯一收敛点。
//
// rec 为零值 Record 表示索引无记录（本地新增）；st.Exists 为 false 表示本地已不存在。
func (p *planner) decide(path string, rec store.Record, st LocalState) []Op {
	switch {
	case !st.Exists:
		return p.decideMissing(path, rec)
	case st.IsDir:
		return p.decideDir(path, rec)
	default:
		return p.decideFile(path, rec, st)
	}
}

// decideMissing 本地已不存在：清理云端残留（索引无记录则无事可做）。
func (p *planner) decideMissing(path string, rec store.Record) []Op {
	if rec.Fid == "" {
		return nil
	}
	return []Op{clearOp(path, rec, "本地已不存在，清理云端")}
}

// decideDir 本地是目录：确保云端也是目录，然后下钻。
func (p *planner) decideDir(path string, rec store.Record) []Op {
	var ops []Op
	switch {
	case rec.Fid == "":
		ops = append(ops, newOp(OpMkdir, path, "新增目录"))
	case rec.Kind != store.KindDir:
		// 云端是同名文件，以本地为准：先清再建
		ops = append(ops, clearOp(path, rec, "本地为目录、云端为同名文件，以本地为准"))
		ops = append(ops, newOp(OpMkdir, path, "目录重建"))
	}
	return append(ops, p.planDir(path)...)
}

// decideFile 本地是文件：按「视频待上传 / .strm 指针 / 普通文件」三条路径分别判定。
func (p *planner) decideFile(path string, rec store.Record, st LocalState) []Op {
	switch {
	case IsStrmPath(path):
		return p.decideStrm(path, rec)
	case p.cfg.ToStrm && p.rules.IsVideoExt(path):
		return p.decideVideo(path, rec, st)
	default:
		return p.decidePlain(path, rec, st)
	}
}

// decideStrm 本地是 .strm 指针文件：比对 pickcode（**不再比对 mtime**）。
//
// pickcode 一致 = 指向的云端视频没变，直接跳过；链接格式不对则顺手修一下。
func (p *planner) decideStrm(path string, rec store.Record) []Op {
	content := ReadStrmFile(path)
	pc := ParsePickCode(content)

	// 类型不符（云端是目录或实体文件）：以本地为准，清掉后按新增接管
	if rec.Fid != "" && rec.Kind != store.KindStrm {
		ops := []Op{clearOp(path, rec, "本地为 STRM、云端类型不符，以本地为准")}
		return append(ops, p.adoptOps(path, pc)...)
	}

	if rec.Fid == "" {
		return p.adoptOps(path, pc) // 本地新增的 .strm
	}
	if pc == "" {
		// 索引有记录但本地 strm 读不出 pickcode：内容坏了，归档云端视频避免留下孤儿
		return []Op{{Kind: OpRetire, Label: OpRetire.Label(), Path: path, Fid: rec.Fid,
			Reason: "STRM 内容失效（无 pickcode）"}}
	}
	if pc != rec.PickCode {
		// 指向了另一个云端视频：旧的归档，新的接管
		return []Op{
			{Kind: OpRetire, Label: OpRetire.Label(), Path: path, Fid: rec.Fid, Reason: "STRM 指向已变更"},
			{Kind: OpAdopt, Label: OpAdopt.Label(), Path: path, PickCode: pc, Reason: "接管新的云端视频"},
		}
	}
	if StrmNeedsFix(p.paths.StrmURL, pc, content) {
		return []Op{newOp(OpNormalize, path, "STRM 直链格式需修正")}
	}
	return nil // 一致，跳过
}

// decideVideo 本地是待上传的视频（命中扩展名且开启 STRM 生成）。
//
// 索引里若已存在同名 .strm 记录，说明这是一次同名覆盖：先上传，拿到新 FID 后
// 由 apply 判断新旧是否真的不同（秒传会复用同一 FID），不同才归档旧的。
func (p *planner) decideVideo(path string, rec store.Record, st LocalState) []Op {
	key := VideoToStrmPath(path)

	// 索引记录挂在 .strm 键上，所以这里重新取一次，而不是用传进来的 rec
	strmRec, has := p.idx.Get(p.ctx, key)
	var ops []Op
	if has && strmRec.Kind != store.KindStrm {
		ops = append(ops, clearOp(key, strmRec, "同名 STRM 记录类型不符，以本地为准"))
		strmRec, has = store.Record{}, false
	}

	if !p.rules.IsVideo(path, st.Size) {
		// 未达体积阈值：已有同名 STRM 就跳过（避免半成品文件被当正片重传）
		if has {
			return ops
		}
		return append(ops, p.decidePlain(path, rec, st)...)
	}

	op := Op{Kind: OpUpload, Label: OpUpload.Label(), Path: path, Size: st.Size, IsVideo: true}
	if has && strmRec.Fid != "" {
		op.ReplaceFid = strmRec.Fid
		op.Reason = "同名视频已存在，覆盖重传"
	} else {
		op.Reason = "新增视频"
	}
	return append(ops, op)
}

// decidePlain 本地是普通文件（含关闭 STRM 时的视频）：比对字节数。
func (p *planner) decidePlain(path string, rec store.Record, st LocalState) []Op {
	if rec.Fid != "" && rec.Kind != store.KindFile {
		ops := []Op{clearOp(path, rec, "本地为文件、云端类型不符，以本地为准")}
		return append(ops, Op{Kind: OpUpload, Label: OpUpload.Label(), Path: path,
			Size: st.Size, Reason: "重新上传"})
	}
	if rec.Fid == "" {
		return []Op{{Kind: OpUpload, Label: OpUpload.Label(), Path: path, Size: st.Size, Reason: "新增文件"}}
	}
	if rec.Size == st.Size {
		return nil // 未变
	}
	// 115 允许同名并存，内容变了必须先删旧的再传
	return []Op{
		{Kind: OpDrop, Label: OpDrop.Label(), Path: path, Fid: rec.Fid, Reason: "文件内容已变，删除云端旧副本"},
		{Kind: OpUpload, Label: OpUpload.Label(), Path: path, Size: st.Size, Reason: "重新上传"},
	}
}

// coveredByLocal 判断索引里的 STRM 键是否被某个本地视频文件「认领」。
//
// 开启 STRM 生成后，视频 a.mkv 的索引键是 a.strm（上传后本地留下的就是 .strm）。
// 于是索引里出现 a.strm 而本地只有 a.mkv 时，这不是「本地把 STRM 删了」，
// 而是「这个视频还没传 / 要覆盖重传」——必须交给那个视频文件的判定统一处理，
// 否则会把云端旧视频当成「本地已删除」误归档。
func (p *planner) coveredByLocal(idxName string, entries map[string]os.DirEntry) bool {
	if !p.cfg.ToStrm || !IsStrmPath(idxName) {
		return false
	}
	stem := strings.TrimSuffix(idxName, filepath.Ext(idxName))
	for name, e := range entries {
		if e.IsDir() || !p.rules.IsVideoExt(name) {
			continue
		}
		if strings.TrimSuffix(name, filepath.Ext(name)) == stem {
			return true
		}
	}
	return false
}

// adoptOps 生成本地新增 .strm 的接管动作；读不出 pickcode 时无事可做。
func (p *planner) adoptOps(path, pickCode string) []Op {
	if pickCode == "" {
		return nil
	}
	return []Op{{Kind: OpAdopt, Label: OpAdopt.Label(), Path: path, PickCode: pickCode, Reason: "接管云端视频"}}
}

// ──── 辅助 ────

// clearOp 按云端记录的种类选择清理方式：视频移入回收目录（可找回），其余直接删除。
func clearOp(path string, rec store.Record, reason string) Op {
	if rec.Kind == store.KindStrm {
		return Op{Kind: OpRetire, Label: OpRetire.Label(), Path: path, Fid: rec.Fid, Reason: reason}
	}
	return Op{Kind: OpDrop, Label: OpDrop.Label(), Path: path, Fid: rec.Fid, Reason: reason}
}

// stateOf 从目录项取本地现状。ok=false 表示无法可靠判定（stat 失败），调用方必须跳过该项——
// **绝不**允许把「读不到信息」当成「本地已删除」，否则断链的文件会把云端副本误清掉。
func stateOf(e os.DirEntry, exists bool) (LocalState, bool) {
	if !exists {
		return LocalState{}, true
	}
	if e.IsDir() {
		return LocalState{Exists: true, IsDir: true}, true
	}
	info, err := e.Info()
	if err != nil {
		return LocalState{}, false
	}
	return LocalState{Exists: true, Size: info.Size()}, true
}

// StateFromInfo 把 os.FileInfo 转成 LocalState（供监听直传这类只有 stat 结果的场景复用判定）。
func StateFromInfo(info os.FileInfo) LocalState {
	if info.IsDir() {
		return LocalState{Exists: true, IsDir: true}
	}
	return LocalState{Exists: true, Size: info.Size()}
}

// readLocalDir 读取目录到 map（跳过上传排除项与透传缓存目录）。
func readLocalDir(path string, rules Rules, excludeDir string) (map[string]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		if excludeDir != "" && filepath.Join(path, e.Name()) == excludeDir {
			continue
		}
		if rules.Excluded(e.Name()) {
			continue
		}
		m[e.Name()] = e
	}
	return m, nil
}

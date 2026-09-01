package mirror

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ytx-zhang/115tools/internal/store"
)

// ──── fake 索引：让判定层可以脱离 115 账号做单测 ────

type fakeIndex map[string]store.Record

func (f fakeIndex) Get(_ context.Context, path string) (store.Record, bool) {
	r, ok := f[path]
	return r, ok
}

func (f fakeIndex) Put(_ context.Context, path string, r store.Record) { f[path] = r }

func (f fakeIndex) Children(_ context.Context, dir string) []store.Child {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []store.Child
	for k, v := range f {
		rel, ok := strings.CutPrefix(k, prefix)
		if !ok || rel == "" || strings.Contains(rel, "/") { // 只收直属子项
			continue
		}
		out = append(out, store.Child{Name: rel, Rec: v})
	}
	slices.SortFunc(out, func(a, b store.Child) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (f fakeIndex) CountRecursive(_ context.Context, path string) int64 {
	prefix := strings.TrimSuffix(path, "/") + "/"
	var n int64
	for k := range f {
		if _, ok := strings.CutPrefix(k, prefix); ok {
			n++
		}
	}
	return n
}

func (f fakeIndex) ListStrmFids(_ context.Context, dirPath string) []string {
	prefix := strings.TrimSuffix(dirPath, "/") + "/"
	var out []string
	for k, v := range f {
		if _, ok := strings.CutPrefix(k, prefix); ok && IsStrmPath(k) && v.Fid != "" {
			out = append(out, v.Fid)
		}
	}
	return out
}

func (f fakeIndex) ClearTree(_ context.Context, paths ...string) {
	for _, p := range paths {
		for k := range f {
			if k == p || strings.HasPrefix(k, strings.TrimSuffix(p, "/")+"/") {
				delete(f, k)
			}
		}
	}
}

// ──── 脚手架 ────

const testStrmURL = "http://192.168.1.9:8080"

type fixture struct {
	t    *testing.T
	root string
	idx  fakeIndex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, root: t.TempDir(), idx: fakeIndex{}}
}

func (f *fixture) touch(rel, content string) string {
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) mkdir(rel string) string {
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// big 建一个 ≥10MB 的视频文件（Truncate 产生稀疏文件，不实际占盘）。
func (f *fixture) big(rel string, size int64) string {
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	fh, err := os.Create(p)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	if err := fh.Truncate(size); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) plan(t *testing.T, cfg LocalCfg) []Op {
	t.Helper()
	paths := Paths{LocalDir: f.root, CloudDir: "/media", StrmURL: testStrmURL}
	rules := NewRules([]string{".mp4", ".mkv"}, nil)
	ops, err := PlanLocal(context.Background(), f.root, paths, f.idx, rules, cfg)
	if err != nil {
		t.Fatalf("PlanLocal 返回错误: %v", err)
	}
	return ops
}

func wantKinds(t *testing.T, ops []Op, want ...OpKind) {
	t.Helper()
	got := make([]OpKind, 0, len(ops))
	for _, op := range ops {
		got = append(got, op.Kind)
	}
	if slices.Equal(got, want) {
		return
	}
	t.Fatalf("动作序列不符\ngot =%v\nwant=%v", got, want)
}

// ──── 判定表：本地不存在 ────

func TestDecideMissingFile(t *testing.T) {
	f := newFixture(t)
	f.idx[f.root+"/gone.mkv"] = store.Record{Fid: "f1", Kind: store.KindFile, Size: 100}

	ops := f.plan(t, LocalCfg{})
	wantKinds(t, ops, OpDrop)
	if ops[0].Fid != "f1" {
		t.Errorf("应带上云端 FID: %+v", ops[0])
	}
}

func TestDecideMissingStrmMovesToTemp(t *testing.T) {
	f := newFixture(t)
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "v1", PickCode: "pc1", Kind: store.KindStrm}

	// 视频可找回 → 移入回收目录，而不是直接删除
	wantKinds(t, f.plan(t, LocalCfg{}), OpRetire)
}

func TestDecideMissingDir(t *testing.T) {
	f := newFixture(t)
	f.idx[f.root+"/old"] = store.Record{Fid: "d1", Kind: store.KindDir}
	wantKinds(t, f.plan(t, LocalCfg{}), OpDrop)
}

func TestDecideMissingWithoutRecordIsNoop(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("keep.txt", "x")
	wantKinds(t, f.plan(t, LocalCfg{}), OpUpload)
}

// ──── 判定表：目录 ────

func TestDecideNewDir(t *testing.T) {
	f := newFixture(t)
	_ = f.mkdir("A")
	_ = f.touch("A/x.mkv", "hello")

	ops := f.plan(t, LocalCfg{})
	wantKinds(t, ops, OpMkdir, OpUpload) // 先建目录再传文件
	if ops[0].Path != f.root+"/A" {
		t.Errorf("应建目录 A: %s", ops[0].Path)
	}
}

func TestDecideExistingDirDescends(t *testing.T) {
	f := newFixture(t)
	_ = f.mkdir("A")
	f.idx[f.root+"/A"] = store.Record{Fid: "d1", Kind: store.KindDir}
	_ = f.touch("A/x.mkv", "hello")

	wantKinds(t, f.plan(t, LocalCfg{}), OpUpload) // 目录已在云端，不重建
}

func TestDecideDirReplacesCloudFile(t *testing.T) {
	f := newFixture(t)
	_ = f.mkdir("A")
	f.idx[f.root+"/A"] = store.Record{Fid: "f1", Kind: store.KindFile, Size: 10}

	// 以本地为准：先删云端同名文件，再建目录
	wantKinds(t, f.plan(t, LocalCfg{}), OpDrop, OpMkdir)
}

// ──── 判定表：普通文件 ────

func TestDecidePlainNewFile(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.txt", "hello")

	ops := f.plan(t, LocalCfg{})
	wantKinds(t, ops, OpUpload)
	if ops[0].Size != 5 {
		t.Errorf("应带上文件大小: %d", ops[0].Size)
	}
}

func TestDecidePlainUnchangedIsNoop(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.txt", "hello")
	f.idx[f.root+"/a.txt"] = store.Record{Fid: "f1", Kind: store.KindFile, Size: 5}

	wantKinds(t, f.plan(t, LocalCfg{}))
}

func TestDecidePlainSizeChanged(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.txt", "hello world")
	f.idx[f.root+"/a.txt"] = store.Record{Fid: "f1", Kind: store.KindFile, Size: 5}

	// 115 允许同名并存，内容变了必须先删旧副本再传
	wantKinds(t, f.plan(t, LocalCfg{}), OpDrop, OpUpload)
}

func TestDecidePlainReplacesCloudDir(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.txt", "hello")
	f.idx[f.root+"/a.txt"] = store.Record{Fid: "d1", Kind: store.KindDir}

	wantKinds(t, f.plan(t, LocalCfg{}), OpDrop, OpUpload)
}

// ──── 判定表：视频 ────

func TestDecideVideoUploadsAndGeneratesStrm(t *testing.T) {
	f := newFixture(t)
	_ = f.big("a.mkv", 20<<20)

	ops := f.plan(t, LocalCfg{ToStrm: true})
	wantKinds(t, ops, OpUpload)
	if !ops[0].IsVideo {
		t.Error("应标记为视频")
	}
	if ops[0].ReplaceFid != "" {
		t.Errorf("首次上传不该带 ReplaceFid: %+v", ops[0])
	}
}

func TestDecideVideoSameNameOverwrites(t *testing.T) {
	f := newFixture(t)
	_ = f.big("a.mkv", 20<<20)
	// 索引已有同名 .strm → 同名覆盖，旧 FID 交给 apply 在拿到新 FID 后再判断是否归档
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "v-old", PickCode: "pc-old", Kind: store.KindStrm}

	ops := f.plan(t, LocalCfg{ToStrm: true})
	wantKinds(t, ops, OpUpload)
	if ops[0].ReplaceFid != "v-old" {
		t.Errorf("应带上待归档的旧 FID: %+v", ops[0])
	}
}

func TestDecideVideoBelowThresholdSkipsWhenStrmExists(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.mkv", "tiny")
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "v1", PickCode: "pc1", Kind: store.KindStrm}

	wantKinds(t, f.plan(t, LocalCfg{ToStrm: true})) // 半成品文件不该顶掉已有的 STRM
}

func TestDecideVideoBelowThresholdUploadsAsPlainWhenNoStrm(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.mkv", "tiny")

	ops := f.plan(t, LocalCfg{ToStrm: true})
	wantKinds(t, ops, OpUpload)
	if ops[0].IsVideo {
		t.Error("未达阈值不该按视频处理（不生成 STRM）")
	}
}

func TestDecideVideoWithoutToStrmKeepsOriginal(t *testing.T) {
	f := newFixture(t)
	_ = f.big("a.mkv", 20<<20)

	ops := f.plan(t, LocalCfg{ToStrm: false})
	wantKinds(t, ops, OpUpload)
	if ops[0].IsVideo {
		t.Error("关闭 STRM 时不该标记 IsVideo（保留原件，按普通文件记索引）")
	}
}

// ──── 判定表：.strm ────

func TestDecideStrmMatchingPickCodeIsNoop(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.strm", StrmContent(testStrmURL, "pc1"))
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "v1", PickCode: "pc1", Kind: store.KindStrm}

	wantKinds(t, f.plan(t, LocalCfg{}))
}

// 本次整改的核心回归点：mtime 变化**不应**触发任何动作。
// v2 把 mtime 当版本号用，改写 .strm 后必须把 mtime 改回去，否则会误判变更、删旧视频重传。
// 判定改比 pickcode 之后，这类问题从根上消失。
func TestDecideStrmMtimeChangeIsIgnored(t *testing.T) {
	f := newFixture(t)
	p := f.touch("a.strm", StrmContent(testStrmURL, "pc1"))
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "v1", PickCode: "pc1", Kind: store.KindStrm}

	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(p, later, later); err != nil { // 模拟外部工具重写了文件
		t.Fatal(err)
	}
	wantKinds(t, f.plan(t, LocalCfg{}))
}

func TestDecideStrmPickCodeChanged(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.strm", StrmContent(testStrmURL, "pc2"))
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "v1", PickCode: "pc1", Kind: store.KindStrm}

	ops := f.plan(t, LocalCfg{})
	wantKinds(t, ops, OpRetire, OpAdopt) // 旧的归档，新的接管
	if ops[1].PickCode != "pc2" {
		t.Errorf("接管动作应带新 pickcode: %+v", ops[1])
	}
}

func TestDecideStrmNewFileIsAdopted(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.strm", StrmContent(testStrmURL, "pc-new"))

	ops := f.plan(t, LocalCfg{})
	wantKinds(t, ops, OpAdopt)
	if ops[0].PickCode != "pc-new" {
		t.Errorf("应带上 pickcode: %+v", ops[0])
	}
}

func TestDecideStrmWithoutPickCodeIsIgnored(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.strm", "not a url at all")

	wantKinds(t, f.plan(t, LocalCfg{})) // 无法接管，无事可做
}

func TestDecideStrmBrokenContentRetiresCloudVideo(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.strm", "garbage")
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "v1", PickCode: "pc1", Kind: store.KindStrm}

	wantKinds(t, f.plan(t, LocalCfg{}), OpRetire)
}

func TestDecideStrmNormalizesStaleLink(t *testing.T) {
	f := newFixture(t)
	// 指向的 pickcode 没变，但 host 是旧地址（用户改了 strm_url）
	_ = f.touch("a.strm", StrmContent("http://old-host:1234", "pc1"))
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "v1", PickCode: "pc1", Kind: store.KindStrm}

	wantKinds(t, f.plan(t, LocalCfg{}), OpNormalize)
}

func TestDecideStrmReplacesCloudFile(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.strm", StrmContent(testStrmURL, "pc1"))
	f.idx[f.root+"/a.strm"] = store.Record{Fid: "f1", Kind: store.KindFile, Size: 10}

	wantKinds(t, f.plan(t, LocalCfg{}), OpDrop, OpAdopt)
}

// 监听把子目录投进来时，root 是子目录而 LocalDir 仍是任务根：
// 判定只应看到子目录下的内容，且云端路径映射不会因 LocalDir 被改而错位。
func TestPlanLocalScansSubdirAsRoot(t *testing.T) {
	f := newFixture(t)
	sub := f.mkdir("A/B")
	_ = f.touch("A/B/x.mkv", "hello")
	_ = f.touch("other.mkv", "world") // 任务根下的其他文件，不该出现在子目录扫描里

	paths := Paths{LocalDir: f.root, CloudDir: "/media", StrmURL: testStrmURL}
	rules := NewRules([]string{".mkv"}, nil)
	ops, err := PlanLocal(context.Background(), sub, paths, f.idx, rules, LocalCfg{})
	if err != nil {
		t.Fatal(err)
	}
	// root 即 A/B 自身：不会为自己的目录发 OpMkdir（上传时 apply 会确保云端目录存在）
	wantKinds(t, ops, OpUpload)
	if ops[0].Path != filepath.Join(sub, "x.mkv") {
		t.Errorf("子目录扫描应只处理 root 之下: %s", ops[0].Path)
	}
}

// ──── 判定表：云端 → 本地 ────

func planCloud(t *testing.T, root string, idx fakeIndex, tree CloudTree, cfg CloudCfg) []Op {
	t.Helper()
	paths := Paths{LocalDir: root, CloudDir: "/media", StrmURL: testStrmURL}
	rules := NewRules([]string{".mp4", ".mkv"}, nil)
	ops, err := PlanCloud(context.Background(), tree, idx, rules, paths, cfg)
	if err != nil {
		t.Fatalf("PlanCloud 返回错误: %v", err)
	}
	return ops
}

func TestPlanCloudDownloadsMissing(t *testing.T) {
	root := t.TempDir()
	tree := CloudTree{RootPath: "/media", RootFid: "root", Items: []CloudItem{
		{Path: "/media/a.mkv", Fid: "v1", PickCode: "pc1", Size: 100, IsVideo: true},
		{Path: "/media/b.txt", Fid: "f1", PickCode: "pc2", Size: 10},
	}}

	ops := planCloud(t, root, fakeIndex{}, tree, CloudCfg{ToStrm: true})
	wantKinds(t, ops, OpDownload, OpDownload)
	if ops[0].Path != filepath.Join(root, "a.strm") {
		t.Errorf("视频应落地为 .strm: %s", ops[0].Path)
	}
	if !ops[0].IsVideo {
		t.Error("视频动作应标记 IsVideo")
	}
	if ops[1].Path != filepath.Join(root, "b.txt") {
		t.Errorf("普通文件应下载实体: %s", ops[1].Path)
	}
}

func TestPlanCloudSkipsExistingLocalFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.strm"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := CloudTree{RootPath: "/media", RootFid: "root", Items: []CloudItem{
		{Path: "/media/a.mkv", Fid: "v1", PickCode: "pc1", IsVideo: true},
	}}

	wantKinds(t, planCloud(t, root, fakeIndex{}, tree, CloudCfg{ToStrm: true}))
}

// 「以本地文件为准」：同步过但本地已删除 → 不重新下载（交给上传作用域去清云端）。
func TestPlanCloudRespectsLocalDeletion(t *testing.T) {
	root := t.TempDir()
	idx := fakeIndex{filepath.Join(root, "a.strm"): {Fid: "v1", PickCode: "pc1", Kind: store.KindStrm}}
	tree := CloudTree{RootPath: "/media", RootFid: "root", Items: []CloudItem{
		{Path: "/media/a.mkv", Fid: "v1", PickCode: "pc1", IsVideo: true},
	}}

	wantKinds(t, planCloud(t, root, idx, tree, CloudCfg{ToStrm: true}))
}

func TestPlanCloudDropsStale(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.strm")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 索引记的是另一个 FID → 云端这一份是冗余副本
	idx := fakeIndex{p: {Fid: "v-other", PickCode: "pc-other", Kind: store.KindStrm}}
	tree := CloudTree{RootPath: "/media", RootFid: "root", Items: []CloudItem{
		{Path: "/media/a.mkv", Fid: "v1", PickCode: "pc1", IsVideo: true},
	}}

	ops := planCloud(t, root, idx, tree, CloudCfg{ToStrm: true, DropStale: true})
	wantKinds(t, ops, OpDrop)
	if ops[0].Fid != "v1" {
		t.Errorf("应删除云端那份冗余副本: %+v", ops[0])
	}
}

func TestPlanCloudArchiveTopDirs(t *testing.T) {
	root := t.TempDir()
	tree := CloudTree{RootPath: "/media", RootFid: "root", Items: []CloudItem{
		{Path: "/media/Movie", Fid: "d1", IsDir: true},
		{Path: "/media/Movie/x.mkv", Fid: "v1", PickCode: "pc1", IsVideo: true},
		{Path: "/media/loose.mkv", Fid: "v2", PickCode: "pc2", IsVideo: true}, // 顶层散落文件不归档
	}}

	ops := planCloud(t, root, fakeIndex{}, tree, CloudCfg{Archive: true})
	wantKinds(t, ops, OpDownload, OpDownload, OpArchive)
	if ops[2].Fid != "d1" {
		t.Errorf("应归档顶层目录: %+v", ops[2])
	}
}

// ──── 排除规则 ────

func TestUploadExcludeIsRespected(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.mkv", "x")
	_ = f.touch("a.part", "x") // 下载中的临时文件

	paths := Paths{LocalDir: f.root, CloudDir: "/media", StrmURL: testStrmURL}
	rules := NewRules([]string{".mkv"}, []string{"*.part", ".DS_Store"})
	ops, err := PlanLocal(context.Background(), f.root, paths, f.idx, rules, LocalCfg{})
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, ops, OpUpload)
	if ops[0].Path != f.root+"/a.mkv" {
		t.Errorf("排除项不该被上传: %s", ops[0].Path)
	}
}

func TestCacheDirIsExcluded(t *testing.T) {
	f := newFixture(t)
	_ = f.touch("a.mkv", "x")
	cacheDir := f.mkdir("cache")
	if err := os.WriteFile(filepath.Join(cacheDir, "c.mkv"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths := Paths{LocalDir: f.root, CloudDir: "/media"}
	rules := NewRules([]string{".mkv"}, nil)
	ops, err := PlanLocal(context.Background(), f.root, paths, f.idx, rules, LocalCfg{ExcludeDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, ops, OpUpload)
	if ops[0].Path != f.root+"/a.mkv" {
		t.Errorf("缓存目录不该被当新增上传: %s", ops[0].Path)
	}
}

// ──── 安全 ────

// Op 序列化不得泄漏云端 FID / pickcode（预演结果会直接回显给前端）。
func TestOpJSONDoesNotLeakCloudIDs(t *testing.T) {
	op := Op{Kind: OpUpload, Label: OpUpload.Label(), Path: "/m/a.mkv",
		Fid: "secret-fid", PickCode: "secret-pc", ReplaceFid: "secret-old", IsVideo: true}
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret-fid", "secret-pc", "secret-old"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("Op JSON 泄漏了云端标识 %q: %s", leak, raw)
		}
	}
}

func TestDangerOpsAreMarked(t *testing.T) {
	for _, k := range []OpKind{OpRetire, OpDrop, OpArchive} {
		if !k.Danger() {
			t.Errorf("%v 应标记为危险动作", k)
		}
	}
	for _, k := range []OpKind{OpUpload, OpDownload, OpMkdir, OpAdopt, OpNormalize} {
		if k.Danger() {
			t.Errorf("%v 不该标记为危险动作", k)
		}
	}
}

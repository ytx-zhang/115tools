package store

import (
	"context"
	"path/filepath"
	"testing"
)

// newTestStore 在临时目录打开一个测试库。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRecordRoundTrip(t *testing.T) {
	cases := []Record{
		{Kind: KindDir, Fid: "d123"},
		{Kind: KindFile, Fid: "f456", Size: 1234567890},
		{Kind: KindStrm, Fid: "v789", PickCode: "abcdefghijklmnop"},
		{Kind: KindStrm, Fid: "", PickCode: ""},
	}
	for _, want := range cases {
		got, ok := decodeRecord(encodeRecord(want))
		if !ok {
			t.Fatalf("解码失败: %+v", want)
		}
		if got != want {
			t.Errorf("往返不一致\nwant=%+v\ngot =%+v", want, got)
		}
	}
}

func TestDecodeRecordRejectsForeignVersion(t *testing.T) {
	if _, ok := decodeRecord([]byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("旧版本记录应视为无记录")
	}
	if _, ok := decodeRecord([]byte{0x02, 0, 0}); ok {
		t.Fatal("截断记录应视为无记录")
	}
}

func TestIndexGetPut(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, ok := s.Get(ctx, "/media/a.mkv"); ok {
		t.Fatal("空库不应有记录")
	}

	want := Record{Kind: KindStrm, Fid: "fid-1", PickCode: "pc-1"}
	s.Put(ctx, "/media/a.strm", want)
	got, ok := s.Get(ctx, "/media/a.strm")
	if !ok {
		t.Fatal("写入后应能读到")
	}
	if got != want {
		t.Fatalf("读到的记录不符: got=%+v want=%+v", got, want)
	}

	// 覆盖写
	want.PickCode = "pc-2"
	s.Put(ctx, "/media/a.strm", want)
	if got, _ = s.Get(ctx, "/media/a.strm"); got.PickCode != "pc-2" {
		t.Fatalf("覆盖写未生效: %+v", got)
	}
}

func TestChildrenSkipsDescendants(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// 建立一个三层树：/media（根）、/media/A（目录）、/media/A/deep（目录）、/media/A/deep/x.mkv
	s.Put(ctx, "/media", Record{Kind: KindDir, Fid: "root"})
	s.Put(ctx, "/media/A", Record{Kind: KindDir, Fid: "a"})
	s.Put(ctx, "/media/A/deep", Record{Kind: KindDir, Fid: "deep"})
	s.Put(ctx, "/media/A/deep/x.mkv", Record{Kind: KindFile, Fid: "x", Size: 10})
	s.Put(ctx, "/media/b.mkv", Record{Kind: KindFile, Fid: "b", Size: 20})
	s.Put(ctx, "/mediaAbc", Record{Kind: KindFile, Fid: "sibling", Size: 1}) // 前缀相近的兄弟，不应被误收

	got := s.Children(ctx, "/media")
	want := map[string]string{"A": "a", "b.mkv": "b"}
	if len(got) != len(want) {
		t.Fatalf("直属子项数量不符: got=%d want=%d (%v)", len(got), len(want), got)
	}
	for _, c := range got {
		if fid, ok := want[c.Name]; !ok || c.Rec.Fid != fid {
			t.Errorf("意外的子项或 FID 不符: %+v", c)
		}
	}

	// 目录本身也需要能从 Children 里被识别为目录（供 plan 下钻）
	var aIsDir bool
	for _, c := range got {
		if c.Name == "A" && c.Rec.Kind == KindDir {
			aIsDir = true
		}
	}
	if !aIsDir {
		t.Error("子目录 A 的 Kind 应为 KindDir")
	}
}

func TestCountRecursive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.Put(ctx, "/media", Record{Kind: KindDir, Fid: "root"})
	s.Put(ctx, "/media/A", Record{Kind: KindDir, Fid: "a"})
	s.Put(ctx, "/media/A/x.mkv", Record{Kind: KindFile, Fid: "x", Size: 1})
	// 口径：只数后代，不含 path 自身（与云端目录 FileCount+FolderCount 对齐）
	if n := s.CountRecursive(ctx, "/media"); n != 2 {
		t.Errorf("递归计数不符: got=%d want=2", n)
	}
	if n := s.CountRecursive(ctx, "/media/A"); n != 1 {
		t.Errorf("子目录递归计数不符: got=%d want=1", n)
	}
	if n := s.CountRecursive(ctx, "/nope"); n != 0 {
		t.Errorf("不存在的路径应计数 0: got=%d", n)
	}
}

func TestListStrmFids(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.Put(ctx, "/media/a.strm", Record{Kind: KindStrm, Fid: "v1"})
	s.Put(ctx, "/media/sub/b.STRM", Record{Kind: KindStrm, Fid: "v2"}) // 大写后缀也要命中
	s.Put(ctx, "/media/c.mkv", Record{Kind: KindFile, Fid: "v3", Size: 1})

	got := s.ListStrmFids(ctx, "/media")
	if len(got) != 2 {
		t.Fatalf("应命中 2 条 strm，实得 %d: %v", len(got), got)
	}
	want := map[string]bool{"v1": true, "v2": true}
	for _, fid := range got {
		if !want[fid] {
			t.Errorf("意外的 FID: %s", fid)
		}
	}
}

func TestClearTree(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.Put(ctx, "/media/A", Record{Kind: KindDir, Fid: "a"})
	s.Put(ctx, "/media/A/x.mkv", Record{Kind: KindFile, Fid: "x", Size: 1})
	s.Put(ctx, "/media/keep.mkv", Record{Kind: KindFile, Fid: "k", Size: 2})

	s.ClearTree(ctx, "/media/A", "/media/keep.mkv")

	if _, ok := s.Get(ctx, "/media/A"); ok {
		t.Error("被清理的目录记录应消失")
	}
	if _, ok := s.Get(ctx, "/media/A/x.mkv"); ok {
		t.Error("被清理目录的后代应一起消失")
	}
	if _, ok := s.Get(ctx, "/media/keep.mkv"); ok {
		t.Error("显式清理的路径应消失")
	}
	if n := s.CountRecursive(ctx, "/media"); n != 0 {
		t.Errorf("清理后应无残留: got=%d", n)
	}
}

func TestActivityAppendAndList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if got := s.List(ctx, "", 0, 10); len(got) != 0 {
		t.Fatalf("空库应无事件: %d", len(got))
	}

	for i := range 3 {
		if _, err := s.Append(ctx, Event{
			TaskID:   "t1",
			TaskName: "任务一",
			Scope:    ScopeUpload,
			Trigger:  TriggerCron,
			State:    StateSuccess,
			Stats:    Stats{Uploaded: int64(i)},
		}); err != nil {
			t.Fatalf("写入事件失败: %v", err)
		}
	}
	if _, err := s.Append(ctx, Event{TaskID: "t2", TaskName: "任务二", Scope: ScopeDownload, Trigger: TriggerManual, State: StateFailed, Error: "boom"}); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}

	all := s.List(ctx, "", 0, 10)
	if len(all) != 4 {
		t.Fatalf("应读到 4 条事件: %d", len(all))
	}
	if all[0].TaskID != "t2" {
		t.Errorf("最新事件应在最前: got=%s", all[0].TaskID)
	}
	if all[0].Seq <= all[1].Seq {
		t.Errorf("seq 应递减（最新在前）: %d vs %d", all[0].Seq, all[1].Seq)
	}

	one := s.List(ctx, "t1", 0, 10)
	if len(one) != 3 {
		t.Fatalf("按任务过滤应得 3 条: %d", len(one))
	}
	for _, e := range one {
		if e.TaskID != "t1" {
			t.Errorf("过滤失效: %s", e.TaskID)
		}
	}

	if got := s.List(ctx, "t1", 0, 2); len(got) != 2 {
		t.Errorf("limit 未生效: %d", len(got))
	}

	if err := s.DeleteTask(ctx, "t1"); err != nil {
		t.Fatalf("删除任务事件失败: %v", err)
	}
	if got := s.List(ctx, "t1", 0, 10); len(got) != 0 {
		t.Errorf("删除后应无残留: %d", len(got))
	}
	if got := s.List(ctx, "", 0, 10); len(got) != 1 {
		t.Errorf("其他任务的事件不应被误删: %d", len(got))
	}
}

func TestStatsEmpty(t *testing.T) {
	if !(Stats{}).Empty() {
		t.Error("零值 Stats 应为 Empty")
	}
	if (Stats{Failed: 1}).Empty() {
		t.Error("非零 Stats 不应为 Empty")
	}
}

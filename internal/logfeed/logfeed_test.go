package logfeed

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestFeedRingEviction(t *testing.T) {
	f := NewFeed(3)
	for i := 0; i < 5; i++ {
		f.Add(Entry{Msg: fmt.Sprintf("m%d", i)})
	}
	if got := f.Seq(); got != 5 {
		t.Fatalf("Seq() = %d, want 5", got)
	}
	ss := f.Snapshot()
	if len(ss) != 3 {
		t.Fatalf("Snapshot() len = %d, want 3", len(ss))
	}
	want := []string{"m4", "m3", "m2"} // 最新在前，m0/m1 被淘汰
	for i, e := range ss {
		if e.Msg != want[i] {
			t.Errorf("Snapshot()[%d] = %q, want %q", i, e.Msg, want[i])
		}
	}
}

func TestFeedSince(t *testing.T) {
	f := NewFeed(10)
	for i := 0; i < 5; i++ {
		f.Add(Entry{Msg: fmt.Sprintf("m%d", i)})
	}
	got := f.Since(2) // 应返回 seq 3、4（最新在前）
	if len(got) != 2 || got[0].Msg != "m4" || got[1].Msg != "m3" {
		t.Fatalf("Since(2) = %+v, want [m4 m3]", got)
	}
	if got := f.Since(4); len(got) != 0 {
		t.Fatalf("Since(4) = %+v, want empty", got)
	}
}

func TestFeedSinceStale(t *testing.T) {
	f := NewFeed(3)
	for i := 0; i < 5; i++ {
		f.Add(Entry{Msg: fmt.Sprintf("m%d", i)})
	}
	got := f.Since(0) // 落后于起点 start=2，返回全部可读 [m4 m3 m2]
	if len(got) != 3 || got[2].Msg != "m2" {
		t.Fatalf("Since(0) = %+v, want [m4 m3 m2]", got)
	}
}

// TestFeedSinceFromZero 锁死「缓冲未回绕（start==0）+ seq==0」场景：这正是线上
// 进程刚启动、SSE 首帧 lastLogSeq=0 时推送第一条 Warn/Error 的路径。
// 旧写法 for s := next-1; s >= lo; s-- 中 s 为 uint64，减到 0 后自减会下溢成
// MaxUint64，"s >= lo" 恒成立 → 无限追加、内存暴涨 OOM 且全程持锁卡死进程。
// （TestFeedSinceStale 用的是已回绕的 feed，start=2，覆盖不到这条路径。）
func TestFeedSinceFromZero(t *testing.T) {
	f := NewFeed(10)
	for i := 0; i < 3; i++ {
		f.Add(Entry{Msg: fmt.Sprintf("m%d", i)})
	}
	got := f.Since(0)
	if len(got) != 3 || got[0].Msg != "m2" || got[2].Msg != "m0" {
		t.Fatalf("Since(0) = %+v, want [m2 m1 m0]", got)
	}
}

func TestFeedClear(t *testing.T) {
	f := NewFeed(5)
	for i := 0; i < 3; i++ {
		f.Add(Entry{Msg: fmt.Sprintf("m%d", i)})
	}
	f.Clear()
	if len(f.Snapshot()) != 0 {
		t.Fatalf("after Clear, Snapshot() = %+v, want empty", f.Snapshot())
	}
	if got := f.Seq(); got != 3 {
		t.Fatalf("after Clear, Seq() = %d, want 3 (seq 不复位)", got)
	}
	f.Add(Entry{Msg: "new"})
	ss := f.Snapshot()
	if len(ss) != 1 || ss[0].Seq != 3 || ss[0].Msg != "new" {
		t.Fatalf("after Clear+Add, Snapshot() = %+v, want [{seq:3 new}]", ss)
	}
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

func TestHandlerCollect(t *testing.T) {
	f := NewFeed(10)
	var buf bytes.Buffer
	h := NewHandler(f, slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}), slog.LevelWarn)
	logger := slog.New(h)
	logger.Info("info-msg")
	logger.Warn("warn-msg", "k", "v")
	logger.Error("err-msg")

	ss := f.Snapshot()
	if len(ss) != 2 {
		t.Fatalf("feed len = %d, want 2 (Info 不收集)", len(ss))
	}
	if ss[0].Level != "ERROR" || ss[0].Msg != "err-msg" {
		t.Errorf("Snapshot()[0] = %+v, want ERROR err-msg", ss[0])
	}
	if ss[1].Level != "WARN" || len(ss[1].Attrs) != 1 || ss[1].Attrs[0].Key != "k" {
		t.Errorf("Snapshot()[1] = %+v, want WARN warn-msg attrs[k=v]", ss[1])
	}
	// stdout 转发完整：Info/Warn/Error 都在
	out := buf.String()
	for _, want := range []string{"info-msg", "warn-msg", "err-msg"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout 缺少 %q, got %q", want, out)
		}
	}
}

func TestHandlerLevelGate(t *testing.T) {
	f := NewFeed(10)
	var buf bytes.Buffer
	h := NewHandler(f, slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}), slog.LevelError)
	logger := slog.New(h)
	logger.Warn("warn-msg")
	logger.Error("err-msg")
	if len(f.Snapshot()) != 1 {
		t.Fatalf("feed len = %d, want 1（LOG_LEVEL=ERROR 时 Warn 不产生）", len(f.Snapshot()))
	}
	if !strings.Contains(buf.String(), "err-msg") {
		t.Errorf("stdout 缺少 err-msg, got %q", buf.String())
	}
}

func TestHandlerWithAttrsChain(t *testing.T) {
	f := NewFeed(10)
	h := NewHandler(f, discardHandler{}, slog.LevelWarn)
	logger := slog.New(h).With("ctx", "x") // WithAttrs 链后收集仍生效
	logger.Error("m")
	if len(f.Snapshot()) != 1 {
		t.Fatalf("feed len = %d, want 1（With 链后收集不丢）", len(f.Snapshot()))
	}
}

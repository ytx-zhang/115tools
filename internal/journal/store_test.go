package journal

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// newTestStore 在临时目录建一个历史库。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// TestRunLogsIsolatedAcrossTasks 不同任务的执行日志必须互相隔离：
// v1 里 seq 由各任务桶自己递增，跨任务重复（A、B 都有 seq=1），
// 明细日志却以 seq 为全局键 → 后结束的 run 覆盖前一个，点开 A 看到 B 的日志。
func TestRunLogsIsolatedAcrossTasks(t *testing.T) {
	s := newTestStore(t)

	seqA, err := s.Begin(Run{TaskID: "taskA", Direction: DirPush, Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Begin A 失败: %v", err)
	}
	seqB, err := s.Begin(Run{TaskID: "taskB", Direction: DirPull, Trigger: TriggerCron})
	if err != nil {
		t.Fatalf("Begin B 失败: %v", err)
	}
	if seqA == seqB {
		t.Fatalf("不同任务拿到相同 seq: %d", seqA)
	}

	s.AppendLog("taskA", seqA, "INFO", "A 的第一条", "")
	s.AppendLog("taskB", seqB, "INFO", "B 的第一条", "")
	// 归属不匹配时必须被丢弃，而不是串进别的任务
	s.AppendLog("taskB", seqA, "INFO", "B 冒用 A 的 seq", "")

	// 运行中（内存缓冲）阶段
	assertLogs(t, s, "taskA", seqA, "A 的第一条")
	assertLogs(t, s, "taskB", seqB, "B 的第一条")
	if logs, err := s.Logs("taskB", seqA); err != nil || len(logs) != 0 {
		t.Fatalf("跨任务查日志应为空，实得 %v (err=%v)", logs, err)
	}

	if err := s.Finish(seqA, StateSuccess, Counters{}, ""); err != nil {
		t.Fatalf("Finish A 失败: %v", err)
	}
	if err := s.Finish(seqB, StateSuccess, Counters{}, ""); err != nil {
		t.Fatalf("Finish B 失败: %v", err)
	}

	// 落盘后（journal.db）阶段
	assertLogs(t, s, "taskA", seqA, "A 的第一条")
	assertLogs(t, s, "taskB", seqB, "B 的第一条")
	if logs, err := s.Logs("taskA", seqB); err != nil || len(logs) != 0 {
		t.Fatalf("跨任务查日志应为空，实得 %v (err=%v)", logs, err)
	}
}

func assertLogs(t *testing.T, s *Store, taskID string, seq uint64, wantMsg string) {
	t.Helper()
	logs, err := s.Logs(taskID, seq)
	if err != nil {
		t.Fatalf("读取 %s/%d 日志失败: %v", taskID, seq, err)
	}
	if len(logs) != 1 || logs[0].Msg != wantMsg {
		t.Fatalf("%s/%d 日志不符，实得 %+v", taskID, seq, logs)
	}
}

// TestSystemLogs 系统程序日志：落库、分页（向上加载更早）、上限淘汰、清空。
func TestSystemLogs(t *testing.T) {
	s := newTestStore(t)

	// 写入 5 条
	for i := 1; i <= 5; i++ {
		entry, err := s.AppendSystemLog("INFO", fmt.Sprintf("msg%d", i), "")
		if err != nil {
			t.Fatalf("写入系统日志失败: %v", err)
		}
		if entry.Seq != uint64(i) {
			t.Fatalf("seq 应递增：第 %d 条 seq=%d", i, entry.Seq)
		}
	}

	// 最新 3 条（正序）
	logs, more, err := s.ListSystemLogs(3, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(logs) != 3 || logs[0].Msg != "msg3" || logs[2].Msg != "msg5" {
		t.Fatalf("最新批次错误: %+v", msgsOf(logs))
	}
	if !more {
		t.Fatal("还有更旧日志，应 has_more=true")
	}

	// 向上加载更早：before=msg3 的 seq（3）→ 应返回 msg1、msg2
	older, more2, err := s.ListSystemLogs(10, 3)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(older) != 2 || older[0].Msg != "msg1" || older[1].Msg != "msg2" {
		t.Fatalf("更早批次错误: %+v", msgsOf(older))
	}
	if more2 {
		t.Fatal("已到最早，不应 has_more")
	}

	// 清空
	if err := s.ClearSystemLogs(); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	logs, _, err = s.ListSystemLogs(10, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("清空后仍有日志: %+v", msgsOf(logs))
	}
	// 清空后 seq 不回绕（继续递增，前端 SSE 去重不受影响）
	entry, err := s.AppendSystemLog("WARN", "after-clear", "")
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if entry.Seq != 6 {
		t.Fatalf("清空后 seq 应继续递增，实得 %d", entry.Seq)
	}
}

// TestSystemLogsPrune 超出 maxSystemLogs 时自动淘汰最旧。
func TestSystemLogsPrune(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < maxSystemLogs+50; i++ {
		if _, err := s.AppendSystemLog("INFO", fmt.Sprintf("m%d", i), ""); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	logs, more, err := s.ListSystemLogs(maxSystemLogs, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(logs) != maxSystemLogs {
		t.Fatalf("应保留 %d 条，实得 %d", maxSystemLogs, len(logs))
	}
	if logs[0].Msg != "m50" || logs[len(logs)-1].Msg != fmt.Sprintf("m%d", maxSystemLogs+49) {
		t.Fatalf("淘汰范围错误：首=%s 尾=%s", logs[0].Msg, logs[len(logs)-1].Msg)
	}
	if more {
		t.Fatal("已淘汰到上限，不应 has_more")
	}
}

func msgsOf(logs []LogEntry) []string {
	out := make([]string, len(logs))
	for i, l := range logs {
		out[i] = l.Msg
	}
	return out
}

// TestSeqKeepsIncreasingAfterReopen 重启后新 run 的 seq 不能撞上已有记录。
func TestSeqKeepsIncreasingAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	seqA, err := s.Begin(Run{TaskID: "taskA"})
	if err != nil {
		t.Fatalf("Begin 失败: %v", err)
	}
	if err := s.Finish(seqA, StateSuccess, Counters{}, ""); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}
	s.Close()

	reopened, err := New(path)
	if err != nil {
		t.Fatalf("重开库失败: %v", err)
	}
	t.Cleanup(reopened.Close)
	seqB, err := reopened.Begin(Run{TaskID: "taskA"})
	if err != nil {
		t.Fatalf("Begin 失败: %v", err)
	}
	if seqB <= seqA {
		t.Fatalf("重开后 seq 未推进：旧 %d 新 %d", seqA, seqB)
	}
}

// TestDropAmbiguousLegacyLogs 打开 v1 遗留库时，被多任务共用的明细日志必须丢弃，
// 且之后的 run 不能复用遗留 seq。
func TestDropAmbiguousLegacyLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// 伪造 v1 数据：两个任务各有 seq=1，共用一条 runlogs。
	if err := db.Update(func(tx *bolt.Tx) error {
		rb, err := tx.CreateBucketIfNotExists(runsBucket)
		if err != nil {
			return err
		}
		lb, err := tx.CreateBucketIfNotExists(runLogsBucket)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(metaBucket); err != nil {
			return err
		}
		for _, taskID := range []string{"taskA", "taskB"} {
			tb, err := rb.CreateBucketIfNotExists([]byte(taskID))
			if err != nil {
				return err
			}
			seq, err := tb.NextSequence()
			if err != nil {
				return err
			}
			out, err := json.Marshal(Run{Seq: seq, TaskID: taskID, State: StateSuccess})
			if err != nil {
				return err
			}
			if err := tb.Put(seqKey(seq), out); err != nil {
				return err
			}
		}
		logs, err := json.Marshal([]LogEntry{{Seq: 1, Msg: "别人的日志"}})
		if err != nil {
			return err
		}
		return lb.Put(seqKey(1), logs)
	}); err != nil {
		t.Fatalf("写入遗留数据失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("打开遗留库失败: %v", err)
	}
	t.Cleanup(s.Close)

	for _, taskID := range []string{"taskA", "taskB"} {
		logs, err := s.Logs(taskID, 1)
		if err != nil {
			t.Fatalf("读取 %s 日志失败: %v", taskID, err)
		}
		if len(logs) != 0 {
			t.Fatalf("%s 的遗留串号日志应被丢弃，实得 %+v", taskID, logs)
		}
	}
	seq, err := s.Begin(Run{TaskID: "taskA"})
	if err != nil {
		t.Fatalf("Begin 失败: %v", err)
	}
	if seq <= 1 {
		t.Fatalf("新 seq 应避开遗留键，实得 %d", seq)
	}
}

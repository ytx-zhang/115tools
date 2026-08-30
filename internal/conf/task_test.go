package conf

import (
	"path/filepath"
	"strings"
	"testing"
)

// sampleTask 返回一个字段合法的任务（方向配置段由调用方按需设置）。
func sampleTask(kind TaskKind) Task {
	return Task{ID: "t_1", Name: "任务", Kind: kind, Enabled: true, LocalDir: "/本地", CloudDir: "/云端"}
}

func TestNormalizeFillsSegmentByKind(t *testing.T) {
	push := sampleTask(KindPush)
	push.normalize()
	if push.Push == nil {
		t.Fatal("push 任务未补齐 push 配置段")
	}
	if got := push.Push.Watch.QuietMinutes; got != defaultQuietMinutes {
		t.Fatalf("静默时间默认值 = %d, 期望 %d", got, defaultQuietMinutes)
	}
	if got := push.Push.Rescan.IntervalHours; got != defaultCronHours {
		t.Fatalf("全量扫描间隔默认值 = %d, 期望 %d", got, defaultCronHours)
	}

	pull := sampleTask(KindPull)
	pull.normalize()
	if pull.Pull == nil {
		t.Fatal("pull 任务未补齐 pull 配置段")
	}
	if got := pull.Pull.Cron.IntervalHours; got != defaultCronHours {
		t.Fatalf("同步间隔默认值 = %d, 期望 %d", got, defaultCronHours)
	}
}

// TestNormalizeDropsOtherSegment 改类型时，另一段配置必须清掉（不落盘、不残留生效）。
func TestNormalizeDropsOtherSegment(t *testing.T) {
	push := sampleTask(KindPush)
	push.Push = &PushOpts{ToStrm: true}
	push.Pull = &PullOpts{ToStrm: true}
	push.Kind = KindPull
	push.normalize()
	if push.Push != nil {
		t.Fatal("改为 pull 后仍残留 push 配置段")
	}
	if push.Pull == nil {
		t.Fatal("改为 pull 后未补齐 pull 配置段")
	}

	pull := sampleTask(KindPull)
	pull.Pull = &PullOpts{ArchiveToTemp: true}
	pull.Push = &PushOpts{ToCache: true}
	pull.Kind = KindPush
	pull.normalize()
	if pull.Pull != nil {
		t.Fatal("改为 push 后仍残留 pull 配置段")
	}
	if !pull.Push.ToCache {
		t.Fatal("push 段内容丢失")
	}
}

// TestNormalizeDropsEmptyAfterPull 附带扫描「无事可做」时自动去掉（AfterPull=nil）。
// 有效动作 = 下载（FetchMissing）或删冗余（DropRedundant）；to_strm 只是下载的子选项，
// 不下载就不生成 strm，单独勾选不算有效动作（正是「取消下载后残留 strm 勾选」的场景）。
func TestNormalizeDropsEmptyAfterPull(t *testing.T) {
	cleared := []struct {
		name string
		ap   *AttachOpts
	}{
		{"全空", &AttachOpts{}},
		{"只勾strm", &AttachOpts{ToStrm: true}},
		{"取消下载但strm残留", &AttachOpts{FetchMissing: false, ToStrm: true}},
	}
	for _, c := range cleared {
		p := sampleTask(KindPush)
		p.Push = &PushOpts{AfterPull: c.ap}
		p.normalize()
		if p.AttachEnabled() {
			t.Fatalf("%s 的附带扫描应被自动去掉，实得 %+v", c.name, p.Push.AfterPull)
		}
	}

	kept := []struct {
		name string
		ap   *AttachOpts
	}{
		{"fetch", &AttachOpts{FetchMissing: true}},
		{"drop", &AttachOpts{DropRedundant: true}},
		{"fetch+strm", &AttachOpts{FetchMissing: true, ToStrm: true}},
	}
	for _, c := range kept {
		p := sampleTask(KindPush)
		p.Push = &PushOpts{AfterPull: c.ap}
		p.normalize()
		if !p.AttachEnabled() {
			t.Fatalf("%s 的附带扫描不应被去掉", c.name)
		}
	}
}

func TestValidateRequiresMatchingSegment(t *testing.T) {
	push := sampleTask(KindPush)
	if err := push.Validate(); err == nil || !strings.Contains(err.Error(), "push 配置段") {
		t.Fatalf("push 任务缺配置段应报错，实得 %v", err)
	}
	pull := sampleTask(KindPull)
	if err := pull.Validate(); err == nil || !strings.Contains(err.Error(), "pull 配置段") {
		t.Fatalf("pull 任务缺配置段应报错，实得 %v", err)
	}
	ok := sampleTask(KindPush)
	ok.Push = &PushOpts{}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法任务被拒: %v", err)
	}
}

func TestPushCfgPullCfgSafeOnNil(t *testing.T) {
	var empty Task
	if empty.PushCfg().ToStrm || empty.PullCfg().ToStrm || empty.AttachCfg().DropRedundant || empty.AttachEnabled() {
		t.Fatal("空任务的方向配置应为零值")
	}
	push := sampleTask(KindPush)
	push.Push = &PushOpts{ToStrm: true, AfterPull: &AttachOpts{ToStrm: true, DropRedundant: true}}
	if !push.PushCfg().ToStrm || !push.AttachEnabled() || !push.AttachCfg().DropRedundant {
		t.Fatal("push 任务配置读取异常")
	}
	// 连带扫描未启用时读取应为零值而不是连带段的残留
	noAttach := sampleTask(KindPush)
	noAttach.Push = &PushOpts{}
	if noAttach.AttachEnabled() || noAttach.AttachCfg().ToStrm {
		t.Fatal("未启用连带扫描时 AttachCfg 应为零值")
	}
}

// TestAddTaskNormalizesSegment 新增任务时后端补齐配置段（前端只提交公共字段也能保存）。
func TestAddTaskNormalizesSegment(t *testing.T) {
	cfg, err := New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	task := sampleTask(KindPull)
	task.ID = ""
	if err := cfg.AddTask(task); err != nil {
		t.Fatalf("新增任务失败: %v", err)
	}
	stored, ok := cfg.GetTask(cfg.ListTasks()[0].ID)
	if !ok {
		t.Fatal("任务未落库")
	}
	if stored.Pull == nil || stored.Pull.Cron.IntervalHours != defaultCronHours {
		t.Fatalf("新增任务未归一化: %+v", stored)
	}
	if stored.Push != nil {
		t.Fatal("pull 任务不应有 push 段")
	}
}

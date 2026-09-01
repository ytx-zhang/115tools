package conf

import (
	"path/filepath"
	"strings"
	"testing"
)

// sampleTask 返回一个方向齐全的合法任务。
func sampleTask() Task {
	return Task{ID: "t_1", Name: "任务", Enabled: true, LocalDir: "/本地", CloudDir: "/云端",
		Upload: true, Download: true}
}

func TestNormalizeClearsWatchWhenUploadOff(t *testing.T) {
	tk := sampleTask()
	tk.Upload = false
	tk.Watch = true
	tk.InstantNow = true
	tk.QuietMinutes = 5
	tk.normalize()
	if tk.Watch {
		t.Fatal("未开上传时监听应失效")
	}
	if tk.InstantNow {
		t.Fatal("未开上传时监听细化开关应失效")
	}
	if tk.QuietMinutes != 5 {
		t.Fatalf("不应改动用户填的静默时间: %d", tk.QuietMinutes)
	}
}

func TestNormalizeClearsArchiveWhenUploadOn(t *testing.T) {
	tk := sampleTask()
	tk.Archive = true
	tk.normalize()
	if tk.Archive {
		t.Fatal("开启上传时归档应被强制关闭（纯下载专用）")
	}

	// 纯下载（未开上传）时归档保留
	tk2 := sampleTask()
	tk2.Upload = false
	tk2.Archive = true
	tk2.normalize()
	if !tk2.Archive {
		t.Fatal("纯下载任务归档应保留")
	}
}

func TestDefaultsFilledOnRead(t *testing.T) {
	var empty Task
	if got := empty.QuietWindow(); got != defaultQuietMinutes {
		t.Fatalf("静默时间默认值 = %d, 期望 %d", got, defaultQuietMinutes)
	}
	if got := empty.CronInterval(); got != defaultCronHours {
		t.Fatalf("定时间隔默认值 = %d, 期望 %d", got, defaultCronHours)
	}
}

func TestValidateRequiresADirection(t *testing.T) {
	tk := sampleTask()
	tk.Upload, tk.Download, tk.Archive = false, false, false
	if err := tk.Validate(); err == nil || !strings.Contains(err.Error(), "至少") {
		t.Fatalf("两个方向都不开应报错，实得 %v", err)
	}

	// 只开归档也合法（归档属于下载作用域）
	tk.Archive = true
	if err := tk.Validate(); err != nil {
		t.Fatalf("只开归档应合法: %v", err)
	}
}

func TestValidateRequiresAbsPaths(t *testing.T) {
	tk := sampleTask()
	tk.LocalDir = "相对路径"
	if err := tk.Validate(); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("应拒绝相对路径，实得 %v", err)
	}
	tk.LocalDir = "/本地"
	tk.CloudDir = "media"
	if err := tk.Validate(); err == nil || !strings.Contains(err.Error(), "/ 开头") {
		t.Fatalf("应拒绝不带斜杠的云端路径，实得 %v", err)
	}
}

func TestValidateRejectsEmptyName(t *testing.T) {
	tk := sampleTask()
	tk.Name = "  "
	if err := tk.Validate(); err == nil || !strings.Contains(err.Error(), "任务名") {
		t.Fatalf("应拒绝空任务名，实得 %v", err)
	}
}

func TestValidateOverlaps(t *testing.T) {
	tk := sampleTask()
	tk2 := sampleTask()
	tk2.Name = "任务二"
	tk2.LocalDir = "/本地/子"
	if err := validateTasks([]Task{tk, tk2}); err == nil || !strings.Contains(err.Error(), "重叠") {
		t.Fatalf("嵌套目录应报冲突，实得 %v", err)
	}

	tk2.LocalDir = "/本地2"
	tk2.CloudDir = "/云端/子"
	if err := validateTasks([]Task{tk, tk2}); err == nil || !strings.Contains(err.Error(), "重叠") {
		t.Fatalf("云端目录嵌套也应报冲突，实得 %v", err)
	}

	tk2.CloudDir = "/云端2"
	if err := validateTasks([]Task{tk, tk2}); err != nil {
		t.Fatalf("不重叠的目录应通过: %v", err)
	}
}

func TestAddTaskNormalizesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	tk := sampleTask()
	tk.ID = ""
	tk.Watch = true
	if err := cfg.AddTask(tk); err != nil {
		t.Fatalf("新增任务失败: %v", err)
	}
	stored, ok := cfg.GetTask(cfg.ListTasks()[0].ID)
	if !ok {
		t.Fatal("任务未落库")
	}
	if !stored.Watch || !stored.Upload {
		t.Fatalf("新增任务未落库开关: %+v", stored)
	}

	// 落盘后重读，应能完整还原（配置无版本号概念，三层结构 round-trip）
	cfg2, err := New(path)
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	re, ok := cfg2.GetTask(stored.ID)
	if !ok {
		t.Fatal("重读后任务丢失")
	}
	if re.Name != stored.Name || re.LocalDir != stored.LocalDir || !re.Watch || !re.Upload {
		t.Fatalf("重读后任务字段不一致: %+v", re)
	}
}

package drive

// compat_test.go —— 115 开放平台响应兼容性回归测试（record/replay）。
//
// 这些测试不发起任何网络请求：原始响应体由 live 探针（用真实 token 抓取、2026-08-26）
// 落盘在 testdata/115_*.json，此处仅「重放」它们，断言解析逻辑与真实格式一致。
//
// 实测结论（非猜测，均有 testdata 样本佐证）：
//   - 普通 API 外壳固定 {state:bool, code:int, message:string, data:...}；
//     仅 refreshToken 接口的 state 是 int（1=成功），二者需区分。
//   - 整数字段（count/folder_count/size/status/isv/fs）实测均为 JSON 数字，
//     故直接用 int64 解析，无需 IntString 双兼容。
//   - 查询类接口「空结果」返回 state=true + data=[]（get_info 不存在、downurl 空、
//     删除成功等），因此必须用 StructOrArray[jsontext.Value] 兜住 data 段，
//     绝不能用 Post[struct{}]（会因 data=[] 解析失败）。
//   - 全部样本无重复字段名、无非法 UTF-8，故走 v2 默认严格选项，不引入宽松选项。

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain 在缺少测试样本时静默跳过整个套件。
// 样本（testdata/）含真实 115 账户数据（token/直链/账户名），已 gitignore 不入库；
// 仅本地存在样本时才跑，他人 clone 后 `go test` 不会因读不到样本而报错。
func TestMain(m *testing.M) {
	if _, err := os.Stat(filepath.Join("testdata", "115_user_info.json")); err != nil {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// load115 读取 testdata 下的真实响应体。
func load115(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return b
}

// parseShell 用 v2 默认严格选项解析 115 响应外壳（Resp[jsontext.Value]）。
// 不传任何宽松选项——真实数据已证明无需 AllowDuplicateNames/AllowInvalidUTF8。
func parseShell(t *testing.T, body []byte) Resp[jsontext.Value] {
	t.Helper()
	var shell Resp[jsontext.Value]
	if err := json.Unmarshal(body, &shell); err != nil {
		t.Fatalf("严格解析外壳失败: %v\nbody=%s", err, body)
	}
	return shell
}

// unmarshalData 把外壳的 data 段解析到 T（要求外壳 state=true 时才调用）。
func unmarshalData[T any](t *testing.T, shell Resp[jsontext.Value]) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(shell.Data, &v); err != nil {
		t.Fatalf("解析 data 段失败: %v\nbody=%s", err, shell.Data)
	}
	return v
}

// ──── 外壳：state=bool / code=int（普通 API）────

func TestRespShell_SuccessStateBool(t *testing.T) {
	// state=true 且 code=0 的成功样本（含空结果 data=[] 的样本）
	success := []string{
		"115_user_info.json", "115_file_list.json", "115_file_list_temp.json",
		"115_folder_add.json", "115_folder_get_info.json", "115_folder_get_info_strm.json",
		"115_downurl_ok.json", "115_downurl_dir.json", "115_downurl_invalid.json",
		"115_offline_list.json", "115_offline_add_invalid.json",
		"115_upload_init.json", "115_ufile_delete.json",
	}
	for _, f := range success {
		t.Run(f, func(t *testing.T) {
			shell := parseShell(t, load115(t, f))
			if !shell.State {
				t.Errorf("%s: State=%v (期望 true)", f, shell.State)
			}
			if shell.Code != 0 {
				t.Errorf("%s: Code=%v (期望 0)", f, shell.Code)
			}
		})
	}
}

func TestRespShell_FailureStateBoolCodeInt(t *testing.T) {
	// state=false 的失败样本：code 是 int，message 是中文语义
	fail := map[string]int64{
		"115_folder_add_duplicate.json":  20004,
		"115_offline_add_duplicate.json": 10008,
		"115_ufile_delete_missing.json":  990003,
	}
	for f, wantCode := range fail {
		t.Run(f, func(t *testing.T) {
			shell := parseShell(t, load115(t, f))
			if shell.State {
				t.Errorf("%s: 期望 state=false", f)
			}
			if shell.Code != wantCode {
				t.Errorf("%s: Code=%d (期望 %d)", f, shell.Code, wantCode)
			}
		})
	}
}

// ──── 文件列表：fc/aid 是 string，isv 是 int ────

func TestFileList_RootDirs(t *testing.T) {
	shell := parseShell(t, load115(t, "115_file_list.json"))
	resp := unmarshalData[[]fileListResponse](t, shell)
	if len(resp) != 7 {
		t.Fatalf("根目录条目数=%d (期望 7)", len(resp))
	}
	if resp[0].Fid == "" || resp[0].PickCode == "" {
		t.Error("目录项应含 fid/pick_code")
	}
	if resp[0].IsDir != "0" { // fc="0" 表示目录（字符串）
		t.Errorf("根目录项 IsDir=%q (期望 0)", resp[0].IsDir)
	}
}

func TestFileList_FileItemTypes(t *testing.T) {
	shell := parseShell(t, load115(t, "115_file_list_temp.json"))
	resp := unmarshalData[[]fileListResponse](t, shell)
	var file *fileListResponse
	for i := range resp {
		if resp[i].IsDir != "0" {
			file = &resp[i]
			break
		}
	}
	if file == nil {
		t.Fatal("未找到文件项（fc != 0）")
	}
	if file.Aid != "1" { // aid 是字符串
		t.Errorf("Aid=%q (期望 \"1\")", file.Aid)
	}
	if file.IsVideo != 1 { // isv 实测 int
		t.Errorf("IsVideo=%d (期望 1)", file.IsVideo)
	}
}

// ──── 目录创建：同名冲突 ────

func TestFolderAdd_Duplicate(t *testing.T) {
	shell := parseShell(t, load115(t, "115_folder_add_duplicate.json"))
	if shell.State {
		t.Error("同名目录创建应 state=false")
	}
	// CreateFolder 用 strings.Contains(err, "该目录名称已存在") 幂等复用 FID，
	// 故 message 必须含该语义信号。
	if !strings.Contains(shell.Message, "该目录名称已存在") {
		t.Errorf("message 应含「该目录名称已存在」，实际 %q", shell.Message)
	}
}

// ──── get_info：对象 / 空数组 / 大目录 count 为 int ────

func TestGetDirInfo_Exists(t *testing.T) {
	shell := parseShell(t, load115(t, "115_folder_get_info.json"))
	resp := unmarshalData[StructOrArray[DirInfo]](t, shell)
	if resp.Value == nil {
		t.Fatal("已存在目录应解析为对象，Value 非 nil")
	}
	if resp.Value.Fid == "" {
		t.Error("DirInfo.Fid 为空")
	}
	if resp.Value.FileCount != 0 || resp.Value.FolderCount != 0 {
		t.Errorf("空目录 count=%d folder_count=%d (期望均 0)", resp.Value.FileCount, resp.Value.FolderCount)
	}
}

func TestGetDirInfo_Missing(t *testing.T) {
	shell := parseShell(t, load115(t, "115_folder_get_info_missing.json"))
	if !shell.State {
		t.Error("get_info 不存在路径返回 state=true（非报错），data=[]")
	}
	resp := unmarshalData[StructOrArray[DirInfo]](t, shell)
	if resp.Value != nil {
		t.Error("不存在路径 data=[]，StructOrArray 应 Value=nil")
	}
}

func TestGetDirInfo_CountIsInt(t *testing.T) {
	// 大目录 count/folder_count 实测为 JSON 数字，int64 直接解析（无需 IntString）
	shell := parseShell(t, load115(t, "115_folder_get_info_strm.json"))
	resp := unmarshalData[StructOrArray[DirInfo]](t, shell)
	if resp.Value == nil {
		t.Fatal("已存在目录应 Value 非 nil")
	}
	if resp.Value.FileCount != 49937 {
		t.Errorf("FileCount=%d (期望 49937)", resp.Value.FileCount)
	}
	if resp.Value.FolderCount != 688 {
		t.Errorf("FolderCount=%d (期望 688)", resp.Value.FolderCount)
	}
}

// ──── downurl：成功对象 / 空数组 ────

func TestDownloadUrl_Ok(t *testing.T) {
	shell := parseShell(t, load115(t, "115_downurl_ok.json"))
	resp := unmarshalData[StructOrArray[map[string]downItem]](t, shell)
	if resp.Value == nil {
		t.Fatal("downurl 成功应返回对象，Value 非 nil")
	}
	if len(*resp.Value) != 1 {
		t.Fatalf("downurl 成功应 1 条，得 %d", len(*resp.Value))
	}
	for fid, item := range *resp.Value {
		if fid == "" || item.FileName == "" {
			t.Error("downItem 应含 fid(file_name)")
		}
		if item.Url.Url == "" {
			t.Error("downurl 成功的 url 应为非空")
		}
	}
}

func TestDownloadUrl_Empty(t *testing.T) {
	// 目录/无效 pickcode 返回 data=[]，StructOrArray 按空结果放行
	for _, f := range []string{"115_downurl_dir.json", "115_downurl_invalid.json"} {
		t.Run(f, func(t *testing.T) {
			shell := parseShell(t, load115(t, f))
			resp := unmarshalData[StructOrArray[map[string]downItem]](t, shell)
			if resp.Value != nil {
				t.Errorf("空结果应 Value=nil，得 %d 条", len(*resp.Value))
			}
		})
	}
}

// ──── 删除：成功 data=[] / 不存在 state=false ────

func TestUfileDelete_EmptyArray(t *testing.T) {
	body := load115(t, "115_ufile_delete.json")
	// 反例：只关心 state 的操作若用 Post[struct{}]，data=[] 会解析失败
	type wrapper struct {
		Data struct{} `json:"data"`
	}
	var w wrapper
	if err := json.Unmarshal(body, &w); err == nil {
		t.Fatal("Post[struct{}] 未失败，与 115 当前 data=[] 行为矛盾")
	}
	// 正例：外壳用 Resp[jsontext.Value]，data 段不强制解析
	var shell Resp[jsontext.Value]
	if err := json.Unmarshal(body, &shell); err != nil {
		t.Fatalf("Resp[jsontext.Value] 解析失败: %v", err)
	}
	if !shell.State {
		t.Error("删除成功应 state=true")
	}
}

func TestUfileDelete_Missing(t *testing.T) {
	shell := parseShell(t, load115(t, "115_ufile_delete_missing.json"))
	if shell.State {
		t.Error("删除不存在 fid 应 state=false")
	}
}

// ──── 离线下载：列表 / 无效 / 重复 ────

func TestOfflineList(t *testing.T) {
	shell := parseShell(t, load115(t, "115_offline_list.json"))
	resp := unmarshalData[OfflineTaskPage](t, shell)
	if resp.Count != 1 {
		t.Errorf("Count=%d (期望 1)", resp.Count)
	}
	if len(resp.Tasks) != 1 {
		t.Fatalf("任务数=%d (期望 1)", len(resp.Tasks))
	}
	if resp.Tasks[0].Status != 1 { // status 实测 int
		t.Errorf("Status=%d (期望 1)", resp.Tasks[0].Status)
	}
}

func TestOfflineAdd_Invalid(t *testing.T) {
	shell := parseShell(t, load115(t, "115_offline_add_invalid.json"))
	if !shell.State {
		t.Error("offline add 外壳应 state=true")
	}
	resp := unmarshalData[[]OfflineAddResult](t, shell)
	if len(resp) != 1 {
		t.Fatalf("应 1 条结果，得 %d", len(resp))
	}
	if !resp[0].State {
		t.Error("单条结果 state 应 true")
	}
	if resp[0].InfoHash == "" {
		t.Error("info_hash 应非空")
	}
}

func TestOfflineAdd_Duplicate(t *testing.T) {
	shell := parseShell(t, load115(t, "115_offline_add_duplicate.json"))
	if shell.State {
		t.Error("重复添加应 state=false")
	}
	if !strings.Contains(shell.Message, "任务已存在") {
		t.Errorf("message 应含「任务已存在」，实际 %q", shell.Message)
	}
}

// ──── 上传初始化 ────

func TestUploadInit(t *testing.T) {
	shell := parseShell(t, load115(t, "115_upload_init.json"))
	resp := unmarshalData[uploadInitResp](t, shell)
	if resp.Status != 4 {
		t.Errorf("Status=%d (期望 4)", resp.Status)
	}
	if resp.Callback.Value != nil { // callback=[]，StructOrArray 容错
		t.Error("callback=[] 应 Value=nil")
	}
}

// ──── StructOrArray 多形态单元测试 ────

func TestStructOrArray_Shapes(t *testing.T) {
	// 对象形态 → 解析成功
	var obj StructOrArray[DirInfo]
	if err := json.Unmarshal([]byte(`{"file_id":"123"}`), &obj); err != nil {
		t.Fatalf("对象形态解析失败: %v", err)
	}
	if obj.Value == nil || obj.Value.Fid != "123" {
		t.Error("对象形态应解析出 Value")
	}
	// 非对象形态（[]/false/null/空）→ 一律放行，Value=nil
	for _, b := range []string{`[]`, `false`, `null`, `""`} {
		var s StructOrArray[DirInfo]
		if err := json.Unmarshal([]byte(b), &s); err != nil {
			t.Errorf("非对象形态 %s 应放行: %v", b, err)
		}
		if s.Value != nil {
			t.Errorf("非对象形态 %s 应 Value=nil", b)
		}
	}
}

// ──── prettyJSON：解码 \uXXXX 与缩进 ────

func TestPrettyJSON(t *testing.T) {
	out := prettyJSON(load115(t, "115_user_info.json"))
	if !strings.Contains(out, "颜太吓") {
		t.Errorf("prettyJSON 未把 \\uXXXX 解码为中文明文:\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Error("prettyJSON 未缩进")
	}
	if got := prettyJSON([]byte("not json at all")); got != "not json at all" {
		t.Errorf("非 JSON 应原样返回，得到 %q", got)
	}
}

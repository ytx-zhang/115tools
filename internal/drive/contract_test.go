package drive

// contract_test.go —— 115 开放平台响应兼容性回归测试（record/replay）。
//
// 不发起网络请求：原始响应体由 live 探针抓取落盘在 testdata/115_*.json，此处仅重放，
// 断言解析逻辑与真实格式一致。实测钉死的契约：
//   - 普通 API 外壳 {state:bool, code:int, message:string, data:...}；仅 refreshToken 接口 state 是 int；
//   - 整数字段（count/folder_count/size/status/isv/fs）均为 JSON 数字，用 int64 解析；
//   - 查询类「空结果」返回 state=true + data=[]，必须用 StructOrArray 兜住，绝不能用 Post[struct{}]；
//   - 全部样本无重复字段名、无非法 UTF-8，走 json/v2 默认严格选项。

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain 在缺少样本时静默跳过（样本含真实账户数据，gitignore 不入库）。
func TestMain(m *testing.M) {
	if _, err := os.Stat(filepath.Join("testdata", "115_user_info.json")); err != nil {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func load115(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return b
}

func parseShell(t *testing.T, body []byte) Resp[jsontext.Value] {
	t.Helper()
	var shell Resp[jsontext.Value]
	if err := json.Unmarshal(body, &shell); err != nil {
		t.Fatalf("严格解析外壳失败: %v\nbody=%s", err, body)
	}
	return shell
}

func unmarshalData[T any](t *testing.T, shell Resp[jsontext.Value]) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(shell.Data, &v); err != nil {
		t.Fatalf("解析 data 段失败: %v\nbody=%s", err, shell.Data)
	}
	return v
}

func TestRespShell_SuccessStateBool(t *testing.T) {
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

func TestRefreshToken_StateIsInt(t *testing.T) {
	body := load115(t, "115_refresh.json")
	var shell Resp[jsontext.Value]
	if err := json.Unmarshal(body, &shell); err == nil {
		t.Fatal("通用外壳(State bool)解析 state=1 未失败，与 115 行为矛盾")
	}
	var res struct {
		State   int64  `json:"state"`
		Code    int64  `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			ExpiresIn    int64  `json:"expires_in"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("刷新响应解析失败: %v", err)
	}
	if res.State != 1 || res.Code != 0 {
		t.Errorf("State=%d Code=%d (期望 1/0)", res.State, res.Code)
	}
	if res.Data.AccessToken == "" || res.Data.RefreshToken == "" {
		t.Error("刷新成功应返回 access_token 与 refresh_token")
	}
	if res.Data.ExpiresIn != 7200 {
		t.Errorf("ExpiresIn=%d (期望 7200)", res.Data.ExpiresIn)
	}
}

func TestFileList_RootDirs(t *testing.T) {
	shell := parseShell(t, load115(t, "115_file_list.json"))
	resp := unmarshalData[[]fileListResponse](t, shell)
	if len(resp) != 7 {
		t.Fatalf("根目录条目数=%d (期望 7)", len(resp))
	}
	if resp[0].Fid == "" || resp[0].PickCode == "" {
		t.Error("目录项应含 fid/pick_code")
	}
	if resp[0].IsDir != "0" {
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
	if file.Aid != "1" {
		t.Errorf("Aid=%q (期望 \"1\")", file.Aid)
	}
	if file.IsVideo != 1 {
		t.Errorf("IsVideo=%d (期望 1)", file.IsVideo)
	}
}

func TestFolderAdd_Duplicate(t *testing.T) {
	shell := parseShell(t, load115(t, "115_folder_add_duplicate.json"))
	if shell.State {
		t.Error("同名目录创建应 state=false")
	}
	if !strings.Contains(shell.Message, "该目录名称已存在") {
		t.Errorf("message 应含「该目录名称已存在」，实际 %q", shell.Message)
	}
}

func TestGetDirInfo_Exists(t *testing.T) {
	shell := parseShell(t, load115(t, "115_folder_get_info.json"))
	resp := unmarshalData[StructOrArray[DirInfo]](t, shell)
	if resp.Value == nil {
		t.Fatal("已存在目录应解析为对象，Value 非 nil")
	}
	if resp.Value.Fid == "" {
		t.Error("DirInfo.Fid 为空")
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
	shell := parseShell(t, load115(t, "115_folder_get_info_strm.json"))
	resp := unmarshalData[StructOrArray[DirInfo]](t, shell)
	if resp.Value == nil {
		t.Fatal("已存在目录应 Value 非 nil")
	}
	if resp.Value.FileCount != 49937 || resp.Value.FolderCount != 688 {
		t.Errorf("FileCount=%d FolderCount=%d (期望 49937/688)", resp.Value.FileCount, resp.Value.FolderCount)
	}
}

func TestDownloadUrl_Ok(t *testing.T) {
	shell := parseShell(t, load115(t, "115_downurl_ok.json"))
	resp := unmarshalData[StructOrArray[map[string]downItem]](t, shell)
	if resp.Value == nil || len(*resp.Value) != 1 {
		t.Fatalf("downurl 成功应 1 条对象，得 %+v", resp.Value)
	}
	for _, item := range *resp.Value {
		if item.FileName == "" || item.URL.URL == "" {
			t.Error("downItem 应含 file_name 与 url")
		}
	}
}

func TestDownloadUrl_Empty(t *testing.T) {
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

func TestUfileDelete_EmptyArray(t *testing.T) {
	body := load115(t, "115_ufile_delete.json")
	type wrapper struct {
		Data struct{} `json:"data"`
	}
	var w wrapper
	if err := json.Unmarshal(body, &w); err == nil {
		t.Fatal("Post[struct{}] 未失败，与 115 当前 data=[] 行为矛盾")
	}
	var shell Resp[jsontext.Value]
	if err := json.Unmarshal(body, &shell); err != nil {
		t.Fatalf("Resp[jsontext.Value] 解析失败: %v", err)
	}
	if !shell.State {
		t.Error("删除成功应 state=true")
	}
}

func TestOfflineList(t *testing.T) {
	shell := parseShell(t, load115(t, "115_offline_list.json"))
	resp := unmarshalData[OfflineTaskPage](t, shell)
	if resp.Count != 1 || len(resp.Tasks) != 1 {
		t.Fatalf("Count=%d 任务数=%d (期望 1/1)", resp.Count, len(resp.Tasks))
	}
	if resp.Tasks[0].Status != 1 {
		t.Errorf("Status=%d (期望 1)", resp.Tasks[0].Status)
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

func TestUploadInit(t *testing.T) {
	shell := parseShell(t, load115(t, "115_upload_init.json"))
	resp := unmarshalData[uploadInitResp](t, shell)
	if resp.Status != 4 {
		t.Errorf("Status=%d (期望 4)", resp.Status)
	}
	if resp.Callback.Value != nil {
		t.Error("callback=[] 应 Value=nil")
	}
}

func TestStructOrArray_Shapes(t *testing.T) {
	var obj StructOrArray[DirInfo]
	if err := json.Unmarshal([]byte(`{"file_id":"123"}`), &obj); err != nil {
		t.Fatalf("对象形态解析失败: %v", err)
	}
	if obj.Value == nil || obj.Value.Fid != "123" {
		t.Error("对象形态应解析出 Value")
	}
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

func TestPrettyJSON(t *testing.T) {
	out := prettyJSON(load115(t, "115_user_info.json"))
	if !strings.Contains(out, "\n") {
		t.Error("prettyJSON 未缩进")
	}
	if got := prettyJSON([]byte("not json at all")); got != "not json at all" {
		t.Errorf("非 JSON 应原样返回，得到 %q", got)
	}
}

package core

import "time"

// Paths 集中保存三个功能模块（local/cloud/strm）共同使用的路径配置与云端目录 FID。
//
// 字段分两类：
//   - 配置类（SyncPath/TempPath/StrmPath/StrmUrl/Settle）：来自 config.yaml，
//     在 NewEnv 时写入，之后只读；
//   - 初始化补全类（SyncFid/TempFid）：程序启动时由 bootstrap 阶段查询云端
//     （或读取本地数据库缓存）后回填，之后只读。
//
// 因为存在「先构造、后回填 FID」的过程，所以 Paths 以指针形式存放在 Env 中，
// 各模块持有的是同一份，bootstrap 回填的结果对所有模块立即可见。
type Paths struct {
	SyncPath string        // 主同步目录（本地与云端保持一致的根目录）
	SyncFid  string        // 主同步目录对应的云端 FID（bootstrap 阶段回填）
	TempPath string        // 云端回收/临时目录（待清理的云端文件先移入这里，而非直接删除）
	TempFid  string        // 回收目录的云端 FID（bootstrap 阶段回填）
	StrmPath string        // STRM 生成的起始目录（云端媒体库根目录）
	StrmUrl  string        // STRM 文件内容中的 302 直链前缀（指向本程序的 /download 接口）
	Settle   time.Duration // 本地同步静默窗口：文件事件后等待该时长内无新事件才真正同步
}

// SettleDuration 把配置里的秒数转换为静默窗口时长。
// 配置为 0 或负数时使用默认值 15 秒，防止窗口过短导致「文件还在写入就开始上传」。
func SettleDuration(secs int) time.Duration {
	if secs <= 0 {
		return 15 * time.Second
	}
	return time.Duration(secs) * time.Second
}

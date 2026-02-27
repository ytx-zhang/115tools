# 115tools

> 一套围绕 115 网盘服务的 Go 工具，提供文件同步、`.strm` 文件生成/消费、Emby 302 转向和相关辅助功能。

---

## 🔍 项目概述

`115tools` 由多个独立但互相协作的包组成，旨在简化与 115 网盘交互的常见任务：

- 自动**本地文件夹与115云盘**之间的双向同步
- 在本地生成或下载 `.strm` 文件并管理云端资源
- 提供 **HTTP API** 作为控制面板，同时包含 **Server‑Sent Events** 实时状态
- 代理 Emby 请求并执行 302 重定向优化
- 封装115的 REST API，含速率控制和重试逻辑

适合家庭媒体服务器用户、Emby/Jellyfin 环境或想用脚本自动化 115 网盘操作的开发者。

---

## 📂 目录结构

```
compose.yaml            # docker-compose 配置
Dockerfile              # 镜像构建文件
go.mod                  # Go 模块定义
index.html              # Web 界面（简单控制面板）
main.go                 # 程序入口

addStrm/                # .strm 任务处理
config/                 # 配置加载与 token 刷新
emby302/                # Emby 代理与 302 逻辑
open115/                # 115 平台 API 封装
strmServer/             # 简单的 strm 文件下载重定向
syncFile/               # 本地与云端同步逻辑 + BoltDB 存储
```

---

## ⚙️ 配置说明

配置文件位于 `data/config.yaml`，启动时由 `config.LoadConfig` 加载并在运行中定期刷新 Token。

### 配置结构

```yaml
token:
  access_token: ""
  refresh_token: "<必填>"
  expire_at: ""       # 自动写入

sync_path: "/path/to/local/folder"
strm_path: "/Cloud/StrmFolder"
temp_path: "/Cloud/TempFolder"
strm_url: "http://your-server:8080"
emby_url: "http://emby.local:8095"
fontinass_url: "http://subtitle-proxy.local"
```

- **token** – 115 API 凭据，必须提供 `refresh_token`。
- **sync_path** – 本地待同步目录。将与云盘保持镜像状态。
- **strm_path** – 云盘中用于生成 `.strm` 文件的根目录。
- **temp_path** – 临时移动/清理操作使用的云盘目录。
- **strm_url** – 服务器地址，用于生成 `.strm` 内容。
- **emby_url** 与 **fontinass_url** – 代理 Emby 与字幕服务的地址。

> 注意：`config` 包会自动在后台每隔 1–5 分钟刷新 Access Token。

---

## 🛠️ 模块详细说明

### `config`

- 管理配置结构与并发访问
- 自动刷新 115 `access_token` (`refreshToken` 模式)
- 提供 `config.Get()` 全局访问

### `open115`

115 REST API 封装，包含：

- 通用 `request` 函数，带速率限制、重试与错误解析
- 文件/文件夹操作：`UploadFile`、`AddFolder`、`MoveFile`、`DeleteFile`、`UpdataFile`
- 列表与信息：`FolderInfo`、`FileList`、`GetDownloadUrl`
- 支持 OSS 上传流程和 SHA1 校验

内部使用了令牌桶速率限制与单例信号量保护，确保不触发 115 限制。

### `syncFile`

负责本地与 115 云盘目录的同步。核心特点：

- 使用 BoltDB (`/app/data/files.db`) 缓存 `本地路径 -> (fid|size)`
- 扫描云端与本地，比较差异并：
  - 上传新文件（大于 10MB 的视频会转为 `.strm`）
  - 创建缺失的云端文件夹
  - 清理云盘上已删除/移动的项（可安全移至 `temp_path`）

相关文件：`syncFile.go`、`db.go`。

### `addStrm`

用于在本地生成或下载 `.strm` 文件并移动相应的云盘资源：

- 遍历 `strm_path` 目录
- 对视频文件生成 `.strm` 内容，或者下载文件
- 下载完成后（且无错误）将对应的云盘文件移动到 `temp_path`

### `strmServer`

- `RedirectToRealURL` HTTP 处理器
- 接受 `?pickcode=...` 请求，使用 User-Agent 作为缓存键
- 缓存 10 分钟，防止频繁查询 115 并减少带宽
- 支持并发请求去重（pendingTasks map）

### `emby302`

为 Emby 提供智能代理：

- 监听 `:8095`，将请求转发到真实 Emby
- 在视频 `/original` 链接中尝试获取最终 CDN 地址并返回 302
- 对字幕请求可重定向到独立服务
- 注入自定义 JS 修复跨域/超时逻辑，增强播放稳定性

### `main.go`

入口程序：

- 加载配置，启动日志化
- 注册 `/sync`, `/strm`, `/logs`, `/download` 等接口
- 启动 `emby302` 后台服务

---

## 📡 HTTP 控制接口

| 方法 | 路径         | 描述                            |
|------|--------------|----------------------------------|
| GET  | `/sync`      | 启动/重试同步任务              |
| POST | `/sync`      | 同上                            |
| GET  | `/stopsync`  | 取消正在进行的同步              |
| GET  | `/strm`      | 启动/重试 `.strm` 添加任务      |
| GET  | `/stopstrm`  | 停止 `.strm` 添加任务           |
| GET  | `/logs`      | SSE 实时推送全局状态            |
| GET  | `/download`  | `strmServer.RedirectToRealURL`  |

服务器监听 `:8080`，`/logs` 返回的数据示例：

```json
{"sync":{"total":10,"completed":2,...},"strm":{...}}
```

---

## 🚀 运行方式

### 本地运行

```bash
go run main.go
```

务必先编辑 `data/config.yaml` 并确保 `token.refresh_token` 可用。

### Docker/Compose

```bash
docker build -t 115tools .
docker-compose up -d
```

`compose.yaml` 已预定义数据卷和端口映射，配置文件与数据库位于宿主机 `./data`。

---

## 📝 许可证

本项目采用 **MIT 许可证**，你可以自由使用、修改并分发，详见 LICENSE 文件。

---

> 有任何问题或功能建议，欢迎提出 Issue 或直接修改代码后发起 PR。

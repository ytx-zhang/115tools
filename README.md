
# 115tools

`115tools` 是一套面向 115 网盘的辅助工具集，主要功能包括本地与云端文件同步、`.strm` 文件的生成、以及为 Emby 提供 302 直连以优化播放体验。

### 100% AI 编写

## 本人没学过编程！

# 使用请注意：本项目由 AI 生成，功能可能不完善，风险自负

## ✨ 核心功能

  - 本地目录与 115 云盘的双向一致性维护。
  - 本地扫描：检测新增/变更文件并上传；对于大于 10MB 的视频，会在上传后在本地生成 `.strm` 索引并把原文件移动到云端。
  - 云端扫描：检查云端是否有新文件或变更，必要时自动从云端下载并在本地生成 `.strm` 索引文件。
  - 自动清理：对本地已删除的文件执行云端清理（视频文件和文件夹移入 `temp_path` 指定的临时目录）。

- STRM 生成与管理（`addStrm`）
  - 递归扫描指定云端目录，批量生成 `.strm` 文件，其他文件下载到本地。

- Emby 302 直链代理（`emby302`）
  - 代理 Emby 请求，不需要转码的视频 302 指向 115 的直链。
  - 支持字幕重定向到fontinass，使用他的noproxy标签镜像不用套娃反代了。

## 🚀 快速上手

### 1) 获取凭据

准备好 115 的 `refresh_token`（可通过openlist的115 Open 接口文档获取），用于在容器内的 `data/config.yaml` 中配置。

### 2) 编辑配置

在 `data/config.yaml` 中填写必要字段，例如：

```yaml
token:
  refresh_token: "你的_115_REFRESH_TOKEN"

sync_path: "/strm媒体库"     # 容器内用于同步的本地目录（必须映射到宿主并与云端路径一致）
strm_path: "/待刮削"    # 容器内用于生成/存放 .strm 的共享目录（必须映射到宿主并与云端路径一致）
temp_path: "/temp"             # 云端临时/回收目录
strm_url: "http://ip:8080" # 本服务地址，用于 .strm 文件中的播放 URL和web页面
emby_url: "http://ip:8095" # 原始 Emby 服务器地址（代理目标）
fontinass_url: ""                  # 可选：外挂字幕代理地址
```

重要：`sync_path` 与 `strm_path` 是“宿主 ↔ 容器 ↔ 云端”三级关联的公用目录。使用 Docker 部署时，必须在卷映射中把宿主目录正确挂载到容器内的 `sync_path` 和 `strm_path`。容器内路径必须与配置中一致，否则同步与 STRM 功能无法正常工作。

示例（docker-compose 中的卷映射示意）：

```yaml
services:
  115tools:
    image: 115tools:latest
    container_name: 115tools
    restart: always
    volumes:
      - ./data:/app/data                    # 配置与 DB
      - /media/strm媒体库:/strm媒体库       # 对应 config.sync_path
      - /media/待刮削:/待刮削       # 对应 config.strm_path
    ports:
      - "8080:8080"  # 管理面板 & strm URL
      - "8095:8095"  # emby302 代理端口
```

## 🛠️ 使用说明

- 管理面板：访问 `http://<host>:8080`，界面提供手动触发 `sync` / `strm` 任务以及 SSE 实时状态（`/logs`）。
- 同步触发：调用 `GET /sync` 或 `POST /sync` 可启动同步任务；`GET /stopsync` 停止。
- STRM 触发：调用 `GET /strm` 启动 `.strm` 生成任务；`GET /stopstrm` 停止。

同步行为要点：
- 本地上传或清理后，程序会对云端进行一致性校验；若云端出现新文件或需下载的资源，程序会自动从云端下载并在本地生成 `.strm`（若为视频）。
- 对于本地的 `.strm` 文件，程序会读取其中的 pickcode 来关联云端真实文件，必要时执行移动/改名操作以保持云端目录整洁。

## 注意事项
115同步目录的操作除了把刮削好的文件丢到115，千万不能直接删除115文件，本程序完全没处理这种情况，需要删除直接本地删除然后同步到115。
## 部署建议

使用 Docker/Compose 部署，确保 `./data`、`sync_path`、`strm_path` 等目录在宿主机上已准备并映射到容器中。


## 反馈与贡献

有问题也可以在 issues 里描述你的部署环境与配置样例，便于定位问题。
交流qq群:1025795951 尽量qq群里说 我几乎不看github
---

> 免责声明：本项目用于学习和自助管理目的，请遵守 115 网盘的服务条款。

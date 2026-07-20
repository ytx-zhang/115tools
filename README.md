# 115tools

基于 115 网盘开放平台的 Emby 媒体库同步工具，支持本地文件自动上传云端、云端文件同步到本地、批量生成 `.strm` 直链文件。

## 功能

- **本地文件自动同步** — 监听本地目录变化，自动上传新增/修改的文件到 115 网盘，视频文件上传后自动转为 `.strm` 直链
- **云端文件同步** — 检测云端新增文件，下载到本地或生成 `.strm` 直链
- **STRM 批量生成** — 从指定 115 目录批量生成 `.strm` 文件，配合 Emby 实现网盘视频直链播放
- **Web 管理面板** — 实时进度展示、一键触发同步/生成任务
- **定时全量同步** — 每 12 小时自动执行本地 + 云端全量同步

## 架构

```
本地目录 ←→ 115 网盘 ←→ Emby (strm 302 直链)
```

- 视频文件上传云端后，本地保留 `.strm` 文件（内含下载直链 URL），Emby 播放时通过 115tools 302 重定向到 115 CDN 直链
- 非视频文件（字幕、nfo 等）直接下载到本地

## 目录命名规则（重要）

**Docker 挂载的目录名必须与 `config.yaml` 中的云端目录名完全一致**，因为代码根据本地文件路径直接映射云端路径。

例如配置文件写了：

```yaml
sync_path: /strm媒体库
strm_path: /待刮削
temp_path: /Temp
```

则 `docker-compose.yml` 的 volumes 必须这样写：

```yaml
volumes:
  - ./media:/strm媒体库     # ✅ 正确：与 sync_path 同名
  - ./strm:/待刮削          # ✅ 正确：与 strm_path 同名
```

如果写成 `- ./media:/media`，程序会查找云端路径 `/media`，必然匹配不上。

> 这就是"同名映射"规则：本地路径以哪个目录挂载，云端就操作哪个路径。

### 首次启动要求

**云端和本地对应目录都必须为空文件夹**。程序首次运行会扫描云端建立本地数据库索引，如果目录中已有文件会导致状态混乱。

## 数据迁移指南

已有数据的媒体库按以下方式植入：

### 从本地迁移（已有本地视频，想上传到 115 不再占用本地空间）

1. 确保云端目录为空，本地目录为空
2. 启动 115tools，完成初始化，管理面板确认正常运行
3. 将原有视频文件移动到本地挂载目录（如 `/strm媒体库/电影/xxx.mp4`）
4. 文件监听器会自动检测 → 上传到云端 → 本地替换为 `.strm` 文件
5. 非视频文件（字幕、nfo）上传后本地保留原文件

### 从云端迁移（已有云端文件）

1. 将云端文件移动到 `sync_path` 对应的目录
2. 启动 115tools，完成初始化
3. 在管理面板点击**"文件同步"**，程序检测到文件后自动下载/生成 strm

### 本地刮削整理流程

如果你的媒体文件需要先在本地通过刮削工具（tMM、nastool 等）整理元数据，推荐以下流程：

1. 将视频文件上传到 115 网盘 `strm_path` 对应目录
2. 在管理面板点击 **"STRM 生成"**，程序在本地生成 `.strm` 直链文件
3. 用刮削工具对本地 strm 目录进行识别、刮削、重命名
4. 刮削完成后，将整理好的目录**移动到** `sync_path` 对应本地目录
5. 程序的本地文件监听会自动检测 → 上传视频到对应云端目录 → 本地保留 `.strm`

### 从其他 115 strm 项目迁移

如果你之前用过其他能生成 115 strm 的项目，且 `.strm` 文件内部链接包含 pickcode（即 URL 中有 `pickcode=` 参数），可以直接迁移：

1. 云端视频文件保留在原位不动
2. 启动 115tools，完成初始化
3. 将所有旧 `.strm` 文件按目录结构**全部复制**到本地 `sync_path` 目录下
4. 程序检测到 `.strm` 文件后会自动：
   - 解析出 pickcode 找到对应的云端视频
   - 将云端视频**移动**到本地 `.strm` 对应的目录位置
   - 将云端文件名改回`.strm`文件名
   - 重写 `.strm` 文件内部链接为 115tools 的格式
5. 全程无需在 115 网盘中手动操作

> 已有大量 strm 文件的老用户，直接复制粘贴即可完成迁移。

### 目录结构

```
115tools/
├── docker-compose.yml
├── config/
│   └── config.yaml          # 配置文件
└── data/                    # 自动创建，存放数据库
```

### docker-compose.yml

```yaml
services:
  115tools:
    image: ghcr.io/ytx-zhang/115tools:latest
    container_name: 115tools
    environment:
      - TZ=Asia/Shanghai
      - LOG_LEVEL=INFO            # DEBUG / INFO / WARN / ERROR
    volumes:
      - ./config/:/app/data                           # 配置文件目录
      - /path/to/media:/strm媒体库                     # 本地同步根目录（必须与 sync_path 同名）
      - /path/to/strm:/待刮削                          # strm 生成本地目录（必须与 strm_path 同名）
    ports:
      - "8080:8080"
    restart: unless-stopped
```

> **关键**：Docker volumes 冒号右边的容器内路径，必须与 `config.yaml` 中 `sync_path`、`strm_path` 的值**字符完全一致**。程序用本地绝对路径直接映射为云端路径，名称不同则无法正常工作。

### config.yaml

```yaml
# 云端同步根目录（115 网盘中的路径）
sync_path: /strm媒体库

# STRM 生成的起始云端目录
strm_path: /待刮削

# 回收/临时目录（云端）
temp_path: /Temp

# STRM 文件中的直链地址（Emby 可访问的地址）
strm_url: http://your-server:8080

# 115 网盘 Token
token:
  access_token: ""
  refresh_token: ""
  expire_at: ""
```

**获取 Token**：首次启动前手动填写 `refresh_token`，程序启动后会自动获取 `access_token` 并周期性刷新。`access_token` 和 `expire_at` 可留空。

### 启动

```bash
docker compose up -d
```

启动后访问 `http://your-server:8080` 打开管理面板：

- 首次启动会扫描云端目录建立本地数据库索引
- 之后会自动监听 `sync_path` 对应本地目录的文件变化并同步

## Web 管理面板

| 操作 | 说明 |
|------|------|
| **文件同步** | 遍历云端目录，将新增/变化文件同步到本地 |
| **STRM 生成** | 遍历 `strm_path` 云端目录，为所有视频文件生成本地 `.strm` |
| **进度条** | 实时展示当前任务的完成进度 |
| **错误日志** | 汇总展示任务执行过程中的异常 |

启动后所有路由：

| 路由 | 说明 |
|------|------|
| `GET /` | 管理面板首页 |
| `GET /logs` | SSE 实时状态推送 |
| `GET /sync` | 触发云端同步 |
| `GET /stopsync` | 停止云端同步 |
| `GET /strm` | 触发 STRM 生成 |
| `GET /stopstrm` | 停止 STRM 生成 |
| `GET /download` | 内部 302 重定向，供 Emby 播放 |

## Emby 配置

在 Emby 媒体库中添加本地 STRM 目录（与 `sync_path`/`strm_path` 对应的本地挂载路径），Emby 会将 `.strm` 文件识别为视频，播放时请求 `strm_url/download?pickcode=xxx&fid=xxx`，115tools 302 重定向到 115 CDN 直链。

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `TZ` | `Asia/Shanghai` | 时区 |
| `LOG_LEVEL` | `INFO` | 日志级别：`DEBUG` / `INFO` / `WARN` / `ERROR` |

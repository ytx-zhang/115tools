# 115tools

基于 115 网盘的 Emby 媒体库同步工具：本地文件自动上传云端、云端文件同步到本地、批量生成 `.strm` 直链文件。

登录方式：**开放平台**（`refresh_token`），在 Web 设置页填写并保存。

## 功能

- **本地文件自动同步** — 监听本地目录变化，自动上传新增/修改的文件到 115 网盘，视频文件上传后自动转为 `.strm` 直链
- **云端文件同步** — 检测云端新增文件，下载到本地或生成 `.strm` 直链
- **STRM 批量生成** — 从指定 115 目录批量生成 `.strm` 文件，配合 Emby 实现网盘视频直链播放
- **离线下载** — 添加 http/magnet/ed2k 链接或上传种子文件，直接下载到 115 网盘指定目录
- **Web 管理面板** — 实时进度展示、一键触发同步/生成任务、日志查看
- **定时全量同步** — 定时自动执行本地 + 云端全量同步（默认 12 小时，可在设置页调整间隔或关闭）

## 使用说明

### 首次启动要求

**云端和本地对应目录都必须为空文件夹**。程序首次运行会扫描云端建立本地数据库索引，如果目录中已有文件会导致状态混乱。

### 目录命名规则（重要）

**Docker 挂载到容器内的目录名，必须与 `config.json` 里的云端目录名（`sync_path`/`strm_path`）完全一致**，因为代码根据本地文件路径直接映射云端路径。

例如 `config.json` 中 `sync_path` 为 `/strm媒体库`、`strm_path` 为 `/待刮削`，则 `docker-compose.yml` 的 volumes 必须这样写：

```yaml
volumes:
  - ./media:/strm媒体库     # ✅ 正确：与 sync_path 同名
  - ./strm:/待刮削          # ✅ 正确：与 strm_path 同名
```

如果写成 `- ./media:/media`，程序会查找云端路径 `/media`，必然匹配不上。

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

> **关键**：Docker volumes 冒号右边的容器内路径，必须与 `config.json` 中 `sync_path`、`strm_path` 的值**字符完全一致**。

### 配置文件

本地配置文件 `<数据目录>/config.json` 由程序自动创建与维护，**所有配置项均通过 Web 管理面板填写**，无需手动编辑。

**获取 Token（开放平台）**：只需在面板填写 `refresh_token` 一项，`access_token` 和 `expire_at` 留空即可——程序启动后会自动获取并周期性刷新、回写。

### 启动

```bash
docker compose up -d
```

启动后访问 `http://your-server:8080` 打开管理面板：

- Web 面板**先于同步启动**：即使配置不完整也能打开面板进行设置
- 配置完整（三个路径、`strm_url`、`refresh_token`）时同步器自动拉起；缺失项会在面板顶部高亮提示，补齐保存后无需重启即自动开始同步
- 首次启动会扫描云端目录建立本地数据库索引

### Web 管理面板

| 操作 | 说明 |
|------|------|
| **登录验证** | 配置 `auth` 后需账号密码登录；`/download` 始终免验证 |
| **仪表盘** | 云端同步 / STRM 生成的启停与实时进度；日志查看 |
| **离线下载** | 添加 http/magnet/ed2k 链接任务、上传种子、查看进度/配额、删除与批量清除 |
| **设置** | 在线修改路径、STRM URL、静默窗口、定时全量同步、`refresh_token`、视频扩展名白名单、上传排除名单等；保存后实时生效 |

### 数据迁移

已有数据的媒体库按以下方式植入：

**从本地迁移（已有本地视频，想上传到 115 不再占用本地空间）**

1. 确保云端目录为空，本地目录为空
2. 启动 115tools，完成初始化，管理面板确认正常运行
3. 将原有视频文件移动到本地挂载目录（如 `/strm媒体库/电影/xxx.mp4`）
4. 文件监听器会自动检测 → 上传到云端 → 本地替换为 `.strm` 文件
5. 非视频文件（字幕、nfo）上传后本地保留原文件

**从云端迁移（已有云端文件）**

1. 将云端文件移动到 `sync_path` 对应的目录
2. 启动 115tools，完成初始化
3. 在管理面板点击**"文件同步"**，程序检测到文件后自动下载/生成 strm

**从其他 115 strm 项目迁移**

如果旧 `.strm` 文件内部链接包含 pickcode（URL 中有 `pickcode=` 参数），可直接迁移：

1. 云端视频文件保留在原位不动
2. 启动 115tools，完成初始化
3. 将所有旧 `.strm` 文件按目录结构**全部复制**到本地 `sync_path` 目录下
4. 程序会自动解析 pickcode 找到对应云端视频，移动到正确位置并重写 `.strm` 链接

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `TZ` | `Asia/Shanghai` | 时区 |
| `LOG_LEVEL` | `INFO` | 日志级别：`DEBUG` / `INFO` / `WARN` / `ERROR` |

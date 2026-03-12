<h1>115tools</h1>
<p><code>115tools</code> 是一套面向 115 网盘的辅助工具集，主要功能包括本地与云端文件同步、<code>.strm</code> 文件的生成、以及为 Emby 提供 302 直连以优化播放体验。</p>
<h3>100% AI 编写</h3>
<h2>本人没学过编程！</h2>
<h1>使用请注意：本项目由 AI 生成，功能可能不完善，风险自负</h1>
<h2>✨ 核心功能</h2>
<ul>
<li>
<p>本地目录与 115 云盘的双向一致性维护。</p>
</li>
<li>
<p>本地扫描：检测新增/变更文件并上传；对于大于 10MB 的视频，会在上传后在本地生成 <code>.strm</code> 索引并把原文件移动到云端。</p>
</li>
<li>
<p>同步文件：检查云端是否有新文件或变更，必要时自动从云端下载并在本地生成 <code>.strm</code> 索引文件。</p>
</li>
<li>
<p>自动清理：对本地已删除的文件执行云端清理（视频文件和文件夹移入 <code>temp_path</code> 指定的临时目录）。</p>
</li>
<li>
<p>STRM 生成与管理（<code>addStrm</code>）</p>
<ul>
<li>递归扫描指定云端目录，批量生成 <code>.strm</code> 文件，其他文件下载到本地。</li>
</ul>
</li>
<li>
<p>Emby 302 直链代理（<code>emby302</code>）</p>
<ul>
<li>代理 Emby 请求，不需要转码的视频 302 指向 115 的直链。</li>
<li>支持字幕重定向到fontinass，使用他的noproxy标签镜像不用套娃反代了。</li>
</ul>
</li>
</ul>
<h2>🚀 快速上手</h2>
<h3>1) 获取凭据</h3>
<p>准备好 115 的 <code>refresh_token</code>（可通过openlist的115 Open 接口文档获取），用于在容器内的 <code>data/config.yaml</code> 中配置。</p>
<h3>2) 编辑配置</h3>
<p>在 <code>data/config.yaml</code> 中填写必要字段，例如：</p>
<pre><code>token:
  refresh_token: &#34;你的_115_REFRESH_TOKEN&#34;

sync_path: &#34;/strm媒体库&#34;     # 容器内用于同步的本地目录（必须映射到宿主并与云端路径一致）
strm_path: &#34;/待刮削&#34;    # 容器内用于生成/存放 .strm 的共享目录（必须映射到宿主并与云端路径一致）
temp_path: &#34;/temp&#34;             # 云端临时/回收目录
strm_url: &#34;http://ip:8080&#34; # 本服务地址，用于 .strm 文件中的播放 URL和web页面
emby_url: &#34;http://ip:8095&#34; # 原始 Emby 服务器地址（代理目标）
fontinass_url: &#34;&#34;                  # 可选：外挂字幕代理地址
</code></pre>
<p>重要：<code>sync_path</code> 与 <code>strm_path</code> 是“宿主 ↔ 容器 ↔ 云端”三级关联的公用目录。使用 Docker 部署时，必须在卷映射中把宿主目录正确挂载到容器内的 <code>sync_path</code> 和 <code>strm_path</code>。容器内路径必须与配置中一致，否则同步与 STRM 功能无法正常工作。</p>
<p>示例（docker-compose 中的卷映射示意）：</p>
<pre><code>services:
  115tools:
    image: 115tools:latest
    container_name: 115tools
    restart: always
    volumes:
      - ./data:/app/data                    # 配置与 DB
      - /media/strm媒体库:/strm媒体库       # 对应 config.sync_path
      - /media/待刮削:/待刮削       # 对应 config.strm_path
    ports:
      - &#34;8080:8080&#34;  # 管理面板 &amp; strm URL
      - &#34;8095:8095&#34;  # emby302 代理端口
</code></pre>
<h2>🛠️ 使用说明</h2>
<ul>
<li>管理面板：访问 <code>http://&lt;host&gt;:8080</code>，界面提供手动触发 <code>sync</code> / <code>strm</code> 任务以及 SSE 实时状态（<code>/logs</code>）。</li>
<li>同步触发：调用 <code>GET /sync</code> 或 <code>POST /sync</code> 可启动同步任务；<code>GET /stopsync</code> 停止。</li>
<li>STRM 触发：调用 <code>GET /strm</code> 启动 <code>.strm</code> 生成任务；<code>GET /stopstrm</code> 停止。</li>
</ul>
<p>同步行为要点：</p>
<ul>
<li>对于本地的 <code>.strm</code> 文件，程序会读取其中的 pickcode 来关联云端真实文件，必要时执行移动/改名操作以保持云端目录整洁。</li>
</ul>
<h2>注意事项</h2>
<p>115同步目录的操作除了把刮削好的文件丢到115，千万不能直接删除115文件，本程序完全没处理这种情况，需要删除直接本地删除然后自动同步到115。</p>
<h2>部署建议</h2>
<p>使用 Docker/Compose 部署，确保 <code>./data</code>、<code>sync_path</code>、<code>strm_path</code> 等目录在宿主机上已准备并映射到容器中。</p>
<h2>反馈与贡献</h2>
<h2>有问题也可以在 issues 里描述你的部署环境与配置样例，便于定位问题。
交流qq群:1025795951 尽量qq群里说 我几乎不看github</h2>
<blockquote>
<p>免责声明：本项目用于学习和自助管理目的，请遵守 115 网盘的服务条款。</p>
</blockquote>

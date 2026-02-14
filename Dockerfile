# --- 第一阶段：构建阶段 (Build) ---
FROM golang:1.26-bookworm AS builder

# 安装时区数据，供第二阶段拷贝
RUN apt-get update && apt-get install -y --no-install-recommends tzdata

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

# 建议使用 /app 作为统一工作目录
WORKDIR /app

# 1. 缓存依赖
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 2. 复制当前目录下所有文件（包含 index.html）
COPY . .

# 3. 编译二进制文件
# 注意：这里输出到当前目录下的 server
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-w -s" -trimpath -pgo=auto -o server .

# --- 第二阶段：运行时阶段 (Run) ---
FROM gcr.io/distroless/static-debian12:latest AS runner

WORKDIR /app

# 4. 从 builder 拷贝时区数据
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
ENV TZ=Asia/Shanghai

# 5. 核心修复：确保路径一致
# 刚才报错是因为 builder 阶段的文件就在 /app 下
COPY --from=builder /app/server .
COPY --from=builder /app/index.html .

EXPOSE 8080 8095

# 使用 ENTRYPOINT 确保信号传递
ENTRYPOINT ["./server"]
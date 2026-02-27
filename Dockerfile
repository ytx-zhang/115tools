# --- 第一阶段：构建 (Build) ---
FROM golang:latest AS builder

WORKDIR /app

# 缓存依赖
COPY go.mod go.sum* ./
RUN go mod download

# 复制源码并编译
COPY . .
# 提示：如果你的项目有名为 server 的二进制文件，确保这里的 -o 名字正确
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -trimpath -o server .

# --- 第二阶段：运行时 (Run) ---
FROM alpine:latest AS runner

# 1. 关键修改点：安装 tzdata，但不要在构建时写死硬链接
# 只需要保证 tzdata 存在，剩下的交给环境变量 TZ
RUN apk add --no-cache tzdata ca-certificates

# 2. 设置默认时区（如果运行时不指定 TZ 参数，则默认使用上海）
ENV TZ=Asia/Shanghai

WORKDIR /app

# 从构建阶段拷贝编译好的二进制文件和 index.html
COPY --from=builder /app/server .
COPY --from=builder /app/index.html .

# 暴露你的后端端口
EXPOSE 8080 8095

ENTRYPOINT ["./server"]
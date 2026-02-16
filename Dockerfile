# --- 第一阶段：构建 (Build) ---
FROM golang:latest AS builder

WORKDIR /app

# 缓存依赖
COPY go.mod go.sum* ./
RUN go mod download

# 复制源码并编译
COPY . .
# CGO_ENABLED=0 保证在 Alpine 这种无 glibc 环境下完美运行
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -trimpath -o server .

# --- 第二阶段：运行时 (Run) ---
FROM alpine:latest AS runner

# 安装软件
RUN apk add --no-cache tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone
WORKDIR /app

# 从构建阶段拷贝编译好的二进制文件和 index.html
COPY --from=builder /app/server .
COPY --from=builder /app/index.html .

# 暴露你的后端端口
EXPOSE 8080 8095

ENTRYPOINT ["./server"]

# --- 第一阶段：构建 ---
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="all=-w -s" \
    -trimpath -buildvcs=false -o server ./cmd/115tools \
    && mkdir -p /out/data

# --- 第二阶段：运行时（distroless/static：自带 CA 证书、无 shell，比 alpine 更小）---
# 保持 root 运行：生产 docker-compose 以 host 卷挂载 /app/data，
# 若切 nonroot(65532) 会因写权限失败导致数据目录不可写。
FROM gcr.io/distroless/static:latest

ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /app/server /app/server
COPY --from=builder /out/data /app/data

EXPOSE 8080

ENTRYPOINT ["/app/server"]

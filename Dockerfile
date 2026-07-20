# --- 第一阶段：构建 ---
FROM golang:alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-w -s" -trimpath -o server .

# --- 第二阶段：运行时 ---
FROM alpine:latest

ENV TZ=Asia/Shanghai

RUN apk add --no-cache ca-certificates \
    && mkdir -p /app/data

WORKDIR /app

COPY --from=builder /app/server .

EXPOSE 8080

ENTRYPOINT ["./server"]
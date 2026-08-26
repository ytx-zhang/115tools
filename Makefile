# 115tools 开发质量门禁与部署（不依赖特定 IDE，纯标准工具链）

GO          ?= go
COMPOSE_DIR ?= /mnt/compose/emby
NODE_IMAGE  ?= node:20-alpine

# 版本号来源 = git tag；构建时注入二进制（internal/version.Version）。
# 未打 tag 的提交用最近 tag（带 -dirty 若有未提交改动），纯本地无 git 时回退 "dev"。
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# 静态检查工具：优先用 PATH 里的 golangci-lint；本机装在 /root/go/bin 但未进 PATH，作默认回退。
GOLANGCI    ?= $(shell command -v golangci-lint 2>/dev/null || echo /root/go/bin/golangci-lint)

.PHONY: fmt lint build check test up frontend-check

# 格式化（提交前必跑）
fmt:
	$(GO) fmt ./...
	$(GO) vet ./...
	gofmt -w .

# 静态检查（零豁免，对齐 .golangci.yml）
lint:
	$(GOLANGCI) run ./...

# 编译入口：把 git tag 注入版本（替换 internal/version.Version）
build:
	$(GO) build -ldflags "-X github.com/ytx-zhang/115tools/internal/version.Version=$(VERSION)" ./cmd/115tools

# 前端 JS 语法自检（无构建步骤，仅 node --check）。
# 本机有 node 直接跑；无 node 则用一次性 $(NODE_IMAGE) 容器挂载校验（等价 node --check）。
frontend-check:
	@if command -v node >/dev/null 2>&1; then \
		echo "使用本机 node 做语法自检"; \
		for f in $$(find internal/web/static/js -name '*.js'); do \
			node --check "$$f" || exit 1; \
		done; \
	else \
		echo "本机无 node，使用容器 $(NODE_IMAGE) 做语法自检"; \
		docker run --rm -v "$(CURDIR)/internal/web/static/js:/js:ro" $(NODE_IMAGE) \
			sh -c 'for f in $$(find /js -name "*.js"); do node --check "$$f" || exit 1; done'; \
	fi

# 提交前一键门禁：格式 + 静态 + 编译 + 前端
check: fmt lint build frontend-check

# 部署：在 compose 父目录构建（注入版本）并重启（与手工操作一致）
up:
	cd $(COMPOSE_DIR) && docker compose build --build-arg VERSION=$(VERSION) && docker compose up -d

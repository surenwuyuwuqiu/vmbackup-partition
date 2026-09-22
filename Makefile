# vmbackup-partition —— VictoriaMetrics 按月份分区筛选备份工具
#
# 常用目标：
#   make build                 本机平台编译
#   make build-linux-amd64     交叉编译 Linux amd64（交付目标平台）
#   make test                  单元测试
#   make check                 fmt + vet + test
#   make release               产出 linux amd64 二进制 + sha256 校验和
#   make e2e-synthetic         合成快照端到端验证（需先 make build）
#   make e2e-real              真实 VictoriaMetrics 端到端验证（单节点还原）
#   make e2e-cluster           真实 3 节点集群：按月备份 → 导入新集群 + retention 对照
#   make clean

SHELL := /bin/bash

# 允许覆盖：make build GO=/usr/local/go/bin/go
GO ?= go

BIN_DIR    := bin
APP        := vmbackup-partition
PKG        := ./cmd/$(APP)

VERSION    ?= v1.0.0
VM_VERSION ?= v1.150.0

# 构建信息注入：使用官方 lib/buildinfo 的 Version 变量，
# 因此 `-version` 输出既包含本工具版本，也包含所依赖的官方库版本。
BUILDINFO_PKG := github.com/VictoriaMetrics/VictoriaMetrics/lib/buildinfo
LDFLAGS := -s -w -X '$(BUILDINFO_PKG).Version=$(APP) $(VERSION) based-on VictoriaMetrics $(VM_VERSION)'

# 官方发布版二进制均为静态链接（CGO 关闭），这里保持一致。
GO_BUILD_FLAGS := -trimpath
CGO_ENABLED ?= 0

.PHONY: all
all: check build

.PHONY: build
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP) $(PKG)
	@echo "已生成 $(BIN_DIR)/$(APP)"

.PHONY: build-linux-amd64
build-linux-amd64:
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP)-linux-amd64 $(PKG)
	@echo "已生成 $(BIN_DIR)/$(APP)-linux-amd64"
	@command -v file >/dev/null && file $(BIN_DIR)/$(APP)-linux-amd64 || true

.PHONY: build-linux-arm64
build-linux-arm64:
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP)-linux-arm64 $(PKG)
	@echo "已生成 $(BIN_DIR)/$(APP)-linux-arm64"

.PHONY: release
release: build-linux-amd64
	@cd $(BIN_DIR) && shasum -a 256 $(APP)-linux-amd64 > $(APP)-linux-amd64.sha256 2>/dev/null || sha256sum $(APP)-linux-amd64 > $(APP)-linux-amd64.sha256
	@cd $(BIN_DIR) && cat $(APP)-linux-amd64.sha256
	@echo "交付物：$(BIN_DIR)/$(APP)-linux-amd64"

.PHONY: test
test:
	$(GO) test ./... -count=1

.PHONY: test-verbose
test-verbose:
	$(GO) test ./... -count=1 -v

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l cmd internal); \
	if [ -n "$$out" ]; then echo "以下文件未格式化："; echo "$$out"; exit 1; fi
	@echo "gofmt 检查通过"

.PHONY: check
check: fmt-check vet test

.PHONY: e2e-synthetic
e2e-synthetic: build
	bash scripts/e2e-synthetic.sh

.PHONY: e2e-real
e2e-real: build
	bash scripts/e2e-real.sh

.PHONY: e2e-cluster
e2e-cluster: build
	bash scripts/e2e-cluster.sh

.PHONY: clean
clean:
	rm -rf $(BIN_DIR)

.PHONY: help
help:
	@sed -n '2,15p' Makefile

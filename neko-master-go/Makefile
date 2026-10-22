# neko-master 单二进制构建
#
# 产物：一个自带 UI + API + WS 的 Go 二进制，不依赖外部 node、不需要
# 在部署目录另放前端文件。前端以 output: 'export' 静态导出，
# 产物由 //go:embed 编进二进制。
#
# 常用：
#   make build          # 构建单二进制（含前端）
#   make build-fast     # 跳过前端构建，只用已生成的产物（改 Go 代码时用）
#   make dev            # air 热重载（开发期，读磁盘产物）
#   make test           # go test ./...
#   make clean          # 清构建产物

WEB_DIR      := ../apps/web
EMBED_DIR    := internal/api/web/out
BIN          := neko-master
VERSION      ?= $(shell grep -m1 '"version"' ../package.json | sed 's/.*: *"\(.*\)".*/\1/')

# 让产物进 embed 目录（//go:embed 只能嵌包目录下的文件）
.PHONY: build-web
build-web:
	@echo "==> 构建前端静态产物"
	cd $(WEB_DIR) && npx next build --webpack
	@echo "==> 拷贝产物到 embed 目录"
	mkdir -p $(EMBED_DIR)
	rm -rf $(EMBED_DIR)/[!.]* $(EMBED_DIR)/.[!.]* 2>/dev/null || true
	cp -R $(WEB_DIR)/out/. $(EMBED_DIR)/
	@du -sh $(EMBED_DIR)

.PHONY: build
build: build-web
	@echo "==> 构建 Go 单二进制 (v$(VERSION))"
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/neko
	@ls -lh $(BIN) | awk '{print "    "$5" "$9}'
	@echo ""
	@echo "✅ 完成。直接运行 ./$(BIN) 即可（UI: http://localhost:3001）"

# 只重建 Go，复用已有前端产物（前端没改时省 1 分钟）
.PHONY: build-fast
build-fast:
	@if [ ! -f "$(EMBED_DIR)/index.html" ] && [ ! -d "$(EMBED_DIR)/zh" ]; then \
		echo "!! embed 目录没有前端产物，回退到完整构建"; \
		$(MAKE) build; exit; \
	fi
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/neko
	@ls -lh $(BIN) | awk '{print "    "$5" "$9}'

.PHONY: dev
dev:
	air

.PHONY: test
test:
	go vet ./... && go test ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: clean
clean:
	rm -f $(BIN)
	rm -rf $(EMBED_DIR)
	mkdir -p $(EMBED_DIR)
	@touch $(EMBED_DIR)/.gitkeep
	cd $(WEB_DIR) && rm -rf .next out

# 只清前端产物，保留 git 占位
.PHONY: clean-web
clean-web:
	rm -rf $(EMBED_DIR)
	mkdir -p $(EMBED_DIR)
	@touch $(EMBED_DIR)/.gitkeep

GO      ?= go
NPM     ?= npm
LDFLAGS ?= -s -w

# embed 依赖 web 产物：dist 里没有 js 就先构建前端。
# 时间戳放在 dist/ 之外，避免被 go:embed 打进二进制。
WEB_STAMP := bin/.web-built
$(WEB_STAMP): $(shell find web/src web/index.html web/package.json web/vite.config.ts -type f 2>/dev/null) | bin
	cd web && $(NPM) install && $(NPM) run build && touch ../bin/.web-built

bin:
	mkdir -p bin

.PHONY: all deps web build build-debug test test-go test-web lint install clean smoke smoke-restart

all: build

deps:
	pkg install -y golang tmux aria2 sqlite nodejs 2>/dev/null || \
	sudo apt install -y build-essential golang-go tmux aria2 nodejs npm

web:
	cd web && $(NPM) ci || cd web && $(NPM) install
	cd web && $(NPM) run build

build: $(WEB_STAMP)
	CGO_ENABLED=1 $(GO) build -tags release -ldflags="$(LDFLAGS)" -o bin/litepanel ./cmd/litepanel

# 调试构建（§12.1/D9）：logx 真实输出 + 保留符号表。
# 保留符号不是顺手：debug 构建的存在意义就是排障，pprof/delve 需要符号；
# 它从来不上生产机，12MB→17MB 的体积差无所谓。
# 注：命令里的 -tags debug 同时意味着 release 版里的 -debug 运行时段位
# 只剩“降低 logx 级别”的作用，访问日志在发布产物里根本不存在。
build-debug: $(WEB_STAMP)
	CGO_ENABLED=1 $(GO) build -tags debug -o bin/litepanel-debug ./cmd/litepanel

# android/termux 不支持 -race；其余平台（包括真正的 Linux 服务器）带 -race。
RACE := $(if $(filter android,$(shell $(GO) env GOOS)),,-race)

test: test-go test-web

test-go:
	$(GO) test $(RACE) ./...
	# 发布/调试两套构建标签都得绿：logx 的双实现与一堆 tag 专属测试
	# 只在其中一边编译，只测一边等于半个仓库没测。
	$(GO) test -tags debug $(RACE) ./...

test-web:
	cd web && $(NPM) run test

# 端到端冒烟：对着编译出来的二进制跑，验的是配置加载 → 路由装配 →
# 静态资源 → tmux 桥接这条真实启动路径。Go 测试证不了这段（它自己 new 依赖）。
# 前提：面板已在 PANEL_URL 上运行；密码在首次登录时由脚本自己改。
smoke: build
	go run tools/smoke/main.go "$${PANEL_PASS:?PANEL_PASS=一次性密码或改后的密码}"

# D5 验收：kill -9 面板之后 tmux 会话必须还活着，重启后能认回来。
smoke-restart: build
	bash tools/smoke_restart.sh

# 安装/升级的唯一入口是脚本：单元、密钥、数据目录、配置模板的逻辑都在
# 那里；Makefile 再抄一份 install 命令迟早会和脚本漂移（漏装 aria2.service
# 这类事故就是这么来的）。
install: build
	bash deploy/install.sh

clean:
	rm -rf bin internal/webdist/dist

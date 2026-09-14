GO      ?= go
NPM     ?= npm
TAGS    ?= release
LDFLAGS ?= -s -w

# embed 依赖 web 产物：dist 里没有 js 就先构建前端。
# 时间戳放在 dist/ 之外，避免被 go:embed 打进二进制。
WEB_STAMP := bin/.web-built
$(WEB_STAMP): $(shell find web/src web/index.html web/package.json web/vite.config.ts -type f 2>/dev/null) | bin
	cd web && $(NPM) install && $(NPM) run build && touch ../bin/.web-built

bin:
	mkdir -p bin

.PHONY: all deps web build build-debug test test-go test-web lint install clean

all: build

deps:
	pkg install -y golang tmux aria2 sqlite nodejs 2>/dev/null || \
	sudo apt install -y build-essential golang-go tmux aria2 nodejs npm

web:
	cd web && $(NPM) ci || cd web && $(NPM) install
	cd web && $(NPM) run build

build: $(WEB_STAMP)
	CGO_ENABLED=1 $(GO) build -tags $(TAGS) -ldflags="$(LDFLAGS)" -o bin/litepanel ./cmd/litepanel

build-debug: $(WEB_STAMP)
	CGO_ENABLED=1 $(GO) build -tags debug -o bin/litepanel-debug ./cmd/litepanel

# android/termux 不支持 -race；其余平台（包括真正的 Linux 服务器）带 -race。
RACE := $(if $(filter android,$(shell $(GO) env GOOS)),,-race)

test: test-go test-web

test-go:
	$(GO) test $(RACE) ./...

test-web:
	cd web && $(NPM) run test

lint:
	cd web && $(NPM) run lint 2>/dev/null || true

install: build
	install -Dm755 bin/litepanel /usr/local/bin/litepanel
	install -Dm644 deploy/litepanel.service /etc/systemd/system/litepanel.service

clean:
	rm -rf bin internal/webdist/dist

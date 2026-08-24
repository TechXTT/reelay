# Reelay build targets.
#
# On Windows run these from Git Bash, not PowerShell: GnuWin32 make resolves
# /bin/sh to the WSL bash on PATH, which cannot see Windows paths the same way.
# For PowerShell there is make.ps1 with the same target names.

BINARY      := reelay
PKG         := ./cmd/reelay
BIN_DIR     := bin
DIST_DIR    := dist

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLUGIN_VERSION ?= 0.1.0
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

MODULE      := github.com/TechXTT/reelay
LDFLAGS     := -s -w \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.Date=$(DATE)

# CGO stays off everywhere. It is what makes the ARM cross-compiles single-file
# static binaries, and the SQLite driver is pure Go precisely so this holds.
export CGO_ENABLED := 0

.PHONY: all build test test-all test-race cover lint vet fmt tidy run dev check web web-install cross docker bench-mem plugin plugin-10 plugin-12 plugin-test clean help

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(PKG)

test:
	go test ./...

test-all: test plugin-test

# -race requires cgo and a 64-bit host compiler. It runs in CI on Linux; on a
# Windows dev box without a 64-bit gcc it will fail to build, which is a
# toolchain gap, not a test failure.
test-race:
	CGO_ENABLED=1 go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 25

vet:
	go vet ./...

lint: vet
	@command -v staticcheck >/dev/null 2>&1 || { \
		echo "staticcheck not installed: go install honnef.co/go/tools/cmd/staticcheck@latest"; exit 1; }
	staticcheck ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

# Validate config and schema without binding a port.
check: build
	$(BIN_DIR)/$(BINARY) --config config.yaml --check

run: build
	$(BIN_DIR)/$(BINARY) --config config.yaml

dev: build
	$(BIN_DIR)/$(BINARY) --config config.yaml --dev

web-install:
	cd web && npm ci

web:
	cd web && npm run build

# Cross-compile matrix. linux/arm GOARM=7 is the Synology DS214se (Marvell
# Armada 370, 32-bit ARMv7); arm64 covers newer NAS and Pi hardware.
cross:
	@mkdir -p $(DIST_DIR)
	GOOS=linux   GOARCH=amd64            go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-amd64   $(PKG)
	GOOS=linux   GOARCH=arm64            go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-arm64   $(PKG)
	GOOS=linux   GOARCH=arm   GOARM=7    go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-armv7   $(PKG)
	GOOS=windows GOARCH=amd64            go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-windows-amd64.exe $(PKG)
	@ls -la $(DIST_DIR)

docker:
	docker build -t reelay:$(VERSION) -t reelay:latest .

plugin: plugin-10 plugin-12

plugin-10:
	@mkdir -p $(DIST_DIR)/plugin-10.11
	dotnet publish plugin/Jellyfin.Plugin.Reelay/Jellyfin.Plugin.Reelay.csproj -c Release -p:JellyfinLine=10.11 -p:Version=$(PLUGIN_VERSION) -o $(DIST_DIR)/plugin-10.11
	cd $(DIST_DIR)/plugin-10.11 && zip -q ../reelay-jellyfin-10.11-$(VERSION).zip Jellyfin.Plugin.Reelay.dll

plugin-12:
	@mkdir -p $(DIST_DIR)/plugin-12
	dotnet publish plugin/Jellyfin.Plugin.Reelay/Jellyfin.Plugin.Reelay.csproj -c Release -p:JellyfinLine=12 -p:Version=$(PLUGIN_VERSION) -o $(DIST_DIR)/plugin-12
	cd $(DIST_DIR)/plugin-12 && zip -q ../reelay-jellyfin-12-$(VERSION).zip Jellyfin.Plugin.Reelay.dll

plugin-test:
	dotnet test plugin/Jellyfin.Plugin.Reelay.Tests/Jellyfin.Plugin.Reelay.Tests.csproj -c Release -p:JellyfinLine=10.11
	dotnet test plugin/Jellyfin.Plugin.Reelay.Tests/Jellyfin.Plugin.Reelay.Tests.csproj -c Release -p:JellyfinLine=12

bench-mem:
	@command -v /usr/bin/time >/dev/null 2>&1 || { echo "/usr/bin/time is required"; exit 1; }
	@mkdir -p $(BIN_DIR)
	@go test -c -o $(BIN_DIR)/reelay-engine-membench.test ./internal/engine
	@/usr/bin/time -v $(BIN_DIR)/reelay-engine-membench.test -test.count=1 -test.run TestWantedToImportedCycle 2>&1 | \
		grep -E "Maximum resident set size|PASS|FAIL"
	@rm -f $(BIN_DIR)/reelay-engine-membench.test

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out

help:
	@grep -E '^[a-z-]+:' Makefile | cut -d: -f1 | sort | tr '\n' ' '; echo

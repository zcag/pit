BINARY      := pit
-include local.mk
HOST        ?= pit-host
REMOTE_USER ?= pit
REMOTE_DIR  ?= pit
SERVICE     := pit
INSTALL_DIR := $(HOME)/.local/bin

_TAG   := $(shell git describe --tags --abbrev=0 2>/dev/null)
_REV   := $(shell git rev-list $(_TAG)..HEAD --count 2>/dev/null || git rev-list HEAD --count 2>/dev/null || echo 0)
_HASH  := $(shell git rev-parse --short HEAD 2>/dev/null || echo pre)
_DIRTY := $(shell git diff --quiet && git diff --cached --quiet || echo +dirty)
VERSION := $(if $(_TAG),$(_TAG)+$(_REV)-$(_HASH)$(_DIRTY),v0+$(_REV)-$(_HASH)$(_DIRTY))
LDFLAGS := -ldflags "-s -w -X main.Version=$(VERSION)"

.PHONY: build test run build-linux deploy install-service logs status restart

build:
	go build $(LDFLAGS) -o bin/$(BINARY) .
	@mkdir -p $(INSTALL_DIR)
	@cp bin/$(BINARY) $(INSTALL_DIR)/$(BINARY).new && mv -f $(INSTALL_DIR)/$(BINARY).new $(INSTALL_DIR)/$(BINARY)
	@echo "installed $(INSTALL_DIR)/$(BINARY)  [$(VERSION)]"

test:
	go test ./...
	node --test test/

run: build
	PIT_API_KEY=$${PIT_API_KEY:-dev} ./bin/$(BINARY) --addr 127.0.0.1:8802 --db /tmp/pit-dev.db --seed

# CGO_ENABLED=0 because the SQLite driver is pure Go — a darwin/arm64 host
# cross-compiles a linux/amd64 binary with no toolchain juggling.
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY)-linux .

# Atomic install: a running pit holds its binary open, so a plain cp fails with
# ETXTBSY. Copy beside it, then mv over.
deploy: test build-linux build
	ssh $(REMOTE_USER)@$(HOST) "mkdir -p $(REMOTE_DIR)"
	rsync -av bin/$(BINARY)-linux $(REMOTE_USER)@$(HOST):$(REMOTE_DIR)/$(BINARY).new
	ssh $(REMOTE_USER)@$(HOST) "mv -f $(REMOTE_DIR)/$(BINARY).new $(REMOTE_DIR)/$(BINARY) && chmod +x $(REMOTE_DIR)/$(BINARY) && systemctl --user restart $(SERVICE)"
	@echo "deployed to $(HOST)  [$(VERSION)]"

install-service:
	@test -f deploy/pit.service || (echo "cp deploy/pit.service.template deploy/pit.service and fill in the secrets" && exit 1)
	ssh $(REMOTE_USER)@$(HOST) "mkdir -p $(REMOTE_DIR) ~/.config/systemd/user"
	rsync -av deploy/pit.service $(REMOTE_USER)@$(HOST):~/.config/systemd/user/$(SERVICE).service
	ssh $(REMOTE_USER)@$(HOST) "systemctl --user daemon-reload && systemctl --user enable --now $(SERVICE)"

logs:
	ssh $(REMOTE_USER)@$(HOST) "journalctl --user -u $(SERVICE) -n 100 -f"

status:
	ssh $(REMOTE_USER)@$(HOST) "systemctl --user status $(SERVICE) --no-pager"

restart:
	ssh $(REMOTE_USER)@$(HOST) "systemctl --user restart $(SERVICE)"

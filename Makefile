SHELL := /bin/bash
.PHONY: help build fetch install uninstall lint spec spec-check

# Stamped into the binary and reported by `wtd -version` and /api/v1/meta, so a client can
# tell what it is talking to. Falls back to "dev" outside a git checkout.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REPO    := heysamtexas/ttyd-ify

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

# Run these WITHOUT sudo. The recipes call sudo themselves; prefixing another one nests
# them, which clobbers SUDO_USER to root and makes install.sh pick root as the service
# user. Variables must be forwarded explicitly via `env` because sudo resets the
# environment — `make install FORCE=1` alone would silently skip the binaries.
build: ## Build the wtd binary (needs Go; run WITHOUT sudo)
	GOTOOLCHAIN=local go build -trimpath -ldflags "-X main.version=$(VERSION)" -o wtd ./cmd/wtd
	@./wtd -version | sed 's/^/    built wtd /'

fetch: ## Download a released wtd binary for this machine (no Go needed) and verify its checksum
	@set -euo pipefail; \
	case "$$(uname -m)" in \
	  x86_64)          arch=amd64 ;; \
	  aarch64|arm64)   arch=arm64 ;; \
	  *) echo "no release build for $$(uname -m) — build from source instead (needs Go): make build" >&2; exit 1 ;; \
	esac; \
	echo "==> resolving the latest release"; \
	url=$$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$(REPO)/releases/latest"); \
	tag=$${url##*/}; \
	case "$$tag" in v*) ;; *) echo "no published release yet for $(REPO)" >&2; exit 1 ;; esac; \
	echo "    $$tag ($$arch)"; \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	base="https://github.com/$(REPO)/releases/download/$$tag"; \
	curl -fsSL -o "$$tmp/wtd" "$$base/wtd-linux-$$arch"; \
	curl -fsSL -o "$$tmp/SHA256SUMS" "$$base/SHA256SUMS"; \
	echo "==> verifying checksum"; \
	want=$$(awk -v f="wtd-linux-$$arch" '$$2 == f || $$2 == "*"f {print $$1}' "$$tmp/SHA256SUMS"); \
	[ -n "$$want" ] || { echo "wtd-linux-$$arch is not listed in SHA256SUMS" >&2; exit 1; }; \
	got=$$(sha256sum "$$tmp/wtd" | cut -d' ' -f1); \
	[ "$$want" = "$$got" ] || { echo "CHECKSUM MISMATCH — refusing it. want $$want got $$got" >&2; exit 1; }; \
	install -m 0755 "$$tmp/wtd" ./wtd; \
	echo "    verified, wrote ./wtd ($$(./wtd -version))"; \
	echo "    now run: make install"

install: ## Install ttyd-ify (no sudo prefix; FORCE=1 overwrites binaries, WT_USER=<u> sets the service user)
	@# Build first when Go is available, and deliberately BEFORE sudo: building as root
	@# writes root-owned files into this checkout and into the Go build cache. A box with
	@# no Go installs the shell parts and tells you where to get a release binary.
	@if command -v go >/dev/null 2>&1; then $(MAKE) --no-print-directory build; fi
	sudo env $(if $(FORCE),FORCE=$(FORCE),) $(if $(WT_USER),WT_USER=$(WT_USER),) ./install.sh

uninstall: ## Remove ttyd-ify (keeps /etc/ttyd-ify; `make uninstall PURGE=1` to remove it)
	sudo ./uninstall.sh $(if $(PURGE),--purge,)

spec: ## Regenerate cmd/wtd/openapi.json from api/openapi.yaml (the source of truth)
	@python3 -c "import yaml,json; json.dump(yaml.safe_load(open('api/openapi.yaml')), open('cmd/wtd/openapi.json','w'), indent=1, sort_keys=True)"
	@echo "wrote cmd/wtd/openapi.json"

spec-check: ## Fail if the generated spec is stale (CI guard against drift)
	@python3 -c "import yaml,json,sys; \
	want=json.dumps(yaml.safe_load(open('api/openapi.yaml')), indent=1, sort_keys=True); \
	got=open('cmd/wtd/openapi.json').read(); \
	sys.exit(0) if want==got else (print('cmd/wtd/openapi.json is stale — run: make spec'), sys.exit(1))"

spec-guards: ## Enforce openapi.yaml's editorial rule: pointers resolve, no citations in served prose
	@python3 test/spec-guards.py

lint: spec-check spec-guards ## shellcheck the scripts + go vet/gofmt/test
	shellcheck bin/wt bin/wt-serve bin/wt-web-serve bin/wt-bind.sh install.sh uninstall.sh docs/bashrc-snippet.sh test/stub-start-command.sh test/install-uninstall.sh
	@# GOTOOLCHAIN=local: go.mod pins go1.22 to match the distro toolchain, and without
	@# this a newer directive would try to download a toolchain that isn't available here.
	GOTOOLCHAIN=local gofmt -l cmd test | tee /dev/stderr | (! read -r)
	GOTOOLCHAIN=local go vet ./...
	@# -race, not plain: the whole point of the hub is a mutex-protected seam between the
	@# output pump and an attaching client, and the race detector is the only thing that
	@# checks it. It roughly doubles the runtime of a 15s suite, which is a fine trade.
	GOTOOLCHAIN=local go test -race ./...

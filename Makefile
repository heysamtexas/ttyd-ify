SHELL := /bin/bash
.PHONY: help build fetch install uninstall lint smoke spec spec-check

# Stamped into the binary and reported by `wtd -version` and /api/v1/meta, so a client can
# tell what it is talking to. Falls back to "dev" outside a git checkout.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REPO    := heysamtexas/ttyd-ify

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

# Run these WITHOUT sudo. The recipes call sudo themselves; prefixing another one nests
# them, which clobbers SUDO_USER to root and makes install.sh pick root as the service
# user. Variables must be forwarded explicitly via `env` because sudo resets the
# environment — `make install WT_USER=alice` alone would never reach install.sh.
build: ## Build the wtd binary (needs Go; run WITHOUT sudo)
	GOTOOLCHAIN=local go build -trimpath -ldflags "-X main.version=$(VERSION)" -o wtd ./cmd/wtd
	@./wtd -version | sed 's/^/    built wtd /'

fetch: ## Download a released wtd binary (no Go needed); TAG=v0.2.0 picks a release
	@# The recipe moved to fetch.sh in #86. Inside a target it could not be shellchecked and its
	@# refusals could not be tested, which is how the checksum gate reached production unexercised.
	@# TAG rather than VERSION, because VERSION above is the string stamped into a build.
	@TAG="$(TAG)" ./fetch.sh

install: ## Install ttyd-ify (no sudo prefix; WT_USER=<u> sets the service user)
	@# Build first when Go is available, and deliberately BEFORE sudo: building as root
	@# writes root-owned files into this checkout and into the Go build cache. A box with no
	@# Go and no ./wtd gets a refusal naming `make fetch` — since ttyd retired (#23) there is
	@# no server to fall back on, so installing the shell parts alone would help nobody.
	@if command -v go >/dev/null 2>&1; then $(MAKE) --no-print-directory build; fi
	sudo env $(if $(WT_USER),WT_USER=$(WT_USER),) ./install.sh

uninstall: ## Remove ttyd-ify (keeps /etc/ttyd-ify; `make uninstall PURGE=1` to remove it)
	sudo ./uninstall.sh $(if $(PURGE),--purge,)

spec: ## Regenerate the embedded spec + docs from api/ (the source of truth)
	@python3 -c "import yaml,json; json.dump(yaml.safe_load(open('api/openapi.yaml')), open('cmd/wtd/openapi.json','w'), indent=1, sort_keys=True)"
	@echo "wrote cmd/wtd/openapi.json"
	@# go:embed cannot reach outside its own package, so the served docs are copies. Same
	@# reason openapi.json is generated into cmd/wtd rather than embedded from api/.
	@mkdir -p cmd/wtd/docs
	@cp api/ws-protocol.md api/session-lifecycle.md api/compatibility.md cmd/wtd/docs/
	@echo "wrote cmd/wtd/docs/ (3 documents)"

spec-check: ## Fail if the embedded spec or docs are stale (CI guard against drift)
	@python3 -c "import yaml,json,sys; \
	want=json.dumps(yaml.safe_load(open('api/openapi.yaml')), indent=1, sort_keys=True); \
	got=open('cmd/wtd/openapi.json').read(); \
	sys.exit(0) if want==got else (print('cmd/wtd/openapi.json is stale — run: make spec'), sys.exit(1))"
	@for d in ws-protocol.md session-lifecycle.md compatibility.md; do \
	  cmp -s "api/$$d" "cmd/wtd/docs/$$d" || { echo "cmd/wtd/docs/$$d is stale — run: make spec"; exit 1; }; \
	done

spec-guards: ## Enforce openapi.yaml's editorial rule: pointers resolve, no citations in served prose
	@python3 test/spec-guards.py

smoke: ## Install into a throwaway systemd container and prove it serves a terminal (needs docker)
	@# Not part of `lint`, and not because it is slow: it needs docker and a privileged container,
	@# which a lint target must not require. This is the discoverable entry point for #79 — the
	@# alternative was a test whose only invocation lived in a comment and a CI job, on a project
	@# whose stated audience is an agent installing on a box it has never seen.
	@set -euo pipefail; \
	echo "==> building the systemd test image"; \
	docker build -q -f test/Dockerfile.systemd -t ttyd-ify-systemd . >/dev/null; \
	echo "==> the checkout needs a wtd binary; there is no Go in the container"; \
	$(MAKE) --no-print-directory build; \
	trap 'docker rm -f wt-smoke >/dev/null 2>&1 || true' EXIT; \
	docker rm -f wt-smoke >/dev/null 2>&1 || true; \
	docker run -d --name wt-smoke --privileged --tmpfs /run \
	  -v "$$PWD:/src:ro" ttyd-ify-systemd >/dev/null; \
	docker exec wt-smoke /src/test/smoke.sh

unit-guards: ## Fail if the unit file lost KillMode=process (session persistence depends on it)
	@# systemd's default is KillMode=control-group, which signals every process in the unit's
	@# cgroup on stop — dtach masters included, however they were reparented. Dropping this line
	@# does not fail a build, break a test, or log anything; it just quietly makes restarting the
	@# service destroy every session that service created (#21). So it gets a guard.
	@grep -qx 'KillMode=process' systemd/wt.service || { \
	  echo "systemd/wt.service is missing KillMode=process — a restart would destroy the sessions it created (#21)"; \
	  exit 1; }
	@# Same failure shape as KillMode, three lines instead of one (#92): RuntimeDirectory
	@# creates /run/wt and $$RUNTIME_DIRECTORY (without it wt-serve passes no -state-dir and
	@# persistence is silently off), Mode keeps raw terminal output in a 0700 directory, and
	@# only Preserve=restart stops systemd wiping it on the restart the saved replay exists
	@# for. Losing any of them errors nowhere; replay just stops surviving restarts.
	@for line in 'RuntimeDirectory=wt' 'RuntimeDirectoryMode=0700' 'RuntimeDirectoryPreserve=restart'; do \
	  grep -qx "$$line" systemd/wt.service || { \
	    echo "systemd/wt.service is missing $$line — saved replay would silently stop surviving restarts (#92)"; \
	    exit 1; }; \
	done
	@echo "unit-guards: wt.service keeps its sessions alive across a restart"

lint: spec-check spec-guards unit-guards ## shellcheck the scripts + go vet/gofmt/test
	shellcheck bin/wt-serve bin/wt-narrate bin/wt-bind.sh install.sh uninstall.sh fetch.sh docs/bashrc-snippet.sh test/stub-start-command.sh test/install-uninstall.sh test/smoke.sh test/fetch.sh
	@# Hermetic — test/fake-release.py serves fixtures on localhost, so this needs no network and
	@# touches nothing outside its own temp dir. That is why it belongs here while the other two
	@# shell suites do not: they install to absolute paths and need a throwaway machine.
	bash test/fetch.sh
	@# GOTOOLCHAIN=local: go.mod pins go1.22 to match the distro toolchain, and without
	@# this a newer directive would try to download a toolchain that isn't available here.
	GOTOOLCHAIN=local gofmt -l cmd test | tee /dev/stderr | (! read -r)
	GOTOOLCHAIN=local go vet ./...
	@# -race, not plain: the whole point of the hub is a mutex-protected seam between the
	@# output pump and an attaching client, and the race detector is the only thing that
	@# checks it. It roughly doubles the runtime of a 15s suite, which is a fine trade.
	GOTOOLCHAIN=local go test -race ./...

SHELL := /bin/bash
.PHONY: help build install uninstall lint spec spec-check

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

# Run these WITHOUT sudo. The recipes call sudo themselves; prefixing another one nests
# them, which clobbers SUDO_USER to root and makes install.sh pick root as the service
# user. Variables must be forwarded explicitly via `env` because sudo resets the
# environment — `make install FORCE=1` alone would silently skip the binaries.
build: ## Build the wtd binary (needs Go; run WITHOUT sudo)
	GOTOOLCHAIN=local go build -o wtd ./cmd/wtd
	@./wtd -version | sed 's/^/    built wtd /'

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

lint: spec-check ## shellcheck the scripts + go vet/gofmt/test
	shellcheck bin/wt bin/wt-serve bin/wt-web-serve bin/wt-bind.sh install.sh uninstall.sh docs/bashrc-snippet.sh test/stub-start-command.sh
	@# GOTOOLCHAIN=local: go.mod pins go1.22 to match the distro toolchain, and without
	@# this a newer directive would try to download a toolchain that isn't available here.
	GOTOOLCHAIN=local gofmt -l cmd test | tee /dev/stderr | (! read -r)
	GOTOOLCHAIN=local go vet ./...
	GOTOOLCHAIN=local go test ./...

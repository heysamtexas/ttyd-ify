SHELL := /bin/bash
.PHONY: help install uninstall lint

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

# Run these WITHOUT sudo. The recipes call sudo themselves; prefixing another one nests
# them, which clobbers SUDO_USER to root and makes install.sh pick root as the service
# user. Variables must be forwarded explicitly via `env` because sudo resets the
# environment — `make install FORCE=1` alone would silently skip the binaries.
install: ## Install ttyd-ify (no sudo prefix; FORCE=1 overwrites binaries, WT_USER=<u> sets the service user)
	sudo env $(if $(FORCE),FORCE=$(FORCE),) $(if $(WT_USER),WT_USER=$(WT_USER),) ./install.sh

uninstall: ## Remove ttyd-ify (keeps /etc/ttyd-ify; `make uninstall PURGE=1` to remove it)
	sudo ./uninstall.sh $(if $(PURGE),--purge,)

lint: ## shellcheck the scripts + go vet/gofmt/test
	shellcheck bin/wt bin/wt-serve bin/wt-web-serve bin/wt-bind.sh install.sh uninstall.sh docs/bashrc-snippet.sh test/stub-start-command.sh
	@# GOTOOLCHAIN=local: go.mod pins go1.22 to match the distro toolchain, and without
	@# this a newer directive would try to download a toolchain that isn't available here.
	GOTOOLCHAIN=local gofmt -l cmd test | tee /dev/stderr | (! read -r)
	GOTOOLCHAIN=local go vet ./...
	GOTOOLCHAIN=local go test ./...

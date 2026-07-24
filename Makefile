SHELL := /bin/bash
.PHONY: help install uninstall lint

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

install: ## Install ttyd-ify (needs root; runs install.sh)
	sudo ./install.sh

uninstall: ## Remove ttyd-ify (keeps /etc/ttyd-ify; `make uninstall PURGE=1` to remove it)
	sudo ./uninstall.sh $(if $(PURGE),--purge,)

lint: ## shellcheck the scripts
	shellcheck bin/wt bin/wt-serve install.sh uninstall.sh docs/bashrc-snippet.sh

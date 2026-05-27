SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.PHONY: help up bench results down ci clean

CLUSTER_NAME ?= kplane-kine

help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

up: ## Spin up the kind cluster, postgres+kine, etcd, both apiservers
	scripts/up.sh

bench: ## Run the side-by-side benchmark and emit results JSON
	scripts/bench.sh

results: ## Render the latest benchmark results into a markdown table
	scripts/results.sh

down: ## Tear down the kind cluster
	scripts/down.sh

ci: ## Run the full pipeline locally — same as GitHub Actions
	scripts/ci.sh

clean: down ## Alias for `down`

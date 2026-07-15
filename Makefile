# Stellar RPC full-history benchmarks — local ops.
# `make` (or `make help`) lists targets. `make convert` is the primary local flow.

.DEFAULT_GOAL := help
.PHONY: help convert test smoke serve

# Variables required by `convert`. `convert` fails early if any is empty.
CONVERT_REQUIRED := RESULTS RUN_ID RUN_NAME KIND RUN_DATE

help: ## Show this help
	@echo "Stellar RPC benchmarks — make targets:"
	@echo
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-9s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "convert usage:"
	@echo "  make convert RESULTS=<dir> RUN_ID=<slug> RUN_NAME=\"...\" \\"
	@echo "               KIND=pubnet|synthetic RUN_DATE=YYYY-MM-DD [FACTS=<json>] [GCS=<gs://...>]"

convert: ## Convert a results dir into docs/runs/<id>.json and update the manifest
	@$(foreach v,$(CONVERT_REQUIRED),$(if $(strip $($(v))),,$(error $(v) is required. \
	Usage: make convert RESULTS=<dir> RUN_ID=<slug> RUN_NAME="..." KIND=pubnet|synthetic RUN_DATE=YYYY-MM-DD [FACTS=<json>] [GCS=<gs://...>])))
	python3 converter/convert.py "$(RESULTS)" \
	  --run-id "$(RUN_ID)" \
	  --run-name "$(RUN_NAME)" \
	  --run-date "$(RUN_DATE)" \
	  --dataset-kind "$(KIND)" \
	  $(if $(strip $(FACTS)),--unit-facts "$(FACTS)",) \
	  $(if $(strip $(GCS)),--source-gcs "$(GCS)",) \
	  --out-dir docs/runs

test: ## Run the converter unit + golden tests
	python3 -m unittest discover converter/tests

smoke: ## Run the jsdom viewer smoke test (needs node; installs deps on first run)
	npm --prefix tests/smoke install --silent
	npm --prefix tests/smoke test

serve: ## Serve docs/ at http://localhost:8000 (the viewer needs http; file:// won't work)
	python3 -m http.server 8000 -d docs

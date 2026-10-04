# client2api developer targets.
#
# Everything here is a thin wrapper over plain `go` commands — nothing is hidden,
# and any target can be reproduced by reading the recipe.  The module has only two
# direct dependencies (utls, golang.org/x/net) and no code generation, so there is
# no codegen stage to hide either.
#
# Windows: run these from Git Bash / WSL, or read the recipes and paste the `go`
# command into PowerShell.  There is no .exe-suffix guessing here on purpose —
# `go build -o client2api` produces client2api on Unix and client2api.exe on
# Windows by itself.

GO      ?= go
PKG     ?= ./...
# Release binaries are stripped and carry the version string from the source,
# matching .github/workflows/go-binaries.yml exactly.  The version is injected
# with -X and re-read from the binary in CI, so keep the two in sync.
# TestMakefileReadsTheVersionFromThisFile pins this pattern: renaming the
# declaration fails the suite instead of silently producing an unversioned
# binary, because `$(shell sed ...)` failing yields an empty VERSION, not an
# error.
VERSION ?= $(shell sed -n 's/^var version = "\(.*\)"$$/\1/p' cmd/client2api/main.go)
LDFLAGS := -s -w -X main.version=$(VERSION)
OUTPUT  ?= bin

.PHONY: all
all: check build ## gate (fmt+vet+test) then build both binaries

##@ Gate

.PHONY: fmt
fmt: ## rewrite sources with gofmt
	$(GO) fmt $(PKG)

.PHONY: fmt-check
fmt-check: ## fail if any file needs gofmt
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
vet: ## run go vet
	$(GO) vet $(PKG)

.PHONY: test
test: ## offline unit tests, no network
	$(GO) test -count=1 -timeout 20m $(PKG)

.PHONY: race
race: ## unit tests under the race detector (needs CGO_ENABLED=1 and a C compiler)
	$(GO) test -race -count=1 -timeout 60m $(PKG)

.PHONY: check
check: fmt-check vet test ## the same gate CI runs, in the same order

##@ Build

.PHONY: build
build: ## build both binaries into $(OUTPUT)/
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/client2api ./cmd/client2api
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/probe ./cmd/probe

.PHONY: run
run: build ## build, then start the gateway on the configured listen address
	./$(OUTPUT)/client2api -config configs/client2api.json

.PHONY: clean
clean: ## remove build output
	rm -rf $(OUTPUT)

##@ Housekeeping

.PHONY: tidy
tidy: ## report unformatted files and stale build output without deleting anything
	@gofmt -l . || true
	@echo "--- build output ---"
	@ls -la $(OUTPUT) 2>/dev/null || echo "no $(OUTPUT)/ yet (run 'make build')"

.PHONY: help
help: ## list targets
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_%-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

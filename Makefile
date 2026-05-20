# gotail developer Makefile.
#
# `make check` reproduces the gates in .github/workflows/ci.yml locally so
# failures surface before pushing. Keep this in sync with that workflow.
#
# External linters resolve to an installed binary when present (fast) and
# otherwise fall back to `go run <tool>@latest` so a fresh clone needs no
# install step. Override any of them, e.g.  make lint GOLANGCI_LINT=golangci-lint

GO ?= go

# Build-tag sets CI exercises: the default build and the fsnotify opt-out.
TAGSETS := "" gotail_nofsnotify

GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo "$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest")
STATICCHECK   ?= $(shell command -v staticcheck 2>/dev/null || echo "$(GO) run honnef.co/go/tools/cmd/staticcheck@latest")
GOVULNCHECK   ?= $(shell command -v govulncheck 2>/dev/null || echo "$(GO) run golang.org/x/vuln/cmd/govulncheck@latest")

.DEFAULT_GOAL := check

.PHONY: check
check: fmt-check vet build test staticcheck lint tidy-check ## Full CI-equivalent gate (run before pushing)

.PHONY: fmt
fmt: ## Auto-format all Go files in place
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt-clean (mirrors the CI gofmt job)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet across every build-tag set
	@for t in $(TAGSETS); do echo "== go vet tags=[$$t] =="; $(GO) vet -tags "$$t" ./... || exit 1; done

.PHONY: build
build: ## Compile all packages
	$(GO) build ./...

.PHONY: test
test: ## go test -race across every build-tag set
	@for t in $(TAGSETS); do echo "== go test -race tags=[$$t] =="; $(GO) test -race -tags "$$t" ./... || exit 1; done

.PHONY: staticcheck
staticcheck: ## Run staticcheck
	$(STATICCHECK) ./...

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum are not tidy (mirrors the CI mod-tidy job)
	$(GO) mod tidy
	git diff --exit-code -- go.mod go.sum

# ── Targets CI runs but `check` deliberately omits ──────────────────────────
# (slow, network-dependent, or producing artifacts — run on demand.)

.PHONY: vulncheck
vulncheck: ## Run govulncheck (needs network for the vuln DB)
	$(GOVULNCHECK) ./...

.PHONY: cross
cross: ## Cross-compile the CI GOOS/GOARCH matrix
	@for pair in linux/amd64 linux/arm64 darwin/arm64 windows/amd64 freebsd/amd64; do \
		os=$${pair%/*}; arch=$${pair#*/}; echo "== build $$os/$$arch =="; \
		GOOS=$$os GOARCH=$$arch $(GO) build ./... || exit 1; \
	done

.PHONY: fuzz
fuzz: ## Short LineReader fuzz run (mirrors the CI fuzz job)
	$(GO) test -run='^$$' -fuzz=FuzzLineReader -fuzztime=30s ./watch

.PHONY: cover
cover: ## Write a coverage profile to coverage.out
	$(GO) test -race -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./...

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

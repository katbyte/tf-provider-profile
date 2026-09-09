# recipes use bash for pipefail support (ubuntu's default sh is dash)
SHELL := /bin/bash

GIT_COMMIT=$(shell git describe --always --long --dirty 2>/dev/null || echo dev)
GIT_VERSION=$(shell git describe --tags --dirty 2>/dev/null | sed 's/-\([0-9]*\)-g/+\1@g/' || echo dev)
TEST_TIMEOUT?=15m

# dev tool binaries are built into .tools/bin (gitignored) from the versions pinned in
# .tools/go.mod - the single source of truth for make and CI; dependabot keeps them updated
TOOLS_BIN=.tools/bin
ACTIONLINT=$(TOOLS_BIN)/actionlint
GOFUMPT=$(TOOLS_BIN)/gofumpt
GOLANGCI_LINT=$(TOOLS_BIN)/golangci-lint
YAMLLINT_VERSION=1.38.0
YAMLLINT=$(TOOLS_BIN)/yamllint

# golangci-lint with the azproviderlint module plugin compiled in (.tools/.custom-gcl.yml);
# lint runs use this binary, the plain go.mod one exists to bootstrap `golangci-lint custom`
GOLANGCI_LINT_MODULES=$(TOOLS_BIN)/golangci-with-modules

# one rule builds any Go tool: the import path comes from the tool directives in .tools/go.mod
$(TOOLS_BIN)/%: .tools/go.mod .tools/go.sum
	@echo "==> building $* (version pinned in .tools/go.mod)..."
	@cd .tools && go build -o bin/$* $$(go list tool | grep "/$*$$")

$(GOLANGCI_LINT_MODULES): .tools/.custom-gcl.yml $(GOLANGCI_LINT)
	@echo "==> building golangci-lint with plugins (versions pinned in .tools/.custom-gcl.yml)..."
	@cd .tools && bin/golangci-lint custom

$(YAMLLINT): makefile
	@command -v python3 >/dev/null || (echo "python3 is required to install yamllint" && exit 1)
	@echo "==> installing yamllint $(YAMLLINT_VERSION) into .tools/venv..."
	@mkdir -p $(TOOLS_BIN)
	@python3 -m venv .tools/venv && .tools/venv/bin/pip install -q yamllint==$(YAMLLINT_VERSION) && ln -sf ../venv/bin/yamllint $@

default: fmt build

all: fmt build

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-24s\033[0m%s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build
build: ## Compile tfpp with version info from git
	@echo "==> building..."
	go build -ldflags "-X github.com/katbyte/tf-provider-profile/lib/version.GitCommit=${GIT_COMMIT} -X github.com/katbyte/tf-provider-profile/lib/version.Version=${GIT_VERSION}"

install: ## Install tfpp into GOPATH/bin with version info from git
	@echo "==> installing..."
	go install -ldflags "-X github.com/katbyte/tf-provider-profile/lib/version.GitCommit=${GIT_COMMIT} -X github.com/katbyte/tf-provider-profile/lib/version.Version=${GIT_VERSION}" .

tools: $(ACTIONLINT) $(GOFUMPT) $(GOLANGCI_LINT) $(GOLANGCI_LINT_MODULES) $(YAMLLINT) ## Install all pinned dev tools into .tools/bin

##@ Formatting
fmt: $(GOFUMPT) $(GOLANGCI_LINT) ## Fix Go formatting (gofmt, gofumpt, goimports)
	@echo "==> Fixing source code with gofmt..."
	find . -name '*.go' | grep -v -e vendor -e '^./.cache' | xargs gofmt -s -w
	@echo "==> Fixing source code with gofumpt..."
	find . -name '*.go' | grep -v -e vendor -e '^./.cache' | xargs $(GOFUMPT) -w
	@echo "==> Fixing imports with golangci-lint (goimports)..."
	$(GOLANGCI_LINT) fmt -E goimports ./...

##@ Linting & Dependencies
lint: $(GOLANGCI_LINT_MODULES) ## Check source code with the golangci linters (incl. azproviderlint)
	@echo "==> Checking source code against linters..."
	$(GOLANGCI_LINT_MODULES) run ./...

lint-fix: $(GOLANGCI_LINT_MODULES) ## Fix source code with all golangci linters
	@echo "==> Checking source code against linters (applying autofixes)..."
	$(GOLANGCI_LINT_MODULES) run --fix ./...

actionlint: $(ACTIONLINT) ## Check GitHub workflows with actionlint
	@echo "==> Checking workflows with actionlint..."
	@$(ACTIONLINT)

yamllint: $(YAMLLINT) ## Check YAML files with yamllint (config in .yamllint.yml)
	@echo "==> Checking YAML files with yamllint..."
	@$(YAMLLINT) -s .

depscheck: ## Check that go.mod/go.sum and vendor/ are in sync
	@echo "==> Checking source code with go mod tidy..."
	@go mod tidy
	@git diff --exit-code -- go.mod go.sum || \
		(echo; echo "Unexpected difference in go.mod/go.sum files. Run 'go mod tidy' command or revert any go.mod/go.sum changes and commit."; exit 1)
	@echo "==> Checking source code with go mod vendor..."
	@go mod vendor
	@git diff --compact-summary --exit-code -- vendor || \
		(echo; echo "Unexpected difference in vendor/ directory. Run 'go mod vendor' command or revert any go.mod/go.sum/vendor changes and commit."; exit 1)
	@echo "==> Checking .tools/go.mod with go mod tidy..."
	@cd .tools && go mod tidy
	@git diff --exit-code -- .tools/go.mod .tools/go.sum || \
		(echo; echo "Unexpected difference in .tools/go.mod/go.sum. Run 'cd .tools && go mod tidy' and commit."; exit 1)

##@ Testing
test: build ## Run unit tests under the race detector
	go test -race ./... -timeout ${TEST_TIMEOUT}

##@ Profiling
run: build ## Profile every release since --since (see tfpp run --help); ARGS passes extra flags
	./tfpp run $(ARGS)

report: build ## Regenerate reports/<provider>/ from cached results
	./tfpp report $(ARGS)

check-all: build test lint actionlint yamllint depscheck ## Run build + test + all linters + depscheck

.PHONY: default all help fmt build lint lint-fix actionlint yamllint depscheck check-all install tools test run report

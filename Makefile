# Convenience wrappers only. Every target below delegates to the Go CLI or the
# Go toolchain: there is no release logic in this file.

BINARY  := release
PKG     := ./cmd/release
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The platforms release binaries are built for.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.DEFAULT_GOAL := help
.PHONY: help build install test cover vet fmt fmt-check verify dist clean \
        check release-patch release-minor release-major

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Build the release CLI into bin/
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

install: ## Install the release CLI into GOBIN
	go install -trimpath -ldflags '$(LDFLAGS)' $(PKG)

test: ## Run the tests with the race detector
	go test -race ./...

cover: ## Run the tests and open a coverage report
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

vet: ## Run go vet
	go vet ./...

fmt: ## Format the code
	gofmt -w .

fmt-check: ## Fail if any file is not gofmt-clean
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then \
		echo "these files are not gofmt-clean:"; echo "$$files"; exit 1; \
	fi

verify: fmt-check vet test ## Run every check that CI runs

dist: ## Cross-compile the release binaries and checksums into dist/
	@rm -rf dist && mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out="dist/$(BINARY)_$(VERSION)_$${os}_$${arch}"; \
		if [ "$$os" = "windows" ]; then out="$$out.exe"; fi; \
		echo "  $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -trimpath -ldflags '$(LDFLAGS)' -o "$$out" $(PKG) || exit 1; \
	done
	@cd dist && { \
		if command -v sha256sum >/dev/null 2>&1; then sha256sum $(BINARY)_*; \
		else shasum -a 256 $(BINARY)_*; fi; \
	} > dist_checksums && mv dist_checksums checksums.txt
	@echo "  dist/checksums.txt"

clean: ## Remove build output
	rm -rf bin dist coverage.out

# --- Releasing -------------------------------------------------------------
# These delegate to the CLI, which is the single source of truth. Run them on
# the default branch with a clean working tree.

check: ## Run the release preflight validations
	go run $(PKG) check

release-patch: ## Tag and push the next patch release
	go run $(PKG) patch

release-minor: ## Tag and push the next minor release
	go run $(PKG) minor

release-major: ## Tag and push the next major release
	go run $(PKG) major

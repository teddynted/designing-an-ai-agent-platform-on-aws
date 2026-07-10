# A convenience wrapper around ./cmd/release. No release logic lives here — the
# Makefile exists so that `make release-minor` is shorter to type and harder to
# get wrong than the go run invocation it expands to.

GO      ?= go
RELEASE := $(GO) run ./cmd/release

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

## -- development --------------------------------------------------------

.PHONY: build
build: ## Compile the release CLI to ./bin/release
	$(GO) build -o bin/release ./cmd/release

.PHONY: test
test: ## Run the unit tests
	$(GO) test ./...

.PHONY: cover
cover: ## Run the tests and report coverage per package
	$(GO) test -cover ./...

.PHONY: race
race: ## Run the tests under the race detector
	$(GO) test -race ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format the source
	$(GO) fmt ./...

.PHONY: check
check: fmt vet test ## Format, vet, and test

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

## -- releasing ----------------------------------------------------------

.PHONY: version
version: ## Print the current version
	@$(RELEASE) current

.PHONY: notes
notes: ## Print the notes for the next release without writing anything
	@$(RELEASE) notes

.PHONY: release-patch
release-patch: check ## Cut a patch release (a backwards-compatible fix)
	$(RELEASE) patch

.PHONY: release-minor
release-minor: check ## Cut a minor release (a backwards-compatible feature)
	$(RELEASE) minor

.PHONY: release-major
release-major: check ## Cut a major release (a breaking change)
	$(RELEASE) major

.PHONY: release-dry-run
release-dry-run: ## Show what a minor release would do, writing nothing
	$(RELEASE) minor --dry-run

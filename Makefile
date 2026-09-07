GO ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: all build test lint vet fmt fmt-check

all: fmt-check vet lint build test

build:
	$(GO) build ./...

test:
	$(GO) test ./... -race -cover

lint:
	$(GOLANGCI_LINT) run

vet:
	$(GO) vet ./...

fmt:
	gofmt -s -w .

fmt-check:
	@files=$$(gofmt -s -l .); \
	if [ -n "$$files" ]; then \
		echo "not gofmt'd:"; echo "$$files"; \
		exit 1; \
	fi

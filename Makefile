GO ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: all build test lint vet fmt fmt-check

all: fmt-check vet lint build test

build:
	$(GO) build ./...

test:
	$(GO) test ./... -race -cover

lint:
	@version=$$($(GOLANGCI_LINT) version --json 2>/dev/null | grep -o '"Version":"[^"]*"' | cut -d'"' -f4); \
	case "$$version" in \
		2.*) ;; \
		*) echo "golangci-lint $$version found, need v2 (.golangci.yml uses the v2 schema)"; exit 1 ;; \
	esac
	$(GOLANGCI_LINT) run

vet:
	$(GO) vet ./...

fmt:
	gofmt -s -w .

fmt-check:
	@files=$$(gofmt -s -l .) || exit 1; \
	if [ -n "$$files" ]; then \
		echo "not gofmt'd:"; echo "$$files"; \
		exit 1; \
	fi

GO   ?= go
PKGS ?= ./...

.PHONY: all test test-unit test-integration fmt vet lint vuln-check tidy check-modern

all: test

## test: unit tests, plus lint and vulnerability scan
test: test-unit lint vuln-check

## test-unit: race-enabled unit tests, no caching
test-unit:
	$(GO) test -race -count=1 $(PKGS)

## test-integration: tests behind the `integration` tag — real network, real downloads
test-integration:
	$(GO) test -race -count=1 -tags=integration $(PKGS)

## fmt: format all packages
fmt:
	$(GO) fmt $(PKGS)

## vet: quick compile-and-correctness check
vet:
	$(GO) vet $(PKGS)

## lint: vet always; staticcheck when available
lint: vet
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck $(PKGS); \
	else \
		echo "lint: staticcheck not installed — skipping"; \
		echo "     go install honnef.co/go/tools/cmd/staticcheck@latest"; \
	fi

## vuln-check: govulncheck when available
vuln-check:
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck $(PKGS); \
	else \
		echo "vuln-check: govulncheck not installed — skipping"; \
		echo "           go install golang.org/x/vuln/cmd/govulncheck@latest"; \
	fi

## check-modern: report constructs `go fix` would rewrite, without writing
check-modern:
	$(GO) fix -diff $(PKGS)

## tidy: sync go.mod/go.sum
tidy:
	$(GO) mod tidy

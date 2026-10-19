SHELL := /bin/bash

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
STATICCHECK_VERSION := v0.8.1
GOVULNCHECK_VERSION := v1.8.0
# A checkout of the site repository, for refreshing the contract copies.
SITE ?= ../rahulbalajee.com

.PHONY: build install test vet fmt-check staticcheck test-race vulncheck quality sync-contract clean

build:          ## Build build/blogctl, stamped with the version
	go build -trimpath -ldflags '$(LDFLAGS)' -o build/blogctl .

install:        ## Install blogctl into GOBIN
	go install -trimpath -ldflags '$(LDFLAGS)' .

test:           ## Vet and test
	go vet ./...
	go test ./...

vet:            ## Go correctness checks
	go vet ./...

fmt-check:      ## Reject unformatted tracked Go files without changing them
	@set -euo pipefail; \
		unformatted=$$(git ls-files -z -- '*.go' | xargs -0 "$$(go env GOROOT)/bin/gofmt" -l); \
		if [[ -n "$$unformatted" ]]; then \
			printf 'Run gofmt on these files:\n%s\n' "$$unformatted"; \
			exit 1; \
		fi

staticcheck:    ## Pinned static analysis; keep tooling out of go.mod
	GOTOOLCHAIN="$$(go env GOVERSION)" go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -checks all ./...

test-race:      ## Uncached tests with the race detector
	CGO_ENABLED=1 go test -race -count=1 ./...

vulncheck:      ## Scan for known vulnerabilities; findings fail the build
	GOBIN="$(CURDIR)/build/tools" GOTOOLCHAIN="$$(go env GOVERSION)" go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	build/tools/govulncheck ./...

quality: fmt-check vet staticcheck test-race vulncheck  ## Local and CI quality gate

sync-contract:  ## Refresh the site's contract copies from $(SITE)
	cp "$(SITE)/docs/api/blog-api.openapi.json" internal/rules/testdata/blog-api.openapi.json
	for name in get create update list-one list-empty media-sign; do \
		cp "$(SITE)/internal/handler/http/testdata/golden/api/$$name.json" "internal/client/testdata/server/$$name.json"; \
	done
	git -C "$(SITE)" rev-parse --short HEAD > internal/rules/testdata/SOURCE
	go test ./internal/rules ./internal/client

clean:          ## Remove build output
	rm -rf build

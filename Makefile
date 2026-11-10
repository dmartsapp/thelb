# theLB - the one place to build and check from.
#
#   make build          the Linux binaries, in bin/
#   make dist VERSION=x a release's binaries and their SHA-256 sums, in dist/
#   make check          everything a change must pass: format, vet, tests, lint, vulnerabilities
#   make e2e            the kernel tests as root, and the built binary in a lab of network namespaces
#   make lab            a lab to try by hand, on this Linux machine: see test/lab.sh
#   make lab-down       take it away
#   make remote-check REMOTE=<ssh host>   the same checks on a Linux machine, over ssh
#   make remote-e2e   REMOTE=<ssh host>   the end-to-end test there (needs passwordless sudo)

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK   ?= govulncheck
GOLANGCI_LINT_VERSION = 2.15.0

VERSION ?= 0.1.0-dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  = -s -w -X github.com/dmartsapp/thelb/internal/version.Version=$(VERSION) -X github.com/dmartsapp/thelb/internal/version.Commit=$(COMMIT)

# A Linux machine to build and test on, over ssh, and where the tree goes there.
REMOTE     ?=
REMOTE_DIR ?= thelb
REMOTE_ENV  = export PATH=$$HOME/.local/go/bin:$$HOME/go/bin:$$PATH;

.PHONY: build dist notes check tools fmt-check vet test lint vuln e2e scale lab lab-down remote-sync remote-check remote-e2e remote-scale clean

build:
	@for arch in amd64 arm64; do \
		echo "==> bin/thelb.linux.$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/thelb.linux.$$arch ./cmd/thelb || exit 1; \
	done

# dist: a release's files - the binaries, named for the version, and their
# SHA-256 sums - in dist/ (make dist VERSION=0.1.0); the release workflow runs it.
dist:
	@[ "$(VERSION)" != "0.1.0-dev" ] || { echo "say which: make dist VERSION=<x.y.z>"; exit 1; }
	@rm -rf dist && mkdir -p dist
	@for arch in amd64 arm64; do \
		echo "==> dist/thelb-$(VERSION)-linux-$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/thelb-$(VERSION)-linux-$$arch ./cmd/thelb || exit 1; \
	done
	@cd dist && sha256sum thelb-* > SHA256SUMS && cat SHA256SUMS

# notes: the changelog's section for VERSION, for the release's page.
notes:
	@awk -v v="## v$(VERSION) " 'index($$0, v) == 1 { on = 1; next } on && /^## / { exit } on' CHANGELOG.md

check: tools fmt-check vet test lint vuln

# Fail early, with the install command, rather than half-way through.
tools:
	@command -v $(GOLANGCI_LINT) >/dev/null || { echo "missing golangci-lint: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION)"; exit 1; }
	@$(GOLANGCI_LINT) --version | grep -q "version $(GOLANGCI_LINT_VERSION) " || { echo "golangci-lint must be v$(GOLANGCI_LINT_VERSION)"; exit 1; }
	@command -v $(GOVULNCHECK) >/dev/null || { echo "missing govulncheck: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }

fmt-check:
	@echo "==> gofmt"
	@out="$$(gofmt -l .)"; [ -z "$$out" ] || { echo "not gofmt-formatted (gofmt -w):"; echo "$$out"; exit 1; }

vet:
	@echo "==> go vet (this platform, and Linux)"
	@$(GO) vet ./...
	@GOOS=linux $(GO) vet ./...

test:
	@echo "==> go test -race"
	@$(GO) test -race -count=1 -timeout 5m ./...

lint:
	@echo "==> golangci-lint"
	@$(GOLANGCI_LINT) run ./...

vuln:
	@echo "==> govulncheck"
	@$(GOVULNCHECK) ./...

e2e:
	@echo "==> the kernel tests, as root, each in a network namespace of its own"
	@sudo -n env "PATH=$$PATH:/usr/sbin:/sbin" "HOME=$$HOME" "GOCACHE=$$($(GO) env GOCACHE)" "GOMODCACHE=$$($(GO) env GOMODCACHE)" $(GO) test -count=1 -run '^TestRoot' ./...
	@echo "==> end to end, as root"
	@# Its own binary: never the bin/thelb someone may be running.
	@CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/e2e/thelb ./cmd/thelb
	@sudo -n test/e2e.sh bin/e2e/thelb

# scale: N services - each its own VIP, three backends - on three directors,
# and what that costs (N=1000 by default).
N ?= 1000
# PROFILE=1: a build with -tags profile, and the leader's CPU profile at rest.
scale:
	@CGO_ENABLED=0 $(GO) build -trimpath $(if $(PROFILE),-tags profile) -ldflags "$(LDFLAGS)" -o bin/scale/thelb ./cmd/thelb
	@sudo -n $(if $(PROFILE),PROFILE=$(CURDIR)/bin/scale/leader.cpu) test/scale.sh bin/scale/thelb $(N)
	@$(if $(PROFILE),$(GO) tool pprof -top -nodecount=35 bin/scale/thelb bin/scale/leader.cpu 2>/dev/null | head -45)

lab:
	@CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/thelb ./cmd/thelb
	@sudo test/lab.sh up bin/thelb

lab-down:
	@sudo test/lab.sh down

remote-sync:
	@[ -n "$(REMOTE)" ] || { echo "say where: make $@ REMOTE=<ssh host>"; exit 1; }
	@rsync -a --delete --exclude .git --exclude bin ./ $(REMOTE):$(REMOTE_DIR)/

remote-check: remote-sync
	@ssh $(REMOTE) '$(REMOTE_ENV) cd $(REMOTE_DIR) && make check COMMIT=$(COMMIT)'

remote-e2e: remote-sync
	@ssh $(REMOTE) '$(REMOTE_ENV) cd $(REMOTE_DIR) && make e2e COMMIT=$(COMMIT)'

remote-scale: remote-sync
	@ssh $(REMOTE) '$(REMOTE_ENV) cd $(REMOTE_DIR) && make scale N=$(N) PROFILE=$(PROFILE) COMMIT=$(COMMIT)'

clean:
	rm -rf bin dist

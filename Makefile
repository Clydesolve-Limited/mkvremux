# mkvremux (module makeMKV, package main) make targets.
# `make help` lists them.
#
# Structure follows the approved reference (backup-tool/isopress):
# gate targets mirror the documented gates (verify, lint, vulncheck,
# test), the Go bin-directory tools are resolved explicitly with
# install hints, and the distribution scripts are thin pass-through
# wrappers. This project has no e2e/perf/race build-tag test tiers,
# so `test`/`verify` stay single-tier (K.I.S.S.).

# Development tools live in the Go bin directory, deliberately not on
# PATH by default. Same resolution as the reference, so the targets
# work unchanged in a fresh shell.
GOPATH_BIN := $(shell go env GOPATH)/bin
GITHUB_URL=https://github.com/Clydesolve-Limited/mkvremux
export GITHUB_URL
# Single source of truth for VERSION (git describe; falls back to
# "dev"). Used only by `ver` and the docker image tags below. The
# binary's own --version is single-sourced from const toolVersion in
# main.go (R2-1): no ldflags -X injection here.
VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)

# docker image coordinates (kept with push/run per stakeholder
# decision; docker/ does not exist yet, see TODO.md). The registry
# host is deliberately NOT hardcoded (publish set must not name an
# internal host): ECR_REPO comes from the environment, e.g.
# `ECR_REPO=<registry>/<namespace> make push`, and the push/run
# guards fail with a clear message while it is unset.
PROJECT = makeMKV
ECR_REPO ?=
APP_IMAGE = $(ECR_REPO)/$(PROJECT):$(VERSION)
APP_IMAGE_LATEST = $(ECR_REPO)/$(PROJECT):latest

.PHONY: build test vet verify vulncheck lint publish deb appimage release push run ver help

# build produces the git-ignored release binary under bin/ (the
# root-level makeMKV file is legacy and is no longer written).
# Strip-only ldflags: the -X version/build injection was retired in
# R2-1 (single-source const toolVersion).
build:
	CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/makeMKV .

# test runs the default suite (this project has no build-tag tiers).
# The explicit -timeout gives the slowest package real headroom over
# go test's 10m per-package default.
test:
	go test ./... -timeout 10m

# vet is the plain static-check pass (also part of verify).
vet:
	go vet ./...

# verify is the fast static gate: build, vet, a formatting check, and
# the module-integrity check (reference's recipe shape).
verify: build
	go vet ./...
	@unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then \
			echo "gofmt needed on:" >&2; echo "$$unformatted" >&2; exit 1; \
		fi
	go mod verify

# vulncheck and lint run the Go bin-directory tools (staticcheck,
# golangci-lint, govulncheck); each fails with an install hint when
# the binary is missing instead of a bare "command not found".
vulncheck:
	@test -x "$(GOPATH_BIN)/govulncheck" || { \
		echo "govulncheck not found at $(GOPATH_BIN)/govulncheck; install it with:" >&2; \
		echo "  go install golang.org/x/vuln/cmd/govulncheck@latest" >&2; exit 1; }
	"$(GOPATH_BIN)/govulncheck" ./...

lint:
	@test -x "$(GOPATH_BIN)/staticcheck" || { \
		echo "staticcheck not found at $(GOPATH_BIN)/staticcheck; install it with:" >&2; \
		echo "  go install honnef.co/go/tools/cmd/staticcheck@latest" >&2; exit 1; }
	@test -x "$(GOPATH_BIN)/golangci-lint" || { \
		echo "golangci-lint not found at $(GOPATH_BIN)/golangci-lint; install it with:" >&2; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest" >&2; exit 1; }
	"$(GOPATH_BIN)/staticcheck" ./...
	"$(GOPATH_BIN)/golangci-lint" run

# publish/deb/appimage/release are pass-through wrappers to the
# distribution scripts, which land with tasks R2-4..R2-7 (the targets
# are part of the approved interface either way; the one-line guards
# fail with that note until the script exists). Arguments pass through
# PUBLISH_FLAGS / RELEASE_FLAGS, e.g.
# `make publish PUBLISH_FLAGS="--dry-run"`.
publish:
	@test -x ./scripts/publish.sh || { echo "scripts/publish.sh not yet present (R2-4)"; exit 1; }
	./scripts/publish.sh $(PUBLISH_FLAGS)

# deb builds the Debian package (dist/mkvremux_<version>_amd64.deb).
deb:
	@test -x ./scripts/package-deb.sh || { echo "scripts/package-deb.sh not yet present (R2-6)"; exit 1; }
	./scripts/package-deb.sh

# appimage stages an AppDir and packs dist/mkvremux-<version>-amd64.AppImage.
appimage:
	@test -x ./scripts/package-appimage.sh || { echo "scripts/package-appimage.sh not yet present (R2-5)"; exit 1; }
	./scripts/package-appimage.sh

# release runs the release flow (gates, artifacts, annotated vX.Y.Z
# tag, GitHub mirror) through scripts/release.sh.
release:
	@test -x ./scripts/release.sh || { echo "scripts/release.sh not yet present (R2-7)"; exit 1; }
	./scripts/release.sh $(RELEASE_FLAGS)

# NOTE: docker targets are not yet functional (docker/ does not exist; see TODO.md).
# push pushes the docker image tags to the registry (honest failing
# guard until docker/ exists and ECR_REPO is provided).
push: build
	@test -n "$(ECR_REPO)" || { echo "ECR_REPO is not set (docker targets are not yet functional; docker/ does not exist - see TODO.md)" >&2; exit 1; }
	docker push $(APP_IMAGE)
	docker push $(APP_IMAGE_LATEST)

# run runs the docker image locally (same honest guard as push).
run: build
	@test -n "$(ECR_REPO)" || { echo "ECR_REPO is not set (docker targets are not yet functional; docker/ does not exist - see TODO.md)" >&2; exit 1; }
	docker run $(APP_IMAGE)

# ver prints the resolved VERSION (git describe, "dev" fallback).
ver:
	@echo '$(VERSION)'

help:
	@echo "mkvremux make targets:"
	@echo "  build        build bin/makeMKV (CGO_ENABLED=0, strip-only ldflags)"
	@echo "  test         go test ./... -timeout 10m"
	@echo "  vet          go vet ./..."
	@echo "  verify       build + vet + gofmt check + go mod verify"
	@echo "  vulncheck    govulncheck ./... (Go bin directory)"
	@echo "  lint         staticcheck ./... + golangci-lint run (Go bin directory)"
	@echo "  publish      push snapshot to GitHub (scripts/publish.sh; PUBLISH_FLAGS=...)"
	@echo "  deb          dist/mkvremux_<version>_amd64.deb (scripts/package-deb.sh)"
	@echo "  appimage     dist/mkvremux-<version>-amd64.AppImage (scripts/package-appimage.sh)"
	@echo "  release      gates + artifacts + annotated vX.Y.Z tag (scripts/release.sh; RELEASE_FLAGS=...)"
	@echo "  push         docker push of image tags (ECR_REPO via env; docker/ absent; honest guard)"
	@echo "  run          docker run of image tag (ECR_REPO via env; docker/ absent; honest guard)"
	@echo "  ver          print the resolved VERSION (git describe)"
	@echo "  help         this list"

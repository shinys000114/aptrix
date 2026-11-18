# SPDX-License-Identifier: GPL-3.0-only

VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE          ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
APTLY_VERSION ?= 1.6.3
APTLY         := $(CURDIR)/.cache/aptly/aptly-$(APTLY_VERSION)
GORELEASER    ?= go run github.com/goreleaser/goreleaser/v2@latest

.PHONY: build install test integration aptly snapshot deb clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/aptrix ./cmd/aptrix

install:
	CGO_ENABLED=0 go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/aptrix

test:
	go vet ./...
	go test ./...

# Runs aptrix against a throwaway aptly on 127.0.0.1 (needs dpkg-deb).
integration: $(APTLY)
	APTRIX_TEST_APTLY=$(APTLY) go test -tags integration -count=1 ./integration/

aptly: $(APTLY)

# Build the pinned aptly from source (distro packages are too old).
# GOTOOLCHAIN=auto: aptly may need a newer Go than this module (setup-go sets local).
$(APTLY):
	rm -rf .cache/aptly/src
	git clone -q --depth 1 --branch v$(APTLY_VERSION) https://github.com/aptly-dev/aptly.git .cache/aptly/src
	echo $(APTLY_VERSION) > .cache/aptly/src/VERSION
	cd .cache/aptly/src && GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o $(APTLY) .
	rm -rf .cache/aptly/src

# Local release dry run: binaries, tarballs and .deb packages in dist/.
snapshot:
	$(GORELEASER) release --snapshot --clean

deb: snapshot
	@ls -1 dist/*.deb

clean:
	rm -rf bin dist

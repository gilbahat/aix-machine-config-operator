# AIX Machine Config Operator
GO       ?= go
GOFLAGS  ?= -mod=vendor
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS  := -s -w -X github.com/gilbahat/aix-machine-config-operator/pkg/version.Version=$(VERSION)
# containers/image (pulled in through MCO packages) needs cgo/gpgme unless
# built with its pure-Go OpenPGP implementation, as the MCO itself is.
TAGS     := containers_image_openpgp
IMAGE    ?= quay.io/example/aix-machine-config-operator:latest
OUT      := _output

export GOFLAGS

.PHONY: all operator agent-aix test vet verify image check-upstream-copies clean

all: operator agent-aix

operator:
	CGO_ENABLED=0 $(GO) build -tags $(TAGS) -ldflags '$(LDFLAGS)' -o $(OUT)/linux/$(shell $(GO) env GOARCH)/aix-machine-config-operator ./cmd/aix-machine-config-operator

# The node agent for AIX on Power (GOOS=aix only supports ppc64).
agent-aix:
	GOOS=aix GOARCH=ppc64 CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o $(OUT)/aix/ppc64/amco-agent ./cmd/amco-agent

test:
	$(GO) test -tags $(TAGS) ./...

vet:
	$(GO) vet -tags $(TAGS) ./...
	GOOS=aix GOARCH=ppc64 CGO_ENABLED=0 $(GO) vet ./cmd/amco-agent ./pkg/agent ./pkg/aix ./pkg/rendered ./pkg/nodestate

# The agent must stay free of MCO and containers/image dependencies so it
# cross-compiles for AIX without vendor patches.
verify: vet test
	@if GOOS=aix GOARCH=ppc64 $(GO) list -deps ./cmd/amco-agent | grep -E 'github.com/openshift/|github.com/containers/'; then \
		echo "amco-agent must not depend on the packages above" >&2; exit 1; fi
	@test -z "$$(gofmt -l cmd pkg)" || { gofmt -l cmd pkg; exit 1; }

image:
	podman build -t $(IMAGE) -f Dockerfile .

# Compare pkg/upstream copies with the pinned MCO source (needs network).
check-upstream-copies:
	hack/check-upstream-copies.sh

clean:
	rm -rf $(OUT)

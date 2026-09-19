GO ?= go

.PHONY: check fmt-check vet test cleanroom build

check: fmt-check vet test cleanroom   ## the gate; CI runs exactly this

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# beads_viewer is licensed with a rider incompatible with beadline's MIT licence:
# no bv module may enter the module graph (docs/design.md, ADR-1 §1).
cleanroom:
	@mods=$$($(GO) list -m all) || exit 1; \
	if printf '%s\n' "$$mods" | grep -iE 'beads_viewer|dicklesworthstone'; then \
		echo "cleanroom: beads_viewer in the module graph; see docs/design.md"; exit 1; \
	fi

# The version 'beadline version' prints: the latest tag and the commits
# since, or the commit; without ldflags beadline falls back to the module
# version, then the VCS revision.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	CGO_ENABLED=0 $(GO) build -ldflags "-X github.com/alexknips/beadline/internal/cli.Version=$(VERSION)" -o bin/beadline ./cmd/beadline

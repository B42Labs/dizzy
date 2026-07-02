BINARY      := openstack-tester
PKG         := ./cmd/openstack-tester
GO          ?= go
GOFLAGS     ?=
GOLANGCI    ?= golangci-lint

# --- Testbed run ------------------------------------------------------------
# `make testbed` runs a neutron scenario directly against the OSISM testbed
# cloud defined in contrib/clouds.yaml. Any run record the command leaves
# behind (run-<id>.json) is cleaned up afterwards, even when the run itself
# fails. Override any variable at invocation:
#   make testbed SCENARIO=scenarios/medium.yaml
#   make testbed TESTBED_CMD=chaos ARGS="--concurrency 16"
#   make testbed KEEP=1   # skip the cleanup, keep resources and run record
OS_CLOUD    ?= test
CLOUDS_FILE ?= contrib/clouds.yaml
OS_CACERT   ?= contrib/testbed.pem
SCENARIO    ?= scenarios/small.yaml
TESTBED_CMD ?= apply

.DEFAULT_GOAL := build

.PHONY: help build install run vet lint fmt test tidy clean testbed

## help: Show this help.
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/## //' | awk -F': ' '{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

## build: Build the openstack-tester binary into the repo root.
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(PKG)

## install: Install the binary into $GOBIN (or $GOPATH/bin).
install:
	$(GO) install $(GOFLAGS) $(PKG)

## run: Build and run the binary (pass args via ARGS=...).
run: build
	./$(BINARY) $(ARGS)

## testbed: Run the neutron small scenario against the testbed cloud, then clean up.
testbed: build
	@test -f "$(CLOUDS_FILE)" || { echo "error: clouds file $(CLOUDS_FILE) not found"; exit 1; }
	@test -f "$(OS_CACERT)"   || { echo "error: CA cert $(OS_CACERT) not found (clouds.yaml 'cacert')"; exit 1; }
	@test -f "$(SCENARIO)"    || { echo "error: scenario $(SCENARIO) not found"; exit 1; }
	@echo "Running neutron $(TESTBED_CMD) against the OSISM testbed:"
	@echo "  Cloud:    $(OS_CLOUD) ($(CLOUDS_FILE))"
	@echo "  Scenario: $(SCENARIO)"
	@echo "  CA cert:  $(OS_CACERT)"
	@before=$$(ls run-*.json 2>/dev/null); \
	OS_CLIENT_CONFIG_FILE="$(CLOUDS_FILE)" \
	./$(BINARY) neutron $(TESTBED_CMD) --os-cloud "$(OS_CLOUD)" --scenario "$(SCENARIO)" $(ARGS); \
	status=$$?; \
	if [ -n "$(KEEP)" ]; then echo "KEEP set, skipping cleanup"; exit $$status; fi; \
	for rec in run-*.json; do \
		[ -e "$$rec" ] || continue; \
		echo "$$before" | grep -Fqx "$$rec" && continue; \
		echo "Cleaning up $$rec"; \
		OS_CLIENT_CONFIG_FILE="$(CLOUDS_FILE)" \
		./$(BINARY) neutron cleanup --os-cloud "$(OS_CLOUD)" --run "$$rec" && rm -f "$$rec" || status=1; \
	done; \
	exit $$status

## vet: Run go vet across all packages.
vet:
	$(GO) vet ./...

## lint: Run golangci-lint across all packages.
lint:
	$(GOLANGCI) run ./...

## fmt: Format all Go sources.
fmt:
	$(GO) fmt ./...

## test: Run all tests.
test:
	$(GO) test ./...

## tidy: Tidy and verify go.mod / go.sum.
tidy:
	$(GO) mod tidy

## clean: Remove build and test artifacts.
clean:
	$(GO) clean
	rm -f $(BINARY)

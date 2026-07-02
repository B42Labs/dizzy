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

# --- Local OTEL smoke stack -------------------------------------------------
# `make otel-up` boots a local kind cluster running VictoriaMetrics reachable
# on http://localhost:8428 and Grafana (anonymous, provisioned) on
# http://localhost:3000; `make testbed-monitor` then runs `neutron monitor
# --otel` against the testbed, pushing OTLP metrics into it; `make otel-ui`
# opens VMUI, `make otel-grafana` opens the Grafana overview dashboard,
# `make otel-verify` checks the stored schema and Grafana health, `make
# otel-down` tears it all down. See README §9. Override the monitor cadence and
# count (MONITOR_INTERVAL=0, the default, runs iterations back-to-back; set a
# duration like 5m for a paced run):
#   make testbed-monitor MONITOR_INTERVAL=5m MONITOR_ITERATIONS=1
MONITOR_INTERVAL   ?= 0
MONITOR_ITERATIONS ?= 0

.DEFAULT_GOAL := build

.PHONY: help build install run vet lint fmt test tidy clean testbed \
	otel-up otel-down otel-verify otel-ui otel-grafana testbed-monitor

## help: Show this help.
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

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

# --- Local OTEL smoke stack -------------------------------------------------

## otel-up: Boot the kind cluster and deploy VictoriaMetrics (:8428) and Grafana (:3000).
otel-up:
	@for tool in docker kind kubectl curl; do \
		command -v $$tool >/dev/null 2>&1 || { echo "error: $$tool not found in PATH (required for the local OTEL stack)"; exit 1; }; \
	done
	@kind get clusters 2>/dev/null | grep -qx ostester-otel \
		|| kind create cluster --config contrib/otel/kind.yaml
	@docker port ostester-otel-control-plane 30300/tcp >/dev/null 2>&1 \
		|| { echo "error: the existing ostester-otel cluster predates Grafana (no host port 3000 mapping); kind cannot add it to a running cluster — run 'make otel-down && make otel-up' to recreate it"; exit 1; }
	@kubectl --context kind-ostester-otel apply -k contrib/otel
	@kubectl --context kind-ostester-otel -n ostester-otel rollout status deployment/victoria-metrics --timeout=180s
	@kubectl --context kind-ostester-otel -n ostester-otel rollout status deployment/grafana --timeout=180s
	@echo "Waiting for VictoriaMetrics on http://localhost:8428/health ..."
	@for i in $$(seq 1 30); do \
		curl -fsS http://localhost:8428/health >/dev/null 2>&1 && { echo "VictoriaMetrics is up: http://localhost:8428/vmui"; break; }; \
		if [ "$$i" -eq 30 ]; then echo "error: VictoriaMetrics did not answer on http://localhost:8428/health within 30s"; exit 1; fi; \
		sleep 1; \
	done
	@echo "Waiting for Grafana on http://localhost:3000/api/health ..."
	@for i in $$(seq 1 60); do \
		curl -fsS http://localhost:3000/api/health >/dev/null 2>&1 && { echo "Grafana is up: http://localhost:3000 ('make otel-grafana' opens the overview dashboard)"; break; }; \
		if [ "$$i" -eq 60 ]; then echo "error: Grafana did not answer on http://localhost:3000/api/health within 60s"; exit 1; fi; \
		sleep 1; \
	done

## otel-down: Delete the kind cluster and all stored metrics.
otel-down:
	@command -v kind >/dev/null 2>&1 || { echo "error: kind not found in PATH"; exit 1; }
	kind delete cluster --name ostester-otel

## otel-verify: Check the metric families in VictoriaMetrics and Grafana health.
otel-verify:
	@command -v curl >/dev/null 2>&1 || { echo "error: curl not found in PATH"; exit 1; }
	@names=$$(curl -fsS 'http://localhost:8428/api/v1/label/__name__/values') \
		|| { echo "error: cannot reach VictoriaMetrics on http://localhost:8428 (run 'make otel-up' first)"; exit 1; }; \
	rc=0; \
	for fam in \
		openstack_tester_operation_duration_seconds \
		openstack_tester_resource_time_to_ready_seconds \
		openstack_tester_iteration_duration_seconds \
		openstack_tester_iteration_operations_total \
		openstack_tester_iterations_total; do \
		if printf '%s' "$$names" | grep -q "\"$$fam"; then \
			echo "ok:      $$fam"; \
		else \
			echo "MISSING: $$fam"; rc=1; \
		fi; \
	done; \
	echo "cloud labels:    $$(curl -fsS 'http://localhost:8428/api/v1/label/cloud/values')"; \
	echo "scenario labels: $$(curl -fsS 'http://localhost:8428/api/v1/label/scenario/values')"; \
	if curl -fsS http://localhost:3000/api/health >/dev/null 2>&1; then \
		echo "ok:      grafana /api/health"; \
	else \
		echo "MISSING: grafana /api/health (run 'make otel-up' first)"; rc=1; \
	fi; \
	if curl -fsS 'http://localhost:3000/api/datasources/proxy/uid/victoriametrics/api/v1/query?query=1' 2>/dev/null | grep -q '"status":"success"'; then \
		echo "ok:      grafana -> victoriametrics datasource proxy"; \
	else \
		echo "MISSING: grafana -> victoriametrics datasource proxy"; rc=1; \
	fi; \
	exit $$rc

## otel-ui: Open VMUI with pre-filled queries showing live data.
otel-ui:
	@url='http://localhost:8428/vmui/#/?g0.expr=histogram_quantile(0.95,%20sum(rate(openstack_tester_operation_duration_seconds_bucket%7Boperation%3D%22create%22%7D%5B15m%5D))%20by%20(kind,%20le))&g1.expr=sum(rate(openstack_tester_iterations_total%5B15m%5D))%20by%20(outcome)'; \
	echo "Opening VMUI: $$url"; \
	open "$$url" 2>/dev/null || xdg-open "$$url" 2>/dev/null || echo "open the URL above in your browser"

## otel-grafana: Open the provisioned Grafana overview dashboard.
otel-grafana:
	@url='http://localhost:3000/d/ostester-overview'; \
	echo "Opening Grafana: $$url"; \
	open "$$url" 2>/dev/null || xdg-open "$$url" 2>/dev/null || echo "open the URL above in your browser"

## testbed-monitor: Run neutron monitor --otel against the testbed, exporting to VictoriaMetrics.
# monitor applies and cleans up each iteration's resources itself, so this
# target runs no post-run cleanup: unlike `testbed`, scanning run-*.json in the
# shared working directory would tear down the resources of a concurrent
# `make testbed` run that happens to write its record alongside.
testbed-monitor: build
	@test -f "$(CLOUDS_FILE)" || { echo "error: clouds file $(CLOUDS_FILE) not found"; exit 1; }
	@test -f "$(OS_CACERT)"   || { echo "error: CA cert $(OS_CACERT) not found (clouds.yaml 'cacert')"; exit 1; }
	@test -f "$(SCENARIO)"    || { echo "error: scenario $(SCENARIO) not found"; exit 1; }
	@curl -fsS -m 2 http://localhost:8428/health >/dev/null 2>&1 \
		|| echo "warning: VictoriaMetrics is not answering on localhost:8428 (run 'make otel-up' first); metrics will be exported into the void"
	@echo "Running neutron monitor --otel against the OSISM testbed:"
	@echo "  Cloud:    $(OS_CLOUD) ($(CLOUDS_FILE))"
	@echo "  Scenario: $(SCENARIO)"
	@echo "  Cadence:  interval $(MONITOR_INTERVAL) (0 = continuous), iterations $(MONITOR_ITERATIONS) (0 = forever, Ctrl-C to stop)"
	@echo "  OTLP:     http://localhost:8428/opentelemetry/v1/metrics (15s export interval)"
	@OS_CLIENT_CONFIG_FILE="$(CLOUDS_FILE)" \
	OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://localhost:8428/opentelemetry/v1/metrics" \
	OTEL_METRIC_EXPORT_INTERVAL=15000 \
	./$(BINARY) neutron monitor --os-cloud "$(OS_CLOUD)" --scenario "$(SCENARIO)" \
		--interval "$(MONITOR_INTERVAL)" --iterations "$(MONITOR_ITERATIONS)" --otel $(ARGS)

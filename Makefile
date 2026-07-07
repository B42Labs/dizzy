BINARY      := dizzy
PKG         := ./cmd/dizzy
GO          ?= go
GOFLAGS     ?=
GOLANGCI    ?= golangci-lint

# --- Testbed run ------------------------------------------------------------
# `make testbed` runs a scenario for one service namespace (SERVICE, default
# neutron) directly against the OSISM testbed cloud defined in
# contrib/clouds.yaml. Any run record the command leaves behind (run-<id>.json)
# is cleaned up afterwards, even when the run itself fails. Override any
# variable at invocation:
#   make testbed SCENARIO=scenarios/neutron/medium.yaml
#   make testbed SERVICE=cinder   # runs scenarios/cinder/small.yaml
#   make testbed TESTBED_CMD=chaos ARGS="--concurrency 16"
#   make testbed KEEP=1   # skip the cleanup, keep resources and run record
OS_CLOUD    ?= test
CLOUDS_FILE ?= contrib/clouds.yaml
OS_CACERT   ?= contrib/testbed.pem
SERVICE     ?= neutron
SCENARIO    ?= scenarios/$(SERVICE)/small.yaml
TESTBED_CMD ?= apply

# --- Local OTEL smoke stack -------------------------------------------------
# `make otel-up` boots a local kind cluster running VictoriaMetrics reachable
# on http://localhost:8428 and Grafana (anonymous, provisioned) on
# http://localhost:3000; `make testbed-monitor` then runs `$(SERVICE) monitor
# --otel` against the testbed, pushing OTLP metrics into it; `make otel-ui`
# opens VMUI, `make otel-grafana` opens the Grafana overview dashboard,
# `make otel-verify` checks the stored schema and Grafana health, `make
# otel-down` tears it all down. See README §9. Override the monitor cadence and
# count (MONITOR_INTERVAL=0, the default, runs iterations back-to-back; set a
# duration like 5m for a paced run):
#   make testbed-monitor MONITOR_INTERVAL=5m MONITOR_ITERATIONS=1
MONITOR_INTERVAL   ?= 0
MONITOR_ITERATIONS ?= 0

# --- Cobalt Core (c5c3) local dev stack -------------------------------------
# `make c5c3` runs a scenario against a local Cobalt Core control-plane dev
# stack (the c5c3 "forge" quick-start:
# https://c5c3.github.io/forge/quick-start-controlplane.html). That quick-start
# brings up Keystone + Horizon, so the default service is `keystone`. The admin
# password is a live Kubernetes Secret, so `make c5c3` first regenerates
# $(C5C3_CLOUDS_FILE) from the cluster (target `c5c3-clouds`) — a gitignored
# clouds.yaml with `verify: false` (the stack serves a self-signed cert, matching
# the quick-start's `openstack --insecure`). Override any variable at invocation:
#   make c5c3 C5C3_SERVICE=keystone C5C3_SCENARIO=scenarios/keystone/medium.yaml
#   make c5c3 C5C3_CMD=chaos ARGS="--duration 10m"
#   make c5c3 C5C3_AUTH_URL=https://keystone.127-0-0-1.nip.io/v3   # KIND_HOST_PORT=443
#   make c5c3 KEEP=1   # skip the cleanup, keep resources and run record
C5C3_OS_CLOUD     ?= c5c3
C5C3_CLOUDS_FILE  ?= contrib/c5c3-clouds.yaml
C5C3_AUTH_URL     ?= https://keystone.127-0-0-1.nip.io:8443/v3
C5C3_NAMESPACE    ?= openstack
C5C3_SECRET       ?= controlplane-keystone-admin-credentials
C5C3_KUBE_CONTEXT ?=
C5C3_KUBECTX      := $(if $(C5C3_KUBE_CONTEXT),--context $(C5C3_KUBE_CONTEXT),)
C5C3_SERVICE      ?= keystone
C5C3_SCENARIO     ?= scenarios/$(C5C3_SERVICE)/small.yaml
C5C3_CMD          ?= apply

.DEFAULT_GOAL := build

.PHONY: help build install run vet lint fmt test tidy clean testbed \
	otel-up otel-down otel-verify otel-ui otel-grafana testbed-monitor \
	c5c3 c5c3-clouds c5c3-monitor

## help: Show this help.
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## build: Build the dizzy binary into the repo root.
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(PKG)

## install: Install the binary into $GOBIN (or $GOPATH/bin).
install:
	$(GO) install $(GOFLAGS) $(PKG)

## run: Build and run the binary (pass args via ARGS=...).
run: build
	./$(BINARY) $(ARGS)

## testbed: Run the small scenario (SERVICE=neutron|cinder|keystone) against the testbed cloud, then clean up.
testbed: build
	@test -f "$(CLOUDS_FILE)" || { echo "error: clouds file $(CLOUDS_FILE) not found"; exit 1; }
	@test -f "$(OS_CACERT)"   || { echo "error: CA cert $(OS_CACERT) not found (clouds.yaml 'cacert')"; exit 1; }
	@test -f "$(SCENARIO)"    || { echo "error: scenario $(SCENARIO) not found"; exit 1; }
	@echo "Running $(SERVICE) $(TESTBED_CMD) against the OSISM testbed:"
	@echo "  Cloud:    $(OS_CLOUD) ($(CLOUDS_FILE))"
	@echo "  Scenario: $(SCENARIO)"
	@echo "  CA cert:  $(OS_CACERT)"
	@before=$$(ls run-*.json 2>/dev/null); \
	OS_CLIENT_CONFIG_FILE="$(CLOUDS_FILE)" \
	./$(BINARY) $(SERVICE) $(TESTBED_CMD) --os-cloud "$(OS_CLOUD)" --scenario "$(SCENARIO)" $(ARGS); \
	status=$$?; \
	if [ -n "$(KEEP)" ]; then echo "KEEP set, skipping cleanup"; exit $$status; fi; \
	for rec in run-*.json; do \
		[ -e "$$rec" ] || continue; \
		echo "$$before" | grep -Fqx "$$rec" && continue; \
		echo "Cleaning up $$rec"; \
		OS_CLIENT_CONFIG_FILE="$(CLOUDS_FILE)" \
		./$(BINARY) $(SERVICE) cleanup --os-cloud "$(OS_CLOUD)" --run "$$rec" && rm -f "$$rec" || status=1; \
	done; \
	exit $$status

## c5c3-clouds: (Re)generate the gitignored clouds.yaml for the c5c3 dev stack from the live admin Secret.
c5c3-clouds:
	@command -v kubectl >/dev/null 2>&1 || { echo "error: kubectl not found in PATH (required for the c5c3 dev stack)"; exit 1; }
	@kubectl $(C5C3_KUBECTX) get secret $(C5C3_SECRET) -n $(C5C3_NAMESPACE) >/dev/null 2>&1 \
		|| { echo "error: secret $(C5C3_SECRET) not found in namespace $(C5C3_NAMESPACE); is the ControlPlane ready? (kubectl wait controlplane/controlplane -n $(C5C3_NAMESPACE) --for=condition=Ready)"; exit 1; }
	@pw=$$(kubectl $(C5C3_KUBECTX) get secret $(C5C3_SECRET) -n $(C5C3_NAMESPACE) -o jsonpath='{.data.password}' | base64 -d); \
	[ -n "$$pw" ] || { echo "error: admin password in secret $(C5C3_SECRET) is empty"; exit 1; }; \
	esc=$$(printf '%s' "$$pw" | sed "s/'/''/g"); \
	{ \
		echo "---"; \
		echo "# Generated by 'make c5c3-clouds' from the c5c3 ControlPlane admin Secret."; \
		echo "# Holds the live admin password — gitignored, do not commit. Regenerated on every 'make c5c3'."; \
		echo "clouds:"; \
		echo "  $(C5C3_OS_CLOUD):"; \
		echo "    auth:"; \
		echo "      username: admin"; \
		echo "      password: '$$esc'"; \
		echo "      project_name: admin"; \
		echo "      auth_url: $(C5C3_AUTH_URL)"; \
		echo "      user_domain_name: Default"; \
		echo "      project_domain_name: Default"; \
		echo "    verify: false"; \
		echo "    identity_api_version: 3"; \
	} > "$(C5C3_CLOUDS_FILE)"
	@echo "Wrote $(C5C3_CLOUDS_FILE) (cloud '$(C5C3_OS_CLOUD)', auth_url $(C5C3_AUTH_URL), verify: false)"

## c5c3: Run a scenario (C5C3_SERVICE=keystone by default) against the local Cobalt Core dev stack, then clean up.
c5c3: build c5c3-clouds
	@test -f "$(C5C3_SCENARIO)" || { echo "error: scenario $(C5C3_SCENARIO) not found"; exit 1; }
	@echo "Running $(C5C3_SERVICE) $(C5C3_CMD) against the Cobalt Core (c5c3) dev stack:"
	@echo "  Cloud:    $(C5C3_OS_CLOUD) ($(C5C3_CLOUDS_FILE))"
	@echo "  Auth URL: $(C5C3_AUTH_URL)"
	@echo "  Scenario: $(C5C3_SCENARIO)"
	@before=$$(ls run-*.json 2>/dev/null); \
	OS_CLIENT_CONFIG_FILE="$(C5C3_CLOUDS_FILE)" \
	./$(BINARY) $(C5C3_SERVICE) $(C5C3_CMD) --os-cloud "$(C5C3_OS_CLOUD)" --scenario "$(C5C3_SCENARIO)" $(ARGS); \
	status=$$?; \
	if [ -n "$(KEEP)" ]; then echo "KEEP set, skipping cleanup"; exit $$status; fi; \
	for rec in run-*.json; do \
		[ -e "$$rec" ] || continue; \
		echo "$$before" | grep -Fqx "$$rec" && continue; \
		echo "Cleaning up $$rec"; \
		OS_CLIENT_CONFIG_FILE="$(C5C3_CLOUDS_FILE)" \
		./$(BINARY) $(C5C3_SERVICE) cleanup --os-cloud "$(C5C3_OS_CLOUD)" --run "$$rec" && rm -f "$$rec" || status=1; \
	done; \
	exit $$status

## c5c3-monitor: Run monitor --otel (C5C3_SERVICE=keystone) against the c5c3 dev stack, exporting to VictoriaMetrics.
# Like testbed-monitor, monitor manages its own per-iteration cleanup, so this
# target runs no post-run cleanup of run-*.json.
c5c3-monitor: build c5c3-clouds
	@test -f "$(C5C3_SCENARIO)" || { echo "error: scenario $(C5C3_SCENARIO) not found"; exit 1; }
	@curl -fsS -m 2 http://localhost:8428/health >/dev/null 2>&1 \
		|| echo "warning: VictoriaMetrics is not answering on localhost:8428 (run 'make otel-up' first); metrics will be exported into the void"
	@echo "Running $(C5C3_SERVICE) monitor --otel against the Cobalt Core (c5c3) dev stack:"
	@echo "  Cloud:    $(C5C3_OS_CLOUD) ($(C5C3_CLOUDS_FILE))"
	@echo "  Auth URL: $(C5C3_AUTH_URL)"
	@echo "  Scenario: $(C5C3_SCENARIO)"
	@echo "  Cadence:  interval $(MONITOR_INTERVAL) (0 = continuous), iterations $(MONITOR_ITERATIONS) (0 = forever, Ctrl-C to stop)"
	@echo "  OTLP:     http://localhost:8428/opentelemetry/v1/metrics (15s export interval)"
	@OS_CLIENT_CONFIG_FILE="$(C5C3_CLOUDS_FILE)" \
	OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://localhost:8428/opentelemetry/v1/metrics" \
	OTEL_METRIC_EXPORT_INTERVAL=15000 \
	./$(BINARY) $(C5C3_SERVICE) monitor --os-cloud "$(C5C3_OS_CLOUD)" --scenario "$(C5C3_SCENARIO)" \
		--interval "$(MONITOR_INTERVAL)" --iterations "$(MONITOR_ITERATIONS)" --otel $(ARGS)

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
	@kind get clusters 2>/dev/null | grep -qx dizzy-otel \
		|| kind create cluster --config contrib/otel/kind.yaml
	@docker port dizzy-otel-control-plane 30300/tcp >/dev/null 2>&1 \
		|| { echo "error: the existing dizzy-otel cluster predates Grafana (no host port 3000 mapping); kind cannot add it to a running cluster — run 'make otel-down && make otel-up' to recreate it"; exit 1; }
	@kubectl --context kind-dizzy-otel apply -k contrib/otel
	@kubectl --context kind-dizzy-otel -n dizzy-otel rollout status deployment/victoria-metrics --timeout=180s
	@kubectl --context kind-dizzy-otel -n dizzy-otel rollout status deployment/grafana --timeout=180s
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
	kind delete cluster --name dizzy-otel

## otel-verify: Check the metric families in VictoriaMetrics and Grafana health.
# dizzy_operation_errors_total is deliberately not a required family:
# it only exists once operations have failed, so a healthy run's steady state is
# its absence, not a missing-metric failure.
otel-verify:
	@command -v curl >/dev/null 2>&1 || { echo "error: curl not found in PATH"; exit 1; }
	@names=$$(curl -fsS 'http://localhost:8428/api/v1/label/__name__/values') \
		|| { echo "error: cannot reach VictoriaMetrics on http://localhost:8428 (run 'make otel-up' first)"; exit 1; }; \
	rc=0; \
	for fam in \
		dizzy_operation_duration_seconds \
		dizzy_resource_time_to_ready_seconds \
		dizzy_iteration_duration_seconds \
		dizzy_iteration_operations_total \
		dizzy_iterations_total; do \
		if printf '%s' "$$names" | grep -q "\"$$fam"; then \
			echo "ok:      $$fam"; \
		else \
			echo "MISSING: $$fam"; rc=1; \
		fi; \
	done; \
	echo "cloud labels:    $$(curl -fsS 'http://localhost:8428/api/v1/label/cloud/values')"; \
	echo "scenario labels: $$(curl -fsS 'http://localhost:8428/api/v1/label/scenario/values')"; \
	echo "service labels:  $$(curl -fsS 'http://localhost:8428/api/v1/label/service/values')"; \
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
	@url='http://localhost:8428/vmui/#/?g0.expr=histogram_quantile(0.95,%20sum(rate(dizzy_operation_duration_seconds_bucket%7Boperation%3D%22create%22%7D%5B15m%5D))%20by%20(kind,%20le))&g1.expr=sum(rate(dizzy_iterations_total%5B15m%5D))%20by%20(outcome)'; \
	echo "Opening VMUI: $$url"; \
	open "$$url" 2>/dev/null || xdg-open "$$url" 2>/dev/null || echo "open the URL above in your browser"

## otel-grafana: Open the provisioned Grafana overview dashboard.
otel-grafana:
	@url='http://localhost:3000/d/dizzy-overview'; \
	echo "Opening Grafana: $$url"; \
	open "$$url" 2>/dev/null || xdg-open "$$url" 2>/dev/null || echo "open the URL above in your browser"

## testbed-monitor: Run monitor --otel (SERVICE=neutron|cinder|keystone) against the testbed, exporting to VictoriaMetrics.
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
	@echo "Running $(SERVICE) monitor --otel against the OSISM testbed:"
	@echo "  Cloud:    $(OS_CLOUD) ($(CLOUDS_FILE))"
	@echo "  Scenario: $(SCENARIO)"
	@echo "  Cadence:  interval $(MONITOR_INTERVAL) (0 = continuous), iterations $(MONITOR_ITERATIONS) (0 = forever, Ctrl-C to stop)"
	@echo "  OTLP:     http://localhost:8428/opentelemetry/v1/metrics (15s export interval)"
	@OS_CLIENT_CONFIG_FILE="$(CLOUDS_FILE)" \
	OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://localhost:8428/opentelemetry/v1/metrics" \
	OTEL_METRIC_EXPORT_INTERVAL=15000 \
	./$(BINARY) $(SERVICE) monitor --os-cloud "$(OS_CLOUD)" --scenario "$(SCENARIO)" \
		--interval "$(MONITOR_INTERVAL)" --iterations "$(MONITOR_ITERATIONS)" --otel $(ARGS)

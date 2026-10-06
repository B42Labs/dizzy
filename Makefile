BINARY      := dizzy
PKG         := ./cmd/dizzy
GO          ?= go
GOFLAGS     ?=
GOLANGCI    ?= golangci-lint

# --- devstack-osism run ------------------------------------------------------
# `make devstack-osism` runs a scenario for one service namespace
# (DEVSTACK_OSISM_SERVICE, default neutron) directly against the OSISM testbed
# cloud defined in contrib/devstack-osism-clouds.yaml. Any run record the
# command leaves behind (run-<id>.json) is cleaned up afterwards, even when the
# run itself fails. Override any variable at invocation:
#   make devstack-osism DEVSTACK_OSISM_SCENARIO=scenarios/neutron/medium.yaml
#   make devstack-osism DEVSTACK_OSISM_SERVICE=cinder   # runs scenarios/cinder/small.yaml
#   make devstack-osism DEVSTACK_OSISM_CMD=chaos ARGS="--concurrency 16"
#   make devstack-osism KEEP=1   # skip the cleanup, keep resources and run record
DEVSTACK_OSISM_OS_CLOUD    ?= test
DEVSTACK_OSISM_CLOUDS_FILE ?= contrib/devstack-osism-clouds.yaml
DEVSTACK_OSISM_CACERT      ?= contrib/devstack-osism.pem
DEVSTACK_OSISM_SERVICE     ?= neutron
DEVSTACK_OSISM_SCENARIO    ?= scenarios/$(DEVSTACK_OSISM_SERVICE)/small.yaml
DEVSTACK_OSISM_CMD         ?= apply

# --- Local OTEL smoke stack -------------------------------------------------
# `make otel-up` boots a local kind cluster running VictoriaMetrics reachable
# on http://localhost:8428 and Grafana (anonymous, provisioned) on
# http://localhost:3000; `make devstack-osism-monitor` then runs
# `$(DEVSTACK_OSISM_SERVICE) monitor --otel` against devstack-osism, pushing
# OTLP metrics into it; `make otel-ui` opens VMUI, `make otel-grafana` opens the
# Grafana overview dashboard, `make otel-verify` checks the stored schema and
# Grafana health, `make otel-down` tears it all down. See
# docs/how-to/export-to-otel.md. Override the monitor cadence and count
# (MONITOR_INTERVAL=0, the default, runs iterations back-to-back; set a duration
# like 5m for a paced run):
#   make devstack-osism-monitor MONITOR_INTERVAL=5m MONITOR_ITERATIONS=1
MONITOR_INTERVAL   ?= 0
MONITOR_ITERATIONS ?= 0

# --- devstack-c5c3 local dev stack -------------------------------------------
# `make devstack-c5c3` runs a scenario against a local Cobalt Core control-plane
# dev stack (the Cobalt Core "forge" quick-start:
# https://c5c3.github.io/forge/quick-start-controlplane.html). That quick-start
# brings up Keystone + Horizon, so the default service is `keystone`. The admin
# password is a live Kubernetes Secret, so `make devstack-c5c3` first regenerates
# $(DEVSTACK_C5C3_CLOUDS_FILE) from the cluster (target `devstack-c5c3-clouds`) —
# a gitignored clouds.yaml with `verify: false` (the stack serves a self-signed
# cert, matching the quick-start's `openstack --insecure`). Override any variable
# at invocation:
#   make devstack-c5c3 DEVSTACK_C5C3_SCENARIO=scenarios/keystone/medium.yaml
#   make devstack-c5c3 DEVSTACK_C5C3_CMD=chaos ARGS="--duration 10m"
#   make devstack-c5c3 DEVSTACK_C5C3_AUTH_URL=https://keystone.127-0-0-1.nip.io/v3   # KIND_HOST_PORT=443
#   make devstack-c5c3 KEEP=1   # skip the cleanup, keep resources and run record
DEVSTACK_C5C3_OS_CLOUD     ?= devstack-c5c3
DEVSTACK_C5C3_CLOUDS_FILE  ?= contrib/devstack-c5c3-clouds.yaml
DEVSTACK_C5C3_AUTH_URL     ?= https://keystone.127-0-0-1.nip.io:8443/v3
DEVSTACK_C5C3_NAMESPACE    ?= openstack
DEVSTACK_C5C3_SECRET       ?= controlplane-keystone-admin-credentials
DEVSTACK_C5C3_KUBE_CONTEXT ?=
DEVSTACK_C5C3_KUBECTX      := $(if $(DEVSTACK_C5C3_KUBE_CONTEXT),--context $(DEVSTACK_C5C3_KUBE_CONTEXT),)
DEVSTACK_C5C3_SERVICE      ?= keystone
DEVSTACK_C5C3_SCENARIO     ?= scenarios/$(DEVSTACK_C5C3_SERVICE)/small.yaml
DEVSTACK_C5C3_CMD          ?= apply

# --- Container image ---------------------------------------------------------
# `make image` cross-builds the two Linux release binaries into bin/ and builds
# the container image for the host platform; `make image-test` checks that image
# against the contract in docs/how-to/install-and-verify.md. Override at
# invocation:
#   make image IMAGE_TAG=v9.9.9
IMAGE     ?= ghcr.io/b42labs/dizzy
IMAGE_TAG ?= dev

.DEFAULT_GOAL := build

.PHONY: help build install run vet lint fmt test tidy clean devstack-osism \
	otel-up otel-down otel-verify otel-ui otel-grafana devstack-osism-monitor \
	devstack-c5c3 devstack-c5c3-clouds devstack-c5c3-monitor image image-test

## help: Show this help.
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/## //' | awk -F': ' '{printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}'

## build: Build the dizzy binary into the repo root.
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(PKG)

## install: Install the binary into $GOBIN (or $GOPATH/bin).
install:
	$(GO) install $(GOFLAGS) $(PKG)

## run: Build and run the binary (pass args via ARGS=...).
run: build
	./$(BINARY) $(ARGS)

## devstack-osism: Run the small scenario (DEVSTACK_OSISM_SERVICE=neutron|cinder|keystone|nova|glance|mix; mix needs DEVSTACK_OSISM_CMD=chaos) against the OSISM testbed, then clean up.
devstack-osism: build
	@test -f "$(DEVSTACK_OSISM_CLOUDS_FILE)" || { echo "error: clouds file $(DEVSTACK_OSISM_CLOUDS_FILE) not found"; exit 1; }
	@test -f "$(DEVSTACK_OSISM_CACERT)"      || { echo "error: CA cert $(DEVSTACK_OSISM_CACERT) not found (clouds.yaml 'cacert')"; exit 1; }
	@test -f "$(DEVSTACK_OSISM_SCENARIO)"    || { echo "error: scenario $(DEVSTACK_OSISM_SCENARIO) not found"; exit 1; }
	@echo "Running $(DEVSTACK_OSISM_SERVICE) $(DEVSTACK_OSISM_CMD) against the OSISM testbed:"
	@echo "  Cloud:    $(DEVSTACK_OSISM_OS_CLOUD) ($(DEVSTACK_OSISM_CLOUDS_FILE))"
	@echo "  Scenario: $(DEVSTACK_OSISM_SCENARIO)"
	@echo "  CA cert:  $(DEVSTACK_OSISM_CACERT)"
	@before=$$(ls run-*.json 2>/dev/null); \
	OS_CLIENT_CONFIG_FILE="$(DEVSTACK_OSISM_CLOUDS_FILE)" \
	./$(BINARY) $(DEVSTACK_OSISM_SERVICE) $(DEVSTACK_OSISM_CMD) --os-cloud "$(DEVSTACK_OSISM_OS_CLOUD)" --scenario "$(DEVSTACK_OSISM_SCENARIO)" $(ARGS); \
	status=$$?; \
	if [ -n "$(KEEP)" ]; then echo "KEEP set, skipping cleanup"; exit $$status; fi; \
	for rec in run-*.json; do \
		[ -e "$$rec" ] || continue; \
		echo "$$before" | grep -Fqx "$$rec" && continue; \
		echo "Cleaning up $$rec"; \
		OS_CLIENT_CONFIG_FILE="$(DEVSTACK_OSISM_CLOUDS_FILE)" \
		./$(BINARY) $(DEVSTACK_OSISM_SERVICE) cleanup --os-cloud "$(DEVSTACK_OSISM_OS_CLOUD)" --run "$$rec" && rm -f "$$rec" || status=1; \
	done; \
	exit $$status

## devstack-c5c3-clouds: (Re)generate the gitignored clouds.yaml for the devstack-c5c3 stack from the live admin Secret.
devstack-c5c3-clouds:
	@command -v kubectl >/dev/null 2>&1 || { echo "error: kubectl not found in PATH (required for the devstack-c5c3 dev stack)"; exit 1; }
	@kubectl $(DEVSTACK_C5C3_KUBECTX) get secret $(DEVSTACK_C5C3_SECRET) -n $(DEVSTACK_C5C3_NAMESPACE) >/dev/null 2>&1 \
		|| { echo "error: secret $(DEVSTACK_C5C3_SECRET) not found in namespace $(DEVSTACK_C5C3_NAMESPACE); is the ControlPlane ready? (kubectl wait controlplane/controlplane -n $(DEVSTACK_C5C3_NAMESPACE) --for=condition=Ready)"; exit 1; }
	@pw=$$(kubectl $(DEVSTACK_C5C3_KUBECTX) get secret $(DEVSTACK_C5C3_SECRET) -n $(DEVSTACK_C5C3_NAMESPACE) -o jsonpath='{.data.password}' | base64 -d); \
	[ -n "$$pw" ] || { echo "error: admin password in secret $(DEVSTACK_C5C3_SECRET) is empty"; exit 1; }; \
	esc=$$(printf '%s' "$$pw" | sed "s/'/''/g"); \
	{ \
		echo "---"; \
		echo "# Generated by 'make devstack-c5c3-clouds' from the Cobalt Core ControlPlane admin Secret."; \
		echo "# Holds the live admin password — gitignored, do not commit. Regenerated on every 'make devstack-c5c3'."; \
		echo "clouds:"; \
		echo "  $(DEVSTACK_C5C3_OS_CLOUD):"; \
		echo "    auth:"; \
		echo "      username: admin"; \
		echo "      password: '$$esc'"; \
		echo "      project_name: admin"; \
		echo "      auth_url: $(DEVSTACK_C5C3_AUTH_URL)"; \
		echo "      user_domain_name: Default"; \
		echo "      project_domain_name: Default"; \
		echo "    verify: false"; \
		echo "    identity_api_version: 3"; \
	} > "$(DEVSTACK_C5C3_CLOUDS_FILE)"
	@echo "Wrote $(DEVSTACK_C5C3_CLOUDS_FILE) (cloud '$(DEVSTACK_C5C3_OS_CLOUD)', auth_url $(DEVSTACK_C5C3_AUTH_URL), verify: false)"

## devstack-c5c3: Run a scenario (DEVSTACK_C5C3_SERVICE=keystone by default) against the local Cobalt Core dev stack, then clean up.
devstack-c5c3: build devstack-c5c3-clouds
	@test -f "$(DEVSTACK_C5C3_SCENARIO)" || { echo "error: scenario $(DEVSTACK_C5C3_SCENARIO) not found"; exit 1; }
	@echo "Running $(DEVSTACK_C5C3_SERVICE) $(DEVSTACK_C5C3_CMD) against the Cobalt Core dev stack:"
	@echo "  Cloud:    $(DEVSTACK_C5C3_OS_CLOUD) ($(DEVSTACK_C5C3_CLOUDS_FILE))"
	@echo "  Auth URL: $(DEVSTACK_C5C3_AUTH_URL)"
	@echo "  Scenario: $(DEVSTACK_C5C3_SCENARIO)"
	@before=$$(ls run-*.json 2>/dev/null); \
	OS_CLIENT_CONFIG_FILE="$(DEVSTACK_C5C3_CLOUDS_FILE)" \
	./$(BINARY) $(DEVSTACK_C5C3_SERVICE) $(DEVSTACK_C5C3_CMD) --os-cloud "$(DEVSTACK_C5C3_OS_CLOUD)" --scenario "$(DEVSTACK_C5C3_SCENARIO)" $(ARGS); \
	status=$$?; \
	if [ -n "$(KEEP)" ]; then echo "KEEP set, skipping cleanup"; exit $$status; fi; \
	for rec in run-*.json; do \
		[ -e "$$rec" ] || continue; \
		echo "$$before" | grep -Fqx "$$rec" && continue; \
		echo "Cleaning up $$rec"; \
		OS_CLIENT_CONFIG_FILE="$(DEVSTACK_C5C3_CLOUDS_FILE)" \
		./$(BINARY) $(DEVSTACK_C5C3_SERVICE) cleanup --os-cloud "$(DEVSTACK_C5C3_OS_CLOUD)" --run "$$rec" && rm -f "$$rec" || status=1; \
	done; \
	exit $$status

## devstack-c5c3-monitor: Run monitor --otel (DEVSTACK_C5C3_SERVICE=keystone) against devstack-c5c3, exporting to VictoriaMetrics.
# Like devstack-osism-monitor, monitor manages its own per-iteration cleanup, so
# this target runs no post-run cleanup of run-*.json.
devstack-c5c3-monitor: build devstack-c5c3-clouds
	@test -f "$(DEVSTACK_C5C3_SCENARIO)" || { echo "error: scenario $(DEVSTACK_C5C3_SCENARIO) not found"; exit 1; }
	@curl -fsS -m 2 http://localhost:8428/health >/dev/null 2>&1 \
		|| echo "warning: VictoriaMetrics is not answering on localhost:8428 (run 'make otel-up' first); metrics will be exported into the void"
	@echo "Running $(DEVSTACK_C5C3_SERVICE) monitor --otel against the Cobalt Core dev stack:"
	@echo "  Cloud:    $(DEVSTACK_C5C3_OS_CLOUD) ($(DEVSTACK_C5C3_CLOUDS_FILE))"
	@echo "  Auth URL: $(DEVSTACK_C5C3_AUTH_URL)"
	@echo "  Scenario: $(DEVSTACK_C5C3_SCENARIO)"
	@echo "  Cadence:  interval $(MONITOR_INTERVAL) (0 = continuous), iterations $(MONITOR_ITERATIONS) (0 = forever, Ctrl-C to stop)"
	@echo "  OTLP:     http://localhost:8428/opentelemetry/v1/metrics (15s export interval)"
	@OS_CLIENT_CONFIG_FILE="$(DEVSTACK_C5C3_CLOUDS_FILE)" \
	OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://localhost:8428/opentelemetry/v1/metrics" \
	OTEL_METRIC_EXPORT_INTERVAL=15000 \
	./$(BINARY) $(DEVSTACK_C5C3_SERVICE) monitor --os-cloud "$(DEVSTACK_C5C3_OS_CLOUD)" --scenario "$(DEVSTACK_C5C3_SCENARIO)" \
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
	rm -rf bin/

# --- Container image ---------------------------------------------------------

## image: Cross-build the Linux binaries into bin/ and build the container image for the host platform (needs docker).
# The go build matches the release workflow's build step
# (.github/workflows/release.yml), so CI tests a binary built like the one the
# release ships; keep the two in sync.
image:
	@command -v docker >/dev/null 2>&1 || { echo "error: docker not found in PATH (required for the container image)"; exit 1; }
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build $(GOFLAGS) -trimpath -ldflags="-s -w -X main.version=$(IMAGE_TAG)" -o bin/dizzy-linux-$$arch $(PKG) || exit 1; \
	done
	docker buildx build --load --tag $(IMAGE):$(IMAGE_TAG) .

## image-test: Check the container image against its contract (run 'make image' first).
# Every check runs even after a failure; the target exits 1 when any failed. The
# generate check passes --log-level error so a passing run prints exactly the
# eight ok: lines; dizzy still prints a real error to stderr.
image-test:
	@docker image inspect $(IMAGE):$(IMAGE_TAG) >/dev/null \
		|| { echo "error: image $(IMAGE):$(IMAGE_TAG) not found (run 'make image' first)"; exit 1; }
	@img="$(IMAGE):$(IMAGE_TAG)"; want="dizzy $(IMAGE_TAG)"; rc=0; \
	insh() { docker run --rm --entrypoint sh "$$img" -c "$$1"; }; \
	report() { if [ "$$2" -eq 0 ]; then echo "ok: $$1"; else echo "FAIL: $$1"; rc=1; fi; }; \
	[ "$$(docker run --rm "$$img" --version)" = "$$want" ]; report version $$?; \
	[ "$$(insh 'echo "$$(id -u):$$(id -g)"')" = 65534:65534 ] && [ "$$(docker image inspect --format '{{.Config.User}}' "$$img")" = 65534:65534 ]; report user $$?; \
	[ "$$(docker image inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' "$$img")" = '["/usr/local/bin/dizzy"] null' ]; report entrypoint $$?; \
	insh 'test "$$(pwd)" = /work && touch /work/.probe'; report workdir $$?; \
	[ "$$(insh 'cp /usr/local/bin/dizzy /work/dizzy && /work/dizzy --version')" = "$$want" ]; report copy $$?; \
	insh 'test -s /etc/ssl/certs/ca-certificates.crt'; report ca-bundle $$?; \
	want_p=$$(cd scenarios && find . -type f \( -name '*.yaml' -o -name '*.yml' \) | LC_ALL=C sort); \
	got_p=$$(insh 'cd /usr/share/dizzy/scenarios && find . -type f | LC_ALL=C sort'); \
	go_n=$$(insh 'find /usr/share/dizzy -name "*.go" | wc -l' | tr -d ' '); \
	[ "$$got_p" = "$$want_p" ] && [ "$$go_n" = 0 ]; report profiles $$?; \
	insh 'for f in /usr/share/dizzy/scenarios/*/*.yaml; do svc=$$(basename "$$(dirname "$$f")"); /usr/local/bin/dizzy --log-level error "$$svc" generate --scenario "$$f" >/dev/null || exit 1; done'; report generate $$?; \
	exit $$rc

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

## devstack-osism-monitor: Run monitor --otel (DEVSTACK_OSISM_SERVICE=neutron|cinder|keystone|nova|glance) against the OSISM testbed, exporting to VictoriaMetrics.
# monitor applies and cleans up each iteration's resources itself, so this target
# runs no post-run cleanup: unlike `devstack-osism`, scanning run-*.json in the
# shared working directory would tear down the resources of a concurrent
# `make devstack-osism` run that happens to write its record alongside.
devstack-osism-monitor: build
	@test -f "$(DEVSTACK_OSISM_CLOUDS_FILE)" || { echo "error: clouds file $(DEVSTACK_OSISM_CLOUDS_FILE) not found"; exit 1; }
	@test -f "$(DEVSTACK_OSISM_CACERT)"      || { echo "error: CA cert $(DEVSTACK_OSISM_CACERT) not found (clouds.yaml 'cacert')"; exit 1; }
	@test -f "$(DEVSTACK_OSISM_SCENARIO)"    || { echo "error: scenario $(DEVSTACK_OSISM_SCENARIO) not found"; exit 1; }
	@curl -fsS -m 2 http://localhost:8428/health >/dev/null 2>&1 \
		|| echo "warning: VictoriaMetrics is not answering on localhost:8428 (run 'make otel-up' first); metrics will be exported into the void"
	@echo "Running $(DEVSTACK_OSISM_SERVICE) monitor --otel against the OSISM testbed:"
	@echo "  Cloud:    $(DEVSTACK_OSISM_OS_CLOUD) ($(DEVSTACK_OSISM_CLOUDS_FILE))"
	@echo "  Scenario: $(DEVSTACK_OSISM_SCENARIO)"
	@echo "  Cadence:  interval $(MONITOR_INTERVAL) (0 = continuous), iterations $(MONITOR_ITERATIONS) (0 = forever, Ctrl-C to stop)"
	@echo "  OTLP:     http://localhost:8428/opentelemetry/v1/metrics (15s export interval)"
	@OS_CLIENT_CONFIG_FILE="$(DEVSTACK_OSISM_CLOUDS_FILE)" \
	OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="http://localhost:8428/opentelemetry/v1/metrics" \
	OTEL_METRIC_EXPORT_INTERVAL=15000 \
	./$(BINARY) $(DEVSTACK_OSISM_SERVICE) monitor --os-cloud "$(DEVSTACK_OSISM_OS_CLOUD)" --scenario "$(DEVSTACK_OSISM_SCENARIO)" \
		--interval "$(MONITOR_INTERVAL)" --iterations "$(MONITOR_ITERATIONS)" --otel $(ARGS)

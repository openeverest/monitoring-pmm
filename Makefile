## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# CONTAINER_TOOL defines the container tool to be used for building images.
CONTAINER_TOOL ?= docker

# Image URL to use for all building/pushing image targets
IMG ?= ghcr.io/openeverest/monitoring-pmm-dev:latest

# Split IMG into repository and tag for Helm values
_IMG_REPO = $(firstword $(subst :, ,$(IMG)))
_IMG_TAG  = $(lastword $(subst :, ,$(IMG)))

# Branch or commit of openeverest/openeverest the integration tests install
# the monitoring CRDs from. Tracks the core API go.mod is pinned to.
OPENEVEREST_REF ?= monitoring/01-api

# Helm chart directory
CHART_DIR ?= charts/monitoring-pmm

# Namespace the extension is installed into
NAMESPACE ?= openeverest-system

# k3d cluster used by the integration tests
K3D_CLUSTER_NAME ?= monitoring-pmm-test

GOLANGCI_LINT_VERSION ?= v2.14.0

CONTROLLER_TOOLS_VERSION ?= v0.18.0
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)

YQ_VERSION ?= v4.44.6
YQ ?= $(LOCALBIN)/yq-$(YQ_VERSION)

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: lint
lint: ## Run golangci-lint.
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

.PHONY: test
test: ## Run Go unit tests.
	go test -race -coverprofile=coverage.out ./...

.PHONY: run
run: ## Run the controller against the current kubeconfig.
	go run ./cmd/controller

##@ Code Generation

.PHONY: manifests
manifests: controller-gen ## Generate the ClusterRole from kubebuilder RBAC markers.
	$(CONTROLLER_GEN) rbac:roleName=monitoring-pmm paths="./..." output:rbac:dir=config/rbac

.PHONY: helm-sync-rbac
helm-sync-rbac: yq ## Sync the generated RBAC rules into the Helm chart.
	@$(YQ) '.rules' config/rbac/role.yaml > $(CHART_DIR)/generated/rbac-rules.yaml

.PHONY: generate
generate: manifests helm-sync-rbac ## Run all code generation.

.PHONY: verify
verify: ## Verify that generated files are up-to-date (for CI).
	@$(MAKE) generate
	@if git diff --quiet -- config/ $(CHART_DIR)/generated/; then \
		echo "Generated files are up-to-date."; \
	else \
		echo "ERROR: Generated files are out of date. Run 'make generate' and commit the changes."; \
		git diff -- config/ $(CHART_DIR)/generated/; \
		exit 1; \
	fi

##@ Build

.PHONY: build
build: $(LOCALBIN) ## Build the controller binary.
	CGO_ENABLED=0 go build -o $(LOCALBIN)/controller ./cmd/controller

.PHONY: docker-build
docker-build: ## Build the controller image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the controller image.
	$(CONTAINER_TOOL) push $(IMG)

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint $(CHART_DIR)

.PHONY: helm-template
helm-template: ## Render the chart locally.
	helm template monitoring-pmm $(CHART_DIR) -n $(NAMESPACE)

##@ Integration tests

.PHONY: k3d-cluster-up
k3d-cluster-up: ## Create the k3d cluster for the integration tests.
	k3d cluster create $(K3D_CLUSTER_NAME) --wait --no-lb \
		--k3s-arg '--disable=traefik,metrics-server,servicelb@server:*'

.PHONY: k3d-cluster-down
k3d-cluster-down: ## Delete the k3d cluster.
	k3d cluster delete $(K3D_CLUSTER_NAME)

.PHONY: load-image
load-image: ## Import IMG into the k3d cluster.
	k3d image import $(IMG) -c $(K3D_CLUSTER_NAME)

.PHONY: install-crds
install-crds: ## Install the OpenEverest monitoring CRDs.
	kubectl apply --server-side -f https://raw.githubusercontent.com/openeverest/openeverest/$(OPENEVEREST_REF)/config/crd/bases/monitoring.openeverest.io_monitoringclasses.yaml
	kubectl apply --server-side -f https://raw.githubusercontent.com/openeverest/openeverest/$(OPENEVEREST_REF)/config/crd/bases/monitoring.openeverest.io_monitoringdestinations.yaml
	kubectl apply --server-side -f https://raw.githubusercontent.com/openeverest/openeverest/$(OPENEVEREST_REF)/config/crd/bases/monitoring.openeverest.io_monitoringbindings.yaml
	kubectl wait --for=condition=Established --timeout=60s \
		crd/monitoringclasses.monitoring.openeverest.io \
		crd/monitoringdestinations.monitoring.openeverest.io \
		crd/monitoringbindings.monitoring.openeverest.io

.PHONY: deploy-ci
deploy-ci: ## Install the chart with IMG (already imported into k3d).
	helm upgrade --install monitoring-pmm $(CHART_DIR) \
		--create-namespace \
		--namespace $(NAMESPACE) \
		--set image.repository=$(_IMG_REPO) \
		--set image.tag=$(_IMG_TAG) \
		--set image.pullPolicy=Never \
		--wait --timeout 2m

.PHONY: test-integration
test-integration: ## Run the chainsaw integration tests against the current cluster.
	chainsaw test --config ./test/integration/.chainsaw.yaml ./test/integration

##@ Tool Dependencies

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Install controller-gen.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: yq
yq: $(YQ) ## Install yq.
$(YQ): $(LOCALBIN)
	$(call go-install-tool,$(YQ),github.com/mikefarah/yq/v4,$(YQ_VERSION))

# go-install-tool will 'go install' any package with custom target and target name.
define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3); \
echo "Installing $${package}"; \
GOBIN=$(LOCALBIN) go install $${package}; \
mv -f $$(echo "$(1)" | sed "s/-$(3)$$//") $(1); \
}
endef

# Timestamp-based image tag — evaluated once at parse time, same across all targets.
BUILD_TAG    := $(shell date +%Y%m%d-%H%M%S)

# Image URL to use all building/pushing image targets
# One image per component domain; the variable, the Dockerfile target and the docker-build-<name> target share the name.
CONTROLLER_IMG ?= cybericebox/laboratory-controller:$(BUILD_TAG)
AGENT_IMG      ?= cybericebox/laboratory-agent:$(BUILD_TAG)
PROXY_IMG      ?= cybericebox/laboratory-proxy:$(BUILD_TAG)
NODE_IMG       ?= cybericebox/laboratory-node:$(BUILD_TAG)
LAB_IMG        ?= cybericebox/laboratory-lab:$(BUILD_TAG)
IMAGES         := controller agent proxy node lab
IMG_controller  = $(CONTROLLER_IMG)
IMG_agent       = $(AGENT_IMG)
IMG_proxy       = $(PROXY_IMG)
IMG_node        = $(NODE_IMG)
IMG_lab         = $(LAB_IMG)

KIND_CLUSTER_NAME ?= icebox

# Lima/k0s dev cluster
# Local cluster kit (Lima VMs, k0s, Kind config, lab scenarios) lives in the infrastructure repo (override LOCAL_K0S to point elsewhere).
LOCAL_K0S      ?= ../infra/local/cluster
LIMA_CTRL      ?= lab-ctrl
LIMA_WORKER    ?= lab-worker
CHART_PATH     ?= charts/laboratory
HELM_NS        ?= laboratory-system

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/*.yaml charts/laboratory/crds/

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: generate-api
generate-api:
	GOTOOLCHAIN=$(GO_TOOLCHAIN) ./hack/update-codegen.sh ## Generate code API client, lister, and informer implementations.

##@ Kind (local testing)

.PHONY: cluster-up
cluster-up: ## Create 3-node Kind cluster (1 control-plane + 2 workers)
	$(KIND) create cluster --config $(LOCAL_K0S)/kind/kind-config.yaml --name $(KIND_CLUSTER_NAME)
	$(KUBECTL) create namespace lab-system --dry-run=client -o yaml | $(KUBECTL) apply -f -

.PHONY: cluster-down
cluster-down: ## Delete Kind cluster
	$(KIND) delete cluster --name $(KIND_CLUSTER_NAME)

.PHONY: docker-build-controller docker-build-agent docker-build-proxy docker-build-node docker-build-lab
docker-build-controller: ## Build the controller (operator) image
	$(CONTAINER_TOOL) build -t $(CONTROLLER_IMG) --target controller .
docker-build-agent: ## Build the agent (gRPC API) image
	$(CONTAINER_TOOL) build -t $(AGENT_IMG) --target agent .
docker-build-proxy: ## Build the proxy (proxy-l7 + proxy-wg) image
	$(CONTAINER_TOOL) build -t $(PROXY_IMG) --target proxy .
docker-build-node: ## Build the node (node-agent + OVS) image
	$(CONTAINER_TOOL) build -t $(NODE_IMG) --target node .
docker-build-lab: ## Build the lab (vpn + gateway) image
	$(CONTAINER_TOOL) build -t $(LAB_IMG) --target lab .

.PHONY: docker-build
docker-build: docker-build-controller docker-build-agent docker-build-proxy docker-build-node docker-build-lab ## Build all 5 images

.PHONY: docker-push-controller docker-push-agent docker-push-proxy docker-push-node docker-push-lab
docker-push-controller: ## Push the controller image
	$(CONTAINER_TOOL) push $(CONTROLLER_IMG)
docker-push-agent: ## Push the agent image
	$(CONTAINER_TOOL) push $(AGENT_IMG)
docker-push-proxy: ## Push the proxy image
	$(CONTAINER_TOOL) push $(PROXY_IMG)
docker-push-node: ## Push the node image
	$(CONTAINER_TOOL) push $(NODE_IMG)
docker-push-lab: ## Push the lab image
	$(CONTAINER_TOOL) push $(LAB_IMG)

.PHONY: docker-push
docker-push: docker-push-controller docker-push-agent docker-push-proxy docker-push-node docker-push-lab ## Push all 5 images

.PHONY: kind-load-proxy
kind-load-proxy: docker-build-proxy ## Build and load proxy image into Kind cluster
	$(KIND) load docker-image $(PROXY_IMG) --name $(KIND_CLUSTER_NAME)

.PHONY: kind-load
kind-load: docker-build ## Build and load all images into Kind cluster
	$(KIND) load docker-image $(CONTROLLER_IMG) --name $(KIND_CLUSTER_NAME)
	$(KIND) load docker-image $(NODE_IMG)  --name $(KIND_CLUSTER_NAME)
	$(KIND) load docker-image $(LAB_IMG)   --name $(KIND_CLUSTER_NAME)
	$(KIND) load docker-image $(PROXY_IMG) --name $(KIND_CLUSTER_NAME)
	$(KIND) load docker-image $(AGENT_IMG) --name $(KIND_CLUSTER_NAME)

.PHONY: kind-patch-node
kind-patch-node: ## Patch node-agent DaemonSet to use local image
	$(KUBECTL) set image daemonset/laboratory-node-agent \
		node-agent=$(NODE_IMG) ovs=$(NODE_IMG) host-prep=$(NODE_IMG) install-cni-bins=$(NODE_IMG) install-cni-conf=$(NODE_IMG) \
		-n laboratory-system
	$(KUBECTL) patch daemonset laboratory-node-agent -n laboratory-system \
		--type=json -p='[{"op":"replace","path":"/spec/template/spec/initContainers/0/imagePullPolicy","value":"Never"},{"op":"replace","path":"/spec/template/spec/initContainers/1/imagePullPolicy","value":"Never"},{"op":"replace","path":"/spec/template/spec/initContainers/2/imagePullPolicy","value":"Never"},{"op":"replace","path":"/spec/template/spec/initContainers/3/imagePullPolicy","value":"Never"},{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'

.PHONY: kind-reload-node
kind-reload-node: docker-build-node ## Rebuild node-agent image, reload into Kind, restart DaemonSet
	$(KIND) load docker-image $(NODE_IMG) --name $(KIND_CLUSTER_NAME)
	$(MAKE) kind-patch-node
	$(KUBECTL) rollout restart daemonset/laboratory-node-agent -n laboratory-system
	$(KUBECTL) rollout status  daemonset/laboratory-node-agent -n laboratory-system

.PHONY: kind-reload-operator
kind-reload-operator: docker-build-controller ## Rebuild operator image, reload into Kind, restart controller
	$(KIND) load docker-image $(CONTROLLER_IMG) --name $(KIND_CLUSTER_NAME)
	$(KUBECTL) patch deployment laboratory-controller-manager -n laboratory-system \
		--type=json -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
	$(KUBECTL) rollout restart deployment/laboratory-controller-manager -n laboratory-system
	$(KUBECTL) rollout status  deployment/laboratory-controller-manager -n laboratory-system

.PHONY: kind-reload-lab
kind-reload-lab: docker-build-lab ## Rebuild lab (vpn+gateway) image, reload into Kind
	$(KIND) load docker-image $(LAB_IMG) --name $(KIND_CLUSTER_NAME)
	@echo "Lab image loaded. Delete VPN/gateway pods to pick up new image:"
	@echo "  kubectl delete pods -n <namespace> -l app=vpn"
	@echo "  kubectl delete pods -n <namespace> -l app=gateway"

.PHONY: kind-reload
kind-reload: kind-reload-operator kind-reload-node kind-reload-lab ## Rebuild and reload all components

# ── Lima / k0s helpers ────────────────────────────────────────────────────────
# Import a single image into both Lima VMs (ctrl + worker).
# Usage: $(call k0s-import,$(NODE_IMG))
define k0s-import
	docker save $(1) | limactl shell $(LIMA_CTRL)   -- sudo k0s ctr --namespace k8s.io images import -
	docker save $(1) | limactl shell $(LIMA_WORKER) -- sudo k0s ctr --namespace k8s.io images import -
endef

##@ Lima / k0s (dev cluster)

.PHONY: k0s-deploy
k0s-deploy: docker-build ## Build ALL images, import into Lima, full helm install (use on fresh cluster)
	$(call k0s-import,$(CONTROLLER_IMG))
	$(call k0s-import,$(NODE_IMG))
	$(call k0s-import,$(LAB_IMG))
	$(call k0s-import,$(PROXY_IMG))
	$(call k0s-import,$(AGENT_IMG))
	helm upgrade --install laboratory $(CHART_PATH) \
		--namespace $(HELM_NS) --create-namespace \
		--values $(CHART_PATH)/values.yaml \
		--set operator.image.tag=$(BUILD_TAG) \
		--set nodeAgent.image.tag=$(BUILD_TAG) \
		--set agent.image.tag=$(BUILD_TAG) \
		--set vpn.image.tag=$(BUILD_TAG) \
		--set inetGateway.image.tag=$(BUILD_TAG) \
		--set proxy.l7.image.tag=$(BUILD_TAG) \
		--set proxy.wg.image.tag=$(BUILD_TAG) \
		--wait --timeout=5m
	@echo ""
	@echo "✓ deployed all: $(BUILD_TAG)"

.PHONY: k0s-reload-operator
k0s-reload-operator: docker-build-controller ## Rebuild operator, import into Lima, update image tag
	$(call k0s-import,$(CONTROLLER_IMG))
	helm upgrade laboratory $(CHART_PATH) \
		--namespace $(HELM_NS) \
		--reuse-values \
		--set operator.image.tag=$(BUILD_TAG) \
		--wait --timeout=3m
	@echo ""
	@echo "✓ operator deployed: $(CONTROLLER_IMG)"

.PHONY: k0s-reload-node
k0s-reload-node: docker-build-node ## Rebuild node-agent, import into Lima, update image tag
	$(call k0s-import,$(NODE_IMG))
	helm upgrade laboratory $(CHART_PATH) \
		--namespace $(HELM_NS) \
		--reuse-values \
		--set nodeAgent.image.tag=$(BUILD_TAG) \
		--wait --timeout=3m
	@echo ""
	@echo "✓ node-agent deployed: $(NODE_IMG)"

.PHONY: k0s-reload-lab
k0s-reload-lab: docker-build-lab ## Rebuild lab (vpn+gateway) image, import into Lima, update image tag
	$(call k0s-import,$(LAB_IMG))
	helm upgrade laboratory $(CHART_PATH) \
		--namespace $(HELM_NS) \
		--reuse-values \
		--set vpn.image.tag=$(BUILD_TAG) \
		--set inetGateway.image.tag=$(BUILD_TAG)
	@echo ""
	@echo "✓ lab image updated: $(LAB_IMG)"
	@echo "  Restart vpn/gateway pods to apply: kubectl delete pods -n <ns> -l app=vpn,app=gateway"

.PHONY: k0s-reload-proxy
k0s-reload-proxy: docker-build-proxy ## Rebuild proxy image, import into Lima, update image tag
	$(call k0s-import,$(PROXY_IMG))
	helm upgrade laboratory $(CHART_PATH) \
		--namespace $(HELM_NS) \
		--reuse-values \
		--set proxy.l7.image.tag=$(BUILD_TAG) \
		--set proxy.wg.image.tag=$(BUILD_TAG) \
		--wait --timeout=3m
	@echo ""
	@echo "✓ proxy deployed: $(PROXY_IMG)"

.PHONY: k0s-reload
k0s-reload: k0s-reload-operator k0s-reload-node k0s-reload-lab k0s-reload-proxy ## Rebuild and reload all components

.PHONY: k0s-upgrade-chart
k0s-upgrade-chart: ## Apply values.yaml changes to existing cluster (preserves current image tags)
	helm upgrade laboratory $(CHART_PATH) \
		--namespace $(HELM_NS) \
		--reuse-values \
		--values $(CHART_PATH)/values.yaml \
		--wait --timeout=2m

.PHONY: kind-deploy
kind-deploy: kind-load install deploy ## Full local deploy: build all + load + CRDs + controller + node-agent
	$(KUBECTL) create namespace lab-system --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUSTOMIZE) build config/node-agent | $(KUBECTL) apply -f -
	$(MAKE) kind-patch-node
	@echo ""
	@echo "Cluster ready. Run tests:"
	@echo "  $(LOCAL_K0S)/scenarios/run.sh single-node"
	@echo "  $(LOCAL_K0S)/scenarios/run.sh multi-node"
	@echo "  $(LOCAL_K0S)/scenarios/run.sh vpn"

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@$(KIND) get clusters | grep -q 'kind' || { \
		echo "No Kind cluster is running. Please start a Kind cluster before running the e2e tests."; \
		exit 1; \
	}
	go test ./test/e2e/ -v -ginkgo.v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager ./cmd/manager

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/manager

# Multi-platform build and push of one image: make docker-buildx IMAGE=proxy PLATFORMS=linux/arm64,linux/amd64
# (IMAGE is one of controller agent proxy node lab). Needs docker buildx and a registry you can push to.
PLATFORMS ?= linux/arm64,linux/amd64
IMAGE ?= controller
.PHONY: docker-buildx
docker-buildx: ## Build and push one image (IMAGE=...) for several platforms
	- $(CONTAINER_TOOL) buildx create --name laboratory-builder
	$(CONTAINER_TOOL) buildx use laboratory-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --target $(IMAGE) --tag $(IMG_$(IMAGE)) .
	- $(CONTAINER_TOOL) buildx rm laboratory-builder

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${CONTROLLER_IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${CONTROLLER_IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
# Tools are built with the toolchain of go.mod, not whatever go is installed: a tool
# built by an older Go cannot parse the newer standard library (generic methods).
GO_TOOLCHAIN ?= $(shell go env GOVERSION)
KUSTOMIZE_VERSION ?= v5.8.2
CONTROLLER_TOOLS_VERSION ?= v0.22.0
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v1.63.4

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

# The image URL that every image build target and push target uses.
IMG ?= controller:latest

# Resolve the Go install path. The path is GOPATH/bin unless GOBIN is set.
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL names the container tool that builds images. The targets are
# tested only with Docker, which the scaffolding sets by default. You can
# replace the value with another tool, for example podman.
CONTAINER_TOOL ?= docker

# Set SHELL to bash so that recipes can run bash commands. The options exit when
# a recipe line returns a non-zero status or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints every target with its description, organized beneath
# its category. A '##@' comment marks a category, and a '##' comment marks a
# target description. The awk command reads every makefile in this invocation,
# finds lines of the form `xyz: ## something`, and formats the target and its
# help text. A line of the form `##@ something` prints as a category heading.
# For more information about ANSI control characters for terminal formatting,
# see https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# For more information about the awk command, see
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate the ClusterRole from the kubebuilder RBAC markers.
	$(CONTROLLER_GEN) rbac:roleName=kata-provider-manager-role paths="./internal/..." output:rbac:artifacts:config=config/components/controller_rbac

.PHONY: generate
generate: controller-gen defaulter-gen
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."
	$(DEFAULTER_GEN) ./internal/config --output-file=zz_generated.defaults.go

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go -health-probe-bind-address 0 \
		--server-config ./config/base/manager/config.yaml

# To build the manager image for another platform, use the --platform flag, for
# example `docker build --platform linux/arm64`. Docker BuildKit must be enabled
# first. For more information, see
# https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS names the target platforms that the manager image is built for, so
# that the image supports multiple architectures. For example, run
# `make docker-buildx IMG=myregistry/myoperator:0.0.1`. To use this option, you
# need to:
# - Run docker buildx. For more information, see https://docs.docker.com/build/buildx/
# - Enable BuildKit. For more information, see https://docs.docker.com/develop/develop-images/build_enhancements/
# - Push the image to your registry. An invalid IMG=<myregistry/image:<tag>> value fails the export.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name kata-provider-builder
	$(CONTAINER_TOOL) buildx use kata-provider-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm kata-provider-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/base/manager && $(KUSTOMIZE) edit set image ghcr.io/datum-labs/kata-provider=${IMG}
	$(KUSTOMIZE) build config/overlays/cell > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: kustomize ## Install the compute CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build "github.com/datum-cloud/compute//config/base/crd?ref=main" | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: kustomize ## Uninstall the compute CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build "github.com/datum-cloud/compute//config/base/crd?ref=main" | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/base/manager && $(KUSTOMIZE) edit set image ghcr.io/datum-labs/kata-provider=${IMG}
	$(KUSTOMIZE) build config/overlays/dev | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/overlays/dev | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## The directory that dependencies install into.
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
DEFAULTER_GEN ?= $(LOCALBIN)/defaulter-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.4.3
CONTROLLER_TOOLS_VERSION ?= v0.17.1
DEFAULTER_GEN_VERSION ?= v0.33.2
GOLANGCI_LINT_VERSION ?= v2.9.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: defaulter-gen
defaulter-gen: $(DEFAULTER_GEN) ## Download defaulter-gen locally if necessary.
$(DEFAULTER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(DEFAULTER_GEN),k8s.io/code-generator/cmd/defaulter-gen,$(DEFAULTER_GEN_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool runs 'go install' for a package, under a custom target path and
# binary name, when that binary does not already exist.
# $1 - target path, including the name of the binary
# $2 - URL of the package to install
# $3 - version of the package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

# Image to build, push and deploy.
IMG ?= ghcr.io/mella7/project-claim:latest

# Tool versions. Binaries go to ./bin with the version in the name, so a
# version bump here installs the new one instead of reusing a stale binary.
CONTROLLER_TOOLS_VERSION ?= v0.19.0
ENVTEST_VERSION ?= release-0.22
ENVTEST_K8S_VERSION ?= 1.34.1
GOLANGCI_LINT_VERSION ?= v2.15.0

LOCALBIN := $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
KUBECTL ?= kubectl
CONTAINER_TOOL ?= docker
SANDBOX_KUBECONFIG := $(LOCALBIN)/sandbox.kubeconfig

# The API server and etcd binaries the tests and the sandbox run against.
# Set KUBEBUILDER_ASSETS yourself to use binaries you already have.
envtest-assets = $(or $(KUBEBUILDER_ASSETS),$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path))

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_0-9-]+:.*##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)}' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate the CRDs and the ClusterRole from the markers in the Go code.
	$(CONTROLLER_GEN) crd rbac:roleName=projectclaim-manager paths=./... \
		output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate the DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

.PHONY: fmt
fmt: ## Format the code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: verify
verify: manifests generate ## Fail if the generated files are not up to date.
	git diff --exit-code -- api config

##@ Testing

.PHONY: test
test: manifests generate fmt vet envtest ## Run all tests against a local API server and etcd.
	KUBEBUILDER_ASSETS="$(envtest-assets)" go test ./... -coverpkg=./internal/... -coverprofile cover.out

.PHONY: mutation-test
mutation-test: envtest ## Break the operator in known ways and check that the tests notice.
	KUBEBUILDER_ASSETS="$(envtest-assets)" python3 hack/mutate.py

.PHONY: demo
demo: ## Walk through the operator's behaviour against the current cluster, with checks.
	KUBECTL="$(KUBECTL)" bash hack/demo.sh

##@ Running locally

.PHONY: build
build: manifests generate fmt vet ## Build the manager binary.
	go build -o bin/manager ./cmd

.PHONY: sandbox
sandbox: envtest ## Start a throwaway API server with the CRDs installed. No cluster needed.
	KUBEBUILDER_ASSETS="$(envtest-assets)" go run ./hack/sandbox --out $(SANDBOX_KUBECONFIG)

.PHONY: run-sandbox
run-sandbox: ## Run the operator against the sandbox.
	KUBECONFIG=$(SANDBOX_KUBECONFIG) go run ./cmd --metrics-secure=false --metrics-bind-address=:8080

.PHONY: run
run: manifests generate ## Run the operator on this machine against the cluster in your kubeconfig.
	go run ./cmd --metrics-secure=false --metrics-bind-address=:8080

##@ Image

.PHONY: docker-build
docker-build: ## Build the image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the image.
	$(CONTAINER_TOOL) push $(IMG)

##@ Deployment

.PHONY: build-installer
build-installer: manifests ## Write everything needed to install the operator to dist/install.yaml.
	@mkdir -p dist
	$(KUBECTL) kustomize config/default | sed 's#ghcr.io/mella7/project-claim:latest#$(IMG)#' > dist/install.yaml

.PHONY: install
install: manifests ## Install the CRDs and the admission policy.
	$(KUBECTL) apply --server-side -k config/crd
	$(KUBECTL) apply -k config/admission

.PHONY: uninstall
uninstall: ## Remove the CRDs and the admission policy. Deletes every claim, tier and tenant.
	$(KUBECTL) delete --ignore-not-found -k config/admission
	$(KUBECTL) delete --ignore-not-found -k config/crd

.PHONY: deploy
deploy: build-installer ## Deploy the operator with image IMG.
	$(KUBECTL) apply --server-side -f dist/install.yaml

.PHONY: undeploy
undeploy: ## Remove the operator. Delete the claims first, or their finalizers will hold them.
	$(KUBECTL) delete --ignore-not-found -f dist/install.yaml

##@ Tools

.PHONY: envtest
envtest: $(if $(KUBEBUILDER_ASSETS),,$(ENVTEST)) ## Install setup-envtest, unless KUBEBUILDER_ASSETS is set.

$(CONTROLLER_GEN):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
	mv $(LOCALBIN)/controller-gen $@

$(ENVTEST):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
	mv $(LOCALBIN)/setup-envtest $@

$(GOLANGCI_LINT):
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(LOCALBIN)/golangci-lint $@

##@ Operator

LOCALBIN ?= $(shell pwd)/bin

CONTROLLER_TOOLS_VERSION ?= v0.22.0
ENVTEST_VERSION ?= v0.25.0
GOIMPORTS_VERSION ?= v0.52.0
ENVTEST_K8S_VERSION ?= 1.37
KIND_VERSION ?= v0.33.0

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)
GOIMPORTS ?= $(LOCALBIN)/goimports-$(GOIMPORTS_VERSION)
KIND ?= $(LOCALBIN)/kind-$(KIND_VERSION)

# The e2e test runs the operator image in a kind cluster. Set CONTAINER_TOOL=podman and
# KIND_EXPERIMENTAL_PROVIDER=podman for podman.
CONTAINER_TOOL ?= docker
E2E_CLUSTER ?= pod-shutdown-operator-e2e
E2E_DIR ?= $(LOCALBIN)/e2e
E2E_IMAGE ?= gsoci.azurecr.io/giantswarm/pod-shutdown-operator:e2e
E2E_ARCH ?= $(shell go env GOARCH)

# The go-build job in CircleCI checks every Go file with goimports, also the generated files.
.PHONY: generate
generate: $(CONTROLLER_GEN) $(GOIMPORTS) ## Generate the DeepCopy methods of the API types.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."
	"$(GOIMPORTS)" -local github.com/giantswarm/pod-shutdown-operator -w api/v1alpha1/zz_generated.deepcopy.go

# The Helm chart reads the CRDs and the rules of the ClusterRole from its files directory.
.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate the CRDs and the ClusterRole.
	"$(CONTROLLER_GEN)" rbac:roleName=pod-shutdown-operator crd paths="./..." \
		output:crd:artifacts:config=helm/pod-shutdown-operator/files/crds \
		output:rbac:artifacts:config=helm/pod-shutdown-operator/files/rbac

.PHONY: test-envtest
test-envtest: $(ENVTEST) ## Run the envtest tests against a local API server.
	KUBEBUILDER_ASSETS="$$("$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -tags envtest $(RACE) ./...

.PHONY: test-e2e
test-e2e: e2e-deploy ## Run the e2e tests in a kind cluster. The cluster stays for a new run.
	KUBECONFIG="$(E2E_DIR)/kubeconfig" go test -tags e2e -count=1 -timeout 30m ./tests/e2e/... -ginkgo.v

.PHONY: e2e-deploy
e2e-deploy: $(KIND) ## Build the operator image, and install the chart in the kind cluster of the e2e tests.
	# The build context holds only the binary, and not the kubeconfig or the image archive.
	mkdir -p "$(E2E_DIR)/image"
	CGO_ENABLED=0 GOOS=linux GOARCH=$(E2E_ARCH) go build -o "$(E2E_DIR)/image/pod-shutdown-operator-linux-$(E2E_ARCH)" .
	$(CONTAINER_TOOL) build --build-arg TARGETARCH=$(E2E_ARCH) -t $(E2E_IMAGE) -f Dockerfile "$(E2E_DIR)/image"
	$(CONTAINER_TOOL) save -o "$(E2E_DIR)/image.tar" $(E2E_IMAGE)
	"$(KIND)" get clusters | grep -qx $(E2E_CLUSTER) || \
		"$(KIND)" create cluster --name $(E2E_CLUSTER) --config tests/e2e/kind-config.yaml --kubeconfig "$(E2E_DIR)/kubeconfig"
	"$(KIND)" get kubeconfig --name $(E2E_CLUSTER) > "$(E2E_DIR)/kubeconfig"
	"$(KIND)" load image-archive "$(E2E_DIR)/image.tar" --name $(E2E_CLUSTER)
	KUBECONFIG="$(E2E_DIR)/kubeconfig" helm upgrade --install pod-shutdown-operator helm/pod-shutdown-operator \
		--namespace pod-shutdown-operator --create-namespace --values tests/e2e/values.yaml --wait --timeout 5m
	# A new image with the same tag needs new pods.
	KUBECONFIG="$(E2E_DIR)/kubeconfig" kubectl --namespace pod-shutdown-operator rollout restart deployment/pod-shutdown-operator
	KUBECONFIG="$(E2E_DIR)/kubeconfig" kubectl --namespace pod-shutdown-operator rollout status deployment/pod-shutdown-operator --timeout 5m

.PHONY: e2e-delete
e2e-delete: $(KIND) ## Delete the kind cluster of the e2e tests.
	"$(KIND)" delete cluster --name $(E2E_CLUSTER)

$(CONTROLLER_GEN):
	mkdir -p "$(LOCALBIN)"
	GOBIN="$(LOCALBIN)" go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
	mv "$(LOCALBIN)/controller-gen" "$@"

$(ENVTEST):
	mkdir -p "$(LOCALBIN)"
	GOBIN="$(LOCALBIN)" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
	mv "$(LOCALBIN)/setup-envtest" "$@"

$(GOIMPORTS):
	mkdir -p "$(LOCALBIN)"
	GOBIN="$(LOCALBIN)" go install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)
	mv "$(LOCALBIN)/goimports" "$@"

$(KIND):
	mkdir -p "$(LOCALBIN)"
	GOBIN="$(LOCALBIN)" go install sigs.k8s.io/kind@$(KIND_VERSION)
	mv "$(LOCALBIN)/kind" "$@"

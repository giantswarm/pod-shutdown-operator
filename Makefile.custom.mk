##@ Operator

LOCALBIN ?= $(shell pwd)/bin

CONTROLLER_TOOLS_VERSION ?= v0.22.0
ENVTEST_VERSION ?= v0.25.0
GOIMPORTS_VERSION ?= v0.52.0
ENVTEST_K8S_VERSION ?= 1.37

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)
GOIMPORTS ?= $(LOCALBIN)/goimports-$(GOIMPORTS_VERSION)

# The go-build job in CircleCI checks every Go file with goimports, also the generated files.
.PHONY: generate
generate: $(CONTROLLER_GEN) $(GOIMPORTS) ## Generate the DeepCopy methods of the API types.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."
	"$(GOIMPORTS)" -local github.com/giantswarm/pod-shutdown-operator -w api/v1alpha1/zz_generated.deepcopy.go

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate the CRDs and the ClusterRole.
	"$(CONTROLLER_GEN)" rbac:roleName=pod-shutdown-operator crd paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: test-envtest
test-envtest: $(ENVTEST) ## Run the envtest tests against a local API server.
	KUBEBUILDER_ASSETS="$$("$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -tags envtest $(RACE) ./...

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

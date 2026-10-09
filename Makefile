IMG ?= gpu-fleet-sentinel:dev
CLUSTER ?= sentinel
NAMESPACE ?= gpu-sentinel

.PHONY: scale fuzz generate all fmt vet lint test test-integration envtest cover build image kind-up kind-load deploy e2e demo kind-down

all: fmt vet test build

fmt:
	gofmt -w .

vet:
	go vet ./...

lint:
	golangci-lint run ./...

test:
	go test -race -count=1 ./...

ENVTEST_K8S ?= 1.32.0
ENVTEST_DIR ?= $(CURDIR)/bin/envtest

envtest:
	go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use $(ENVTEST_K8S) --bin-dir $(ENVTEST_DIR) -p path

test-integration:
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use $(ENVTEST_K8S) --bin-dir $(ENVTEST_DIR) -p path)" \
	  go test -tags integration -race -count=1 -v ./test/integration/

# 1,000-node benchmark; needs kwok (brew install kwok) and `make envtest` assets.
scale:
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use $(ENVTEST_K8S) --bin-dir $(ENVTEST_DIR) -p path)" hack/scale.sh

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.2

# Regenerates deepcopy code and the CRD shipped in the Helm chart.
generate:
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=deploy/helm/gpu-fleet-sentinel/crds

# Each fuzz target runs for FUZZTIME. Crashers are written to testdata/fuzz/ and
# then run as ordinary tests, so commit them.
FUZZTIME ?= 20s
fuzz:
	go test -run '^$$' -fuzz '^FuzzParseDCGM$$' -fuzztime $(FUZZTIME) ./internal/health
	go test -run '^$$' -fuzz '^FuzzEvaluate$$' -fuzztime $(FUZZTIME) ./internal/health
	go test -run '^$$' -fuzz '^FuzzChatCompletionsResponse$$' -fuzztime $(FUZZTIME) ./internal/incident
	go test -run '^$$' -fuzz '^FuzzTruncate$$' -fuzztime $(FUZZTIME) ./internal/textutil

cover:
	go test -race -coverprofile=cover.out ./... && go tool cover -func=cover.out | tail -1

build:
	CGO_ENABLED=0 go build -o bin/ ./cmd/...

image:
	docker build -t $(IMG) .

kind-up:
	kind create cluster --name $(CLUSTER) --config deploy/kind/cluster.yaml

kind-load: image
	kind load docker-image $(IMG) --name $(CLUSTER)

deploy:
	helm upgrade --install sentinel deploy/helm/gpu-fleet-sentinel \
	  --namespace $(NAMESPACE) --create-namespace \
	  -f deploy/kind/values-kind.yaml --set image.repository=$(firstword $(subst :, ,$(IMG))) \
	  --set image.tag=$(lastword $(subst :, ,$(IMG))) --wait

e2e: kind-load deploy
	NAMESPACE=$(NAMESPACE) ./hack/e2e.sh

demo:
	NAMESPACE=$(NAMESPACE) ./hack/demo.sh

kind-down:
	kind delete cluster --name $(CLUSTER)

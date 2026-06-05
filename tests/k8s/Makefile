# tests/k8s/Makefile — kind-based integration tests.
#
# Deploys postgres + mock-llm + mock-mcp (helm umbrella chart at deploy/deps)
# and bifrost (serelib-rendered from deploy/bifrost, applied with kubectl), then
# drives the go-bifrost-ai SDK.
#
# CLUSTER LIFECYCLE IS OWNED HERE / BY CI, NOT THE GO CODE. The Go fixtures
# attach to an existing cluster via $KUBECONFIG (kind-up writes it).
#
# Quick start (local):
#   make kind-up                                   # create cluster + verify
#   make test-k8s-api-sanity NAMESPACE=bifrost-test-local
#   make kind-down                                 # delete cluster
#
# CI (one job): before_script `make kind-up`; script
#   `make test-k8s-api-sanity NAMESPACE=bifrost-test-$CI_JOB_ID`; after_script
#   `kubectl delete ns bifrost-test-$CI_JOB_ID`.
#
.DEFAULT_GOAL := help

GO         ?= go
TAGS       ?= k8s
TIMEOUT    ?= 30m
COUNT      ?= 1
GOPRIVATE  ?= gopkg.volterra.us

REPO_ROOT  := $(abspath $(CURDIR)/../..)

CLUSTER    ?= bifrost-test
NODE_IMAGE ?= kindest/node:v1.31.4
# Kubeconfig written by kind-up (repointed at the control-plane container IP)
# and consumed by the test targets via $KUBECONFIG.
KUBECONFIG_PATH ?= /tmp/$(CLUSTER).kubeconfig
# Deploy namespace; CI passes a per-job unique value (its lifecycle is then
# caller-owned and the test won't delete it).
NAMESPACE  ?= bifrost-apisanity

# Image build is a prerequisite of the tests; bypass with SKIP_IMAGE=1.
IMAGE_DEP := image
ifeq ($(SKIP_IMAGE),1)
  IMAGE_DEP :=
endif

# Cluster setup is a prerequisite of the tests; bypass with SKIP_KIND_UP=1 when
# CI already ran `make kind-up` in before_script.
KIND_DEP := kind-up
ifeq ($(SKIP_KIND_UP),1)
  KIND_DEP :=
endif

# Friendly make vars -> harness env vars. KUBECONFIG points the Go fixtures at
# the kind-up-written, container-IP-repointed kubeconfig; BIFROST_K8S_NAMESPACE
# selects the deploy namespace.
TEST_ENV  = GOWORK=off GOPRIVATE=$(GOPRIVATE) BIFROST_K8S_SKIP_BUILD=1
TEST_ENV += KUBECONFIG=$(KUBECONFIG_PATH) BIFROST_K8S_CLUSTER=$(CLUSTER) BIFROST_K8S_NAMESPACE=$(NAMESPACE)
TEST_ENV += $(if $(filter 1,$(KEEP)),BIFROST_K8S_KEEP=1)

.PHONY: help image kind-up kind-down clean test test-k8s-api-sanity test-k8s-crud-sanity

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n",$$1,$$2}'

image: ## Build bifrost:local-test from the F5XC-patched source (delegates to root Makefile)
	@$(MAKE) -C $(REPO_ROOT) test-k8s-image

kind-up: ## Create the kind cluster if missing, repoint kubeconfig at the node IP, verify connectivity
	@if ! kind get clusters 2>/dev/null | grep -qx '$(CLUSTER)'; then \
		echo "creating kind cluster '$(CLUSTER)' ($(NODE_IMAGE))"; \
		kind create cluster --name '$(CLUSTER)' --image '$(NODE_IMAGE)' --wait 120s; \
	else echo "kind cluster '$(CLUSTER)' already exists"; fi
	@kind export kubeconfig --name '$(CLUSTER)' --kubeconfig '$(KUBECONFIG_PATH)'
	@ip=$$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' '$(CLUSTER)-control-plane'); \
		if [ -n "$$ip" ]; then \
			kubectl --kubeconfig '$(KUBECONFIG_PATH)' config set-cluster 'kind-$(CLUSTER)' --server="https://$$ip:6443" >/dev/null; \
			echo "repointed kubeconfig at control-plane IP $$ip:6443"; \
		fi
	@kubectl --kubeconfig '$(KUBECONFIG_PATH)' wait --for=condition=Ready nodes --all --timeout=120s
	@echo "control plane reachable for '$(CLUSTER)'"

kind-down: ## Delete the kind cluster and its kubeconfig
	@kind delete cluster --name '$(CLUSTER)' 2>/dev/null || true
	@rm -f '$(KUBECONFIG_PATH)'

clean: kind-down ## Alias for kind-down

# ---- tests (attach to the kind-up cluster; deploy into NAMESPACE) ----
test-k8s-api-sanity: $(IMAGE_DEP) $(KIND_DEP) ## API sanity: one of each governance entity (NAMESPACE=...)
	$(TEST_ENV) $(GO) test -tags=$(TAGS) -count=$(COUNT) -timeout $(TIMEOUT) -v \
		-run '^TestAPISanity$$' ./scenarios/...

test-k8s-crud-sanity: $(IMAGE_DEP) $(KIND_DEP) ## CRUD lifecycle sanity across all resources (NAMESPACE=...)
	$(TEST_ENV) $(GO) test -tags=$(TAGS) -count=$(COUNT) -timeout $(TIMEOUT) -v \
		-run '^TestAPICRUDSanity$$' ./scenarios/...

test: test-k8s-api-sanity ## Alias for test-k8s-api-sanity

# tests/k8s/Makefile — kind-based API-sanity integration test.
#
# Deploys postgres + mock-llm (helm umbrella chart at deploy/deps) and bifrost
# (serelib-rendered from deploy/bifrost, applied with kubectl), then creates one
# of each governance API entity via the go-bifrost-ai SDK.
#
# Quick start:
#   make test                # build image, run the test, teardown
#   make test KEEP=1         # keep the cluster/namespace for inspection
#   make test SKIP_IMAGE=1   # assume bifrost:local-test already exists
#   make clean               # delete the reusable 'bifrost-test' kind cluster
#
.DEFAULT_GOAL := help

GO        ?= go
TAGS      ?= k8s
TIMEOUT   ?= 30m
COUNT     ?= 1
GOPRIVATE ?= gopkg.volterra.us

REPO_ROOT := $(abspath $(CURDIR)/../..)

# Image build is a prerequisite of the test; bypass with SKIP_IMAGE=1.
IMAGE_DEP := image
ifeq ($(SKIP_IMAGE),1)
  IMAGE_DEP :=
endif

# Friendly make vars -> harness env vars.
TEST_ENV  = GOWORK=off GOPRIVATE=$(GOPRIVATE) BIFROST_K8S_SKIP_BUILD=1
TEST_ENV += $(if $(filter 1,$(KEEP)),BIFROST_K8S_KEEP=1)

.PHONY: help image clean test

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n",$$1,$$2}'

image: ## Build bifrost:local-test from the F5XC-patched source (delegates to root Makefile)
	@$(MAKE) -C $(REPO_ROOT) test-k8s-image

clean: ## Delete the reusable 'bifrost-test' kind cluster
	@$(MAKE) -C $(REPO_ROOT) test-k8s-clean

test: $(IMAGE_DEP) ## Run the API-sanity test (postgres+mock-llm via helm, bifrost via serelib render)
	$(TEST_ENV) $(GO) test -tags=$(TAGS) -count=$(COUNT) -timeout $(TIMEOUT) -v \
		-run '^TestAPISanity$$' ./scenarios/...

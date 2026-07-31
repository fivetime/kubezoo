SHELL := bash

GO ?= go
TARGETS ?= kubezoo clusterresourcequota
WHAT ?= $(TARGETS)
IMAGE_REPO ?= ghcr.io/fivetime
IMAGE_TAG ?= $(shell git describe --tags --always --dirty)
TARGET_PLATFORMS ?= linux/amd64
ENVTEST_K8S_VERSION ?= 1.36.x
SETUP_ENVTEST_VERSION ?= release-0.24

.DEFAULT_GOAL := build

.PHONY: build
build:
	@GIT_VERSION="$(IMAGE_TAG)" bash hack/build.sh $(WHAT)

# Everything here runs without an apiserver now. The two suites that needed one
# left with the packages they belong to: the controller's to kubezoo-controller,
# and the scope-table check to kubezoo-contract, whose vocabulary it checks.
#
# ⚠️ Which means green here says less than it used to. The behaviour this repo is
# actually judged on is in hack/lab, and that needs all three.
.PHONY: test
test:
	@$(GO) test ./...

.PHONY: envtest
envtest:
	@GOBIN="$(CURDIR)/bin" $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

.PHONY: test-race
test-race:
	@$(GO) test -race ./...

.PHONY: format
format:
	@$(GO) fmt ./...

.PHONY: lint
lint:
	@golangci-lint run

.PHONY: codegen
codegen:
	@bash hack/make-rules/codegen.sh

.PHONY: verify-codegen
verify-codegen:
	@bash hack/make-rules/codegen.sh --verify

.PHONY: code-gen client-gen
code-gen: codegen
client-gen: codegen

.PHONY: docker-build
docker-build:
	@docker buildx build --load --platform $(TARGET_PLATFORMS) \
		--build-arg GIT_VERSION="$(IMAGE_TAG)" \
		-f build/kubezoo.Dockerfile \
		-t $(IMAGE_REPO)/kubezoo:$(IMAGE_TAG) .
	@docker buildx build --load --platform $(TARGET_PLATFORMS) \
		--build-arg GIT_VERSION="$(IMAGE_TAG)" \
		-f build/clusterresourcequota.Dockerfile \
		-t $(IMAGE_REPO)/clusterresourcequota:$(IMAGE_TAG) .

.PHONY: local-up
local-up:
	@bash hack/make-rules/local_up.sh

.PHONY: clean
clean:
	@rm -rf -- _output bin cover.out coverage.out

.PHONY: help
help:
	@echo "Targets: build, test-unit, test-integration, test, test-race, format, lint, codegen, verify-codegen, docker-build, local-up, clean"

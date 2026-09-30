.PHONY: build test integration generate fmt vet check manifests

build:
	CGO_ENABLED=0 go build -trimpath -o bin/infra-smoketest ./cmd/infra-smoketest

test:
	go test -race -count=1 ./...

integration:
	go test -race -count=1 -tags=integration ./internal/controller/...

generate:
	controller-gen object paths=./api/...
	controller-gen crd:crdVersions=v1,generateEmbeddedObjectMeta=true paths=./api/... output:crd:dir=config/crd

fmt:
	gofmt -w api cmd internal

vet:
	go vet ./...

manifests:
	kustomize build config/default >/dev/null
	kustomize build examples >/dev/null

check: vet test integration manifests
	@test -z "$$(gofmt -l api cmd internal)"
	actionlint
	shellcheck scripts/*.sh

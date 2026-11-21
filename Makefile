.PHONY: all generate test lint build image manifests

all: generate lint test build

# Deepcopy code, CRD and RBAC from the API types and +kubebuilder markers.
generate:
	go tool controller-gen object paths=./api/...
	go tool controller-gen crd rbac:roleName=tender paths=./... \
		output:crd:artifacts:config=config/crd output:rbac:artifacts:config=config/rbac

test:
	go test -race ./...

lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...

build:
	CGO_ENABLED=0 go build -o bin/tender ./cmd

image:
	docker build -t tender:dev .

# Render the install manifests.
manifests:
	kubectl kustomize config/default

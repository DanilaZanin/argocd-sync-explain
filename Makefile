GOTESTFLAGS ?= -race -count=1
VERSION     ?= dev
BIN         := bin/argocd-sync-explain
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: build vet lint test e2e e2e-full snapshot clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/argocd-sync-explain

vet:
	go vet ./...
	go vet -tags e2e ./test/e2e

lint:
	golangci-lint run ./...
	golangci-lint run --build-tags e2e ./test/e2e/...

test:
	go test $(GOTESTFLAGS) ./...

# Small scenario set on the current Argo CD version (one kind cluster).
e2e: build
	test/e2e/run.sh current small

# Every scenario on the current and the previous Argo CD version (one clean cluster each).
e2e-full: build
	test/e2e/run.sh all full

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist

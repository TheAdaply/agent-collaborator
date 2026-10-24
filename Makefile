VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/TheAdaply/agent-collaborator/internal/node.Version=$(VERSION)

.PHONY: build test e2e vet clean
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/agent-collaborator ./cmd/agent-collaborator
vet:
	gofmt -l . | (! grep .) && go vet ./...
test: vet build
	go test -count=1 ./...
	tests/e2e.sh bin/agent-collaborator
clean:
	rm -rf bin

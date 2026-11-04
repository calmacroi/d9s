VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install run vet e2e clean

build:
	go build -ldflags "$(LDFLAGS)" -o d9s .

install:
	go install -ldflags "$(LDFLAGS)" .
	@echo "installed to $$(go env GOBIN || true)$$( [ -z "$$(go env GOBIN)" ] && echo "$$(go env GOPATH)/bin")/d9s"

run: build
	./d9s

vet:
	gofmt -l . && go vet ./... && go vet -tags e2e ./...

# Drives the real UI against the local daemon using throwaway d9s-e2e* resources.
e2e:
	./e2e/run.sh

clean:
	rm -f d9s

REPO_ROOT:=${CURDIR}
OUT_DIR=$(REPO_ROOT)/bin

# Go build settings
GO111MODULE=on
CGO_ENABLED=0
export GO111MODULE CGO_ENABLED

.PHONY: all build 
build: 
	@echo "Building all binaries..."
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o ./bin/krun .

clean:
	rm -rf "$(OUT_DIR)/"

test:
	CGO_ENABLED=1 go test -short -v -race -count 1 ./...

lint:
	hack/lint.sh

update:
	go mod tidy

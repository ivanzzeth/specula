# Specula — build orchestration
BINARY := specula
PKG := ./cmd/specula

.PHONY: all ui build test vet fmt run clean

all: build

## ui: build the embedded WebUI into web/dist (required before `build`)
ui:
	cd web && npm install && npm run build

## build: build the WebUI then the single static binary (WebUI embedded)
build: ui
	CGO_ENABLED=0 go build -o bin/$(BINARY) $(PKG)

## build-go: build only the Go binary (assumes web/dist already built)
build-go:
	CGO_ENABLED=0 go build -o bin/$(BINARY) $(PKG)

## test: run all Go tests
test:
	go test -count=1 ./...

## test-race: run tests with the race detector
test-race:
	go test -count=1 -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

## run: build and run with the example config
run: build
	./bin/$(BINARY) --config specula.example.yaml

clean:
	rm -rf bin web/dist/assets web/dist/index.html

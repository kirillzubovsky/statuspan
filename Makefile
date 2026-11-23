.PHONY: build test vet fmt

build:
	go build -o statuspan ./cmd/statuspan

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

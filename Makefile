BINARY := raspberry-pi-cloudflared-upgrade
OUTPUT := build

.PHONY: build build-all test check install clean

## build: build for this machine → build/raspberry-pi-cloudflared-upgrade
build:
	go build -trimpath -o $(OUTPUT)/$(BINARY) .

## build-all: cross-build for Linux x86-64, Raspberry Pi (arm64) and Windows
build-all:
	GOOS=linux   GOARCH=amd64 go build -trimpath -o $(OUTPUT)/$(BINARY)-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build -trimpath -o $(OUTPUT)/$(BINARY)-linux-arm64 .
	GOOS=windows GOARCH=amd64 go build -trimpath -o $(OUTPUT)/$(BINARY)-windows-amd64.exe .

## test: run the unit tests (no Raspberry Pi needed; a fake Pi is simulated)
test:
	go test ./...

## check: format check + vet + tests
check:
	@test -z "$$(gofmt -l .)" || (echo "run gofmt -w ." && gofmt -l . && exit 1)
	go vet ./...
	go test ./...

## install: build, then copy into $(go env GOPATH)/bin as raspberry-pi-cloudflared-upgrade
install: build
	install -D -m 755 $(OUTPUT)/$(BINARY) "$$(go env GOPATH)/bin/$(BINARY)"

clean:
	rm -rf $(OUTPUT)

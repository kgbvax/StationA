.PHONY: run build build-linux vet clean

run: build
	./testui -config config.toml

build:
	GOWORK=off go build -o testui ./cmd/testui

build-linux:
	GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/testui-linux-arm64 ./cmd/testui

vet:
	GOWORK=off go vet ./cmd/testui

clean:
	rm -f testui bin/testui dist/testui-linux-arm64

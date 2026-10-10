.PHONY: build test candidates

build:
	CGO_ENABLED=0 go build -trimpath -o dist/aimount ./cmd/aimount

test:
	go test ./...
	go test -race ./...
	go vet ./...

candidates:
	sh scripts/build-candidates.sh

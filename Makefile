# The UI is embedded in the binary, so build it first.
.PHONY: build ui sqlc proto test

build: ui
	go build -o bin/hotbed ./cmd/hotbed

ui:
	go generate ./ui

sqlc:
	sqlc generate

proto:
	go tool buf generate

test:
	go test -race ./...
	pnpm -C ui test
	pnpm -C ui typecheck

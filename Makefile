# The UI is embedded in the binary, so build it first.
.PHONY: build ui sqlc proto skills test dev-cockpit

build: ui
	go build -o bin/hotbed ./cmd/hotbed

dev-cockpit:
	go run ./cmd/dev-cockpit

ui:
	go generate ./ui

sqlc:
	sqlc generate

proto:
	go tool buf generate

skills:
	go generate ./skills

test:
	go test -race ./...
	pnpm -C ui test
	pnpm -C ui typecheck

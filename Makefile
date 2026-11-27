# The hotbed binary does not embed the UI: the cf-controller Worker serves ui/dist.
.PHONY: build ui sqlc proto skills test dev-cockpit

build:
	go build -o bin/hotbed ./cmd/hotbed

dev-cockpit:
	go run ./cmd/dev-cockpit

ui:
	pnpm -C ui install --frozen-lockfile
	pnpm -C ui build

sqlc:
	sqlc generate

proto:
	go tool buf generate

skills:
	go generate ./skills

test:
	pnpm -C cf-controller run build
	go test -race ./...
	pnpm -C cf-controller run check
	pnpm -C cf-controller test
	pnpm -C ui test
	pnpm -C ui typecheck

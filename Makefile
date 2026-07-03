.PHONY: help run build migrate-up migrate-down migrate-status migrate-create sqlc seed lint vet fmt test tidy

# Load .env for local targets if present.
ifneq (,$(wildcard .env))
include .env
export
endif

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

run: ## Run the API server
	go run ./cmd/api

build: ## Build the API binary to ./bin/api
	go build -o bin/api ./cmd/api

migrate-up: ## Apply all pending migrations
	go run ./cmd/migrate up

migrate-down: ## Roll back the last migration
	go run ./cmd/migrate down

migrate-status: ## Show migration status
	go run ./cmd/migrate status

migrate-create: ## Create a new migration: make migrate-create name=add_foo
	goose -dir db/migrations create $(name) sql

sqlc: ## Generate typed Go from db/queries
	sqlc generate

seed: ## Create the initial super admin (uses SEED_ADMIN_* env vars)
	go run ./cmd/seed

fmt: ## Format code
	gofmt -w .

vet: ## Run go vet
	go vet ./...

lint: vet ## Alias for vet (add golangci-lint later if desired)

test: ## Run tests
	go test ./...

tidy: ## Tidy modules
	go mod tidy

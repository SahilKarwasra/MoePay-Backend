PG_BIN ?= /opt/homebrew/opt/postgresql@17/bin
TEST_DATABASE_URL ?= postgres://localhost:5432/moepay_test?sslmode=disable

.PHONY: setup db migrate run dev lint build

setup: ## Install local services with Homebrew (no Docker)
	brew install postgresql@17 redis
	brew services start postgresql@17
	brew services start redis

db: ## Create dev and test databases
	$(PG_BIN)/createdb moepay || true
	$(PG_BIN)/createdb moepay_test || true

migrate: ## Apply schema migrations to DATABASE_URL
	go run ./cmd/migrate up

run: ## Run the API
	go run ./cmd/api

dev: ## Run the API with live reload (go install github.com/air-verse/air@latest)
	air

build:
	go build -o bin/api ./cmd/api
	go build -o bin/migrate ./cmd/migrate

lint:
	go vet ./...
	gofmt -l .

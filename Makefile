DB_URL      ?= postgres://verdict:verdict@localhost:5433/verdict?sslmode=disable
TEST_DB_URL ?= postgres://verdict:verdict@localhost:5433/verdict_test?sslmode=disable

.PHONY: build test test-short db-up db-down migrate eod2-sync

build:
	go build -o bin/algo ./cmd/algo

test: ## integration + unit; needs `make db-up`
	VERDICT_TEST_DATABASE_URL=$(TEST_DB_URL) go test ./...

test-short: ## unit tests only, no database
	go test -short ./...

db-up:
	docker compose up -d postgres
	@until docker compose exec -T postgres pg_isready -U verdict -d verdict >/dev/null 2>&1; do sleep 1; done
	@echo "postgres ready on localhost:5433"

db-down:
	docker compose down

migrate:
	go run ./cmd/algo migrate --database-url "$(DB_URL)"

eod2-sync:
	./scripts/eod2-sync.sh

GREEN     := $(shell tput -Txterm setaf 2)
YELLOW    := $(shell tput -Txterm setaf 3)
RESET     := $(shell tput -Txterm sgr0)
GO        ?= go
GOBIN_VPS ?= /usr/local/bin

compose := docker compose

all: help

help: ## Show this help.
	@echo ''
	@echo 'Usage:'
	@echo '  ${YELLOW}make${RESET} ${GREEN}<target>${RESET}'
	@echo ''
	@echo 'Targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  ${YELLOW}%-24s${GREEN}%s${RESET}\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the Docker images.
	$(compose) build

build-cli: ## Build the agent-mem CLI into ./bin/agent-mem. Override GO=/path/to/go if needed.
	mkdir -p bin
	$(GO) build -o ./bin/agent-mem ./cmd/agent-mem

install-cli: ## Install the agent-mem CLI. Override with GO=/path/to/go and GOBIN=/path, e.g. /usr/local/bin.
ifdef GOBIN
	GOBIN="$(GOBIN)" $(GO) install ./cmd/agent-mem
else
	$(GO) install ./cmd/agent-mem
endif

install-cli-vps: ## Install agent-mem system-wide via sudo (preserves PATH so go is found). Override GOBIN_VPS=/path (default /usr/local/bin).
	sudo env "PATH=$$PATH" GOBIN="$(GOBIN_VPS)" $(GO) install ./cmd/agent-mem

up: ## Start all services.
	$(compose) up -d

down: ## Stop all services.
	$(compose) down

status: ## Show service status.
	$(compose) ps

logs: ## Show worker logs.
	$(compose) logs -f worker

migrate: up ## Run all pending database migrations.
	$(compose) exec worker agent-mem migrate

migrate-create: ## Create a new migration file. Usage: make migrate-create name=add_column_to_table
	$(GO) run cmd/agent-mem/main.go migrate-create $(name)

migrate-status: up ## Show migration status.
	$(compose) exec worker agent-mem migrate-status

migrate-rollback: up ## Rollback last migration. Usage: make migrate-rollback version=20260323000000
	$(compose) exec worker agent-mem migrate-rollback -v $(version)

migrate-up-by-one: up ## Apply the next pending migration.
	$(compose) exec worker agent-mem migrate-up-by-one

migrate-fix: up ## Force-delete a failed migration record. Usage: make migrate-fix version=20260323000000
	$(compose) exec worker agent-mem migrate-fix -v $(version)

restart: ## Rebuild and restart worker.
	$(compose) up -d --build worker

db-reset: ## Clear the database and re-run migrations.
	$(compose) down -v
	$(compose) up -d
	@echo "Waiting for postgres..."
	@sleep 5
	$(compose) exec worker agent-mem migrate

.PHONY: all help build build-cli install-cli install-cli-vps up down status logs migrate migrate-create migrate-status migrate-rollback migrate-up-by-one migrate-fix restart db-reset

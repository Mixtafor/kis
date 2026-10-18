-include .env
export

.PHONY: help up down logs build run test migrate seed

help:
	@awk 'BEGIN {FS = ":.*## "} /^[a-z_-]+:.*## / {printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

up: ## Собрать и запустить БД, миграции и сервер
	docker compose up -d --build --wait --wait-timeout 120 app

down:
	docker compose down --remove-orphans

logs: ## Смотреть логи сервера и БД
	docker compose logs -f --tail=100 app db

build: ## Собрать все Go-команды в bin/
	go build -trimpath -o bin/ ./cmd/...

run: ## Запустить сервер на хосте с настройками .env
	go run ./cmd/server

test: ## Запустить unit-тесты с race detector
	go test -race -count=1 ./...

migrate: ## Миграции
	docker compose run --build --rm migrate up

seed: ## Добавить демонстрационных корреспондентов
	docker compose --profile tools run --rm seed

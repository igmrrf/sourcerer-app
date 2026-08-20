# Makefile commands for easy execution
start-dev:
	docker compose -f docker-compose.local.yml up -d --build

restart-dev:
	docker compose down && docker compose -f docker-compose.local.yml up -d --build

start:
	docker compose -f docker-compose.yml up -d --build

restart:
	docker compose down && docker compose -f docker-compose.yml up -d --build

build-cli:
	./build_cli.sh

generate-ssl:
	./generate_local_ssl.sh

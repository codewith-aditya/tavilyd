GO            ?= go
VERSION       ?= 1.0.0
BINARY        ?= bin/server
IMAGE         ?= tavilyd:$(VERSION)
COMPOSE       ?= docker compose
GOLANGCI      ?= golangci-lint

.PHONY: all build run test test-race bench vet lint fmt tidy docker-build up down restart logs ps health search extract secret clean ci

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false \
		-ldflags "-s -w -X main.buildVersion=$(VERSION)" \
		-o $(BINARY) ./cmd/server

run: build
	./$(BINARY)

test:
	$(GO) test ./...

test-race:
	$(GO) test -race -count=1 ./...

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/rerank ./internal/search

vet:
	$(GO) vet ./...

lint:
	$(GOLANGCI) run ./...

fmt:
	gofmt -s -w $$(find . -name '*.go' -not -path './vendor/*')

tidy:
	$(GO) mod tidy

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

restart:
	$(COMPOSE) restart api

logs:
	$(COMPOSE) logs -f api

ps:
	$(COMPOSE) ps

health:
	curl -fsS http://localhost:$${PUBLISH_PORT:-8080}/health | python3 -m json.tool

search:
	curl -fsS -X POST http://localhost:$${PUBLISH_PORT:-8080}/search \
		-H 'Content-Type: application/json' \
		-H "Authorization: Bearer $${API_KEY}" \
		-d '{"query":"golang context cancellation","search_depth":"basic","max_results":3}' | python3 -m json.tool

extract:
	curl -fsS -X POST http://localhost:$${PUBLISH_PORT:-8080}/extract \
		-H 'Content-Type: application/json' \
		-H "Authorization: Bearer $${API_KEY}" \
		-d '{"urls":["https://go.dev/blog/context"],"format":"markdown"}' | python3 -m json.tool

secret:
	openssl rand -hex 32

clean:
	rm -rf bin coverage.out

ci: vet test-race

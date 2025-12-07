COMPOSE_FILE := compose.yaml
GATEWAY_HOST_PORT ?= 18080
GATEWAY_URL ?= http://localhost:$(GATEWAY_HOST_PORT)
EXAMPLES_MODE ?= basic

.PHONY: gateway-build gateway-up gateway-down gateway-logs gateway-restart \
        worker-build worker-up worker-down worker-logs worker-restart \
        local-up local-down local-logs clean \
        test-stream test-non-stream test-models test-stats health-gateway

gateway-build:
	docker compose -f $(COMPOSE_FILE) --profile gateway build gateway

gateway-up:
	docker compose -f $(COMPOSE_FILE) --profile gateway up -d gateway

gateway-down:
	docker compose -f $(COMPOSE_FILE) --profile gateway down

gateway-logs:
	docker compose -f $(COMPOSE_FILE) --profile gateway logs -f gateway

gateway-restart: gateway-down gateway-up

worker-build:
	docker compose -f $(COMPOSE_FILE) --profile worker build worker

worker-up:
	docker compose -f $(COMPOSE_FILE) --profile worker up -d worker redis

worker-down:
	docker compose -f $(COMPOSE_FILE) --profile worker down

worker-logs:
	docker compose -f $(COMPOSE_FILE) --profile worker logs -f worker

worker-restart: worker-down worker-up

local-up:
	docker compose -f $(COMPOSE_FILE) --profile gateway --profile worker up -d

local-down:
	docker compose -f $(COMPOSE_FILE) --profile gateway --profile worker down

local-logs:
	docker compose -f $(COMPOSE_FILE) --profile gateway --profile worker logs -f

clean:
	docker compose -f $(COMPOSE_FILE) --profile gateway --profile worker down -v
	docker system prune -f

test-stream:
	curl -X POST http://localhost:$(GATEWAY_HOST_PORT)/v1/chat/completions \
		-H "Content-Type: application/json" \
		-d '{"model":"gpt-oss:20b","messages":[{"role":"user","content":"Hello"}],"stream":true}'

test-non-stream:
	curl -X POST http://localhost:$(GATEWAY_HOST_PORT)/v1/chat/completions \
		-H "Content-Type: application/json" \
		-d '{"model":"gpt-oss:20b","messages":[{"role":"user","content":"Hello"}],"stream":false}'

test-models:
	curl http://localhost:$(GATEWAY_HOST_PORT)/v1/models

test-stats:
	curl http://localhost:$(GATEWAY_HOST_PORT)/v1/stats

health-gateway:
	curl http://localhost:$(GATEWAY_HOST_PORT)/health

uv-run-examples:
	NO_PROXY=localhost,127.0.0.1,::1 no_proxy=localhost,127.0.0.1,::1 BASE_URL=$(GATEWAY_URL) EXAMPLES_MODE=$(EXAMPLES_MODE) uv run examples/use_ollama_with_proxy.py

uv-run-openai:
	NO_PROXY=localhost,127.0.0.1,::1 no_proxy=localhost,127.0.0.1,::1 BASE_URL=$(GATEWAY_URL) uv run --extra openai examples/use_ollama_with_proxy.py

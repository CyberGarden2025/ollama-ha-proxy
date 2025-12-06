# Двухступенчатый прокси для Ollama

Реализация устойчивого к разрывам связи прокси-слоя для Ollama с OpenAI-совместимым API.

## Архитектура

```
Клиент → Gateway (Frontend) → Worker (Backend) → Ollama
         [Сервер 1]              [Сервер 2]
```

### Особенности

- **Gateway (Frontend)**: OpenAI API compatible интерфейс (`/v1/chat/completions`, `/v1/models`)
- **Worker (Backend)**: Job-based API с буферизацией стрима в Redis
- **Устойчивость**: Автоматические retry с exponential backoff при разрывах связи
- **Буферизация**: Все чанки сохраняются в Redis, можно читать с любой позиции
- **Rate Limiting**: Ограничение параллельных запросов к Ollama с очередью и отклонением избыточных запросов

## Структура проекта

```
.
├── gateway/
│   ├── main.go
│   ├── go.mod
│   ├── go.sum
│   └── Dockerfile
├── worker/
│   ├── main.go
│   ├── go.mod
│   ├── go.sum
│   └── Dockerfile
├── compose.yaml
├── Makefile
└── README.md
```

## Быстрый старт

### Предварительные требования

- Docker и Docker Compose
- Ollama запущена на порту 11434

### Развертывание по отдельным серверам

**Сервер Worker (backend + Redis):**

```bash
make worker-build
make worker-up
make worker-logs
```

**Сервер Gateway (frontend):**

```bash
make gateway-build
BACKEND_PROXY_URL=http://192.168.1.218:5345 make gateway-up
make gateway-logs
```

### Локальный запуск всего стека (dev)

```bash
make local-up
# остановить
make local-down
```

`compose.yaml` использует профили `gateway` и `worker`, поэтому команды Makefile запускают только нужные сервисы на выбранной машине.

> Если порт 5345 или 18080 на хосте занят, задайте другие значения перед запуском, например:
> `WORKER_HOST_PORT=15345 GATEWAY_HOST_PORT=18081 make local-up`

### Конфигурация

#### Worker (Backend)

Переменные окружения:

- `REDIS_URL` - URL Redis (по умолчанию: `redis://localhost:6379/0`)
- `OLLAMA_BASE_URL` - URL Ollama (по умолчанию: `http://192.168.1.218:11434` — для текущего сервера worker)
- `WORKER_CONCURRENCY` - количество воркеров (по умолчанию: `10`)
- `JOB_TTL_SECONDS` - время жизни jobs (по умолчанию: `86400`)
- `PORT` - порт сервиса (по умолчанию: `5345`)

> Если Ollama запущена на хосте и контейнер не может резолвить `host.docker.internal`, задайте `OLLAMA_BASE_URL` с явным IP хоста (например `http://192.168.1.10:11434`) или убедитесь, что в compose есть `extra_hosts: ["host.docker.internal:host-gateway"]` для сервиса worker.

#### Gateway (Frontend)

Переменные окружения:

- `BACKEND_PROXY_URL` - URL Worker (по умолчанию: `http://localhost:5345`)
- `POLL_INTERVAL_MS` - интервал опроса (по умолчанию: `500`)
- `RETRY_BACKOFF_INIT_MS` - начальная задержка retry (по умолчанию: `1000`)
- `RETRY_BACKOFF_MAX_MS` - максимальная задержка retry (по умолчанию: `30000`)
- `JOB_TIMEOUT_MS` - общий таймаут job (по умолчанию: `1800000`)
- `OPENAI_API_KEY_REQUIRED` - требовать API ключ (по умолчанию: `false`)
- `OPENAI_API_KEY` - API ключ для проверки
- `PORT` - порт сервиса (по умолчанию: `8080`, внутри контейнера; хост-порт по умолчанию: `18080`)

## API

### OpenAI Compatible Endpoints (Gateway)

#### POST /v1/chat/completions

Создание chat completion (с поддержкой streaming).

**Request:**

```json
{
  "model": "gpt-oss:20b",
  "messages": [
    {"role": "user", "content": "Hello!"}
  ],
  "stream": true,
  "temperature": 0.7,
  "max_tokens": 100
}
```

**Response (stream: true):** Server-Sent Events

```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1234567890,"model":"gpt-oss:20b","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: [DONE]
```

**Response (stream: false):** JSON

```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "gpt-oss:20b",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "Hello!"},
    "finish_reason": "stop"
  }]
}
```

#### GET /v1/models

Список доступных моделей.

**Response:**

```json
{
  "object": "list",
  "data": [
    {"id": "gpt-oss:20b", "object": "model", "created": 1234567890, "owned_by": "ollama"},
    {"id": "gpt-oss:120b", "object": "model", "created": 1234567890, "owned_by": "ollama"}
  ]
}
```

#### GET /v1/stats

Статистика загрузки backend воркеров.

**Response:**

```json
{
  "active": 2,
  "queued": 1,
  "capacity": 3,
  "max_queue": 6
}
```

### Job Management API (Worker)

#### POST /jobs

Создание новой задачи генерации.

**Request:**

```json
{
  "model": "gpt-oss:20b",
  "messages": [{"role": "user", "content": "Hello"}],
  "options": {"temperature": 0.7}
}
```

**Response:**

```json
{
  "job_id": "uuid",
  "status": "queued"
}
```

#### GET /jobs/{job_id}/events?from_seq=N

Получение событий (чанков) с заданной позиции.

**Response:**

```json
{
  "status": "running",
  "chunks": [
    {"seq": 1, "delta": "Hello", "done": false},
    {"seq": 2, "delta": "!", "done": true, "finish_reason": "stop"}
  ]
}
```

#### GET /jobs/{job_id}/status

Получение статуса задачи.

**Response:**

```json
{
  "status": "completed",
  "created_at": "2024-01-01T12:00:00Z",
  "completed_at": "2024-01-01T12:00:30Z"
}
```

#### POST /jobs/{job_id}/cancel

Отмена задачи.

#### GET /stats

Статистика воркеров.

**Response:**

```json
{
  "active": 2,
  "queued": 1,
  "capacity": 3,
  "max_queue": 6
}
```

## Тестирование

### Streaming запрос

```bash
make test-stream
```

Или вручную:

```bash
curl -X POST http://localhost:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-oss:20b",
    "messages": [{"role": "user", "content": "Расскажи короткую историю"}],
    "stream": true
  }'
```

### Non-streaming запрос

```bash
make test-non-stream
```

### Список моделей

```bash
make test-models
```

### Получение статистики загрузки

```bash
# Статистика воркеров Worker
curl http://localhost:18080/v1/stats

# Ответ:
# {
#   "active": 2,      // Активно обрабатываются
#   "queued": 1,      // В очереди
#   "capacity": 3,    // Максимум параллельных
#   "max_queue": 6    // Размер очереди (2 * capacity)
# }
```

## Как это работает

### Контроль параллельности и Rate Limiting

Ollama обрабатывает запросы параллельно без встроенной очереди. Worker реализует механизм контроля:

**Параметр `WORKER_CONCURRENCY`** определяет:
- **N активных обработчиков**: Максимум N запросов одновременно отправляется в Ollama
- **Очередь размером N**: Дополнительные N запросов ожидают в очереди
- **Отклонение после 2N**: Запросы сверх лимита (активные + в очереди) получают HTTP 429

**Пример с WORKER_CONCURRENCY=3:**
- Запросы 1-3: Обрабатываются немедленно (активные)
- Запросы 4-6: Помещаются в очередь (ожидают)
- Запрос 7+: Отклоняется с ошибкой `429 Too Many Requests`

**Рекомендации по настройке:**
- Для моделей 7B-20B: `WORKER_CONCURRENCY=3-5`
- Для моделей 70B-120B: `WORKER_CONCURRENCY=1-2`
- Зависит от доступной VRAM и производительности

### Успешный сценарий с нестабильной сетью

1. Клиент отправляет запрос на Gateway
2. Gateway создает job на Worker
3. Worker запускает воркер, который стримит из Ollama
4. Все чанки сохраняются в Redis с монотонным `seq`
5. Gateway опрашивает Worker каждые 500ms
6. При разрыве связи Gateway ↔ Worker:
   - Gateway делает retry с экспоненциальным backoff
   - SSE стрим клиенту НЕ закрывается
   - Чанки продолжают накапливаться в Redis
7. После восстановления связи:
   - Gateway запрашивает чанки с последнего `seq`
   - Стрим возобновляется для клиента
8. При завершении генерации:
   - Последний чанк имеет `done: true`
   - Gateway отправляет `[DONE]` и закрывает SSE

### Обработка ошибок

- **Падение Ollama**: Worker записывает финальный чанк с `finish_reason: "error"`
- **Превышение таймаута**: Gateway закрывает SSE с ошибкой после `JOB_TIMEOUT_MS`
- **Сетевые ошибки**: Автоматические retry до максимального backoff
- **Перегрузка системы**: HTTP 429 с сообщением `Service overloaded`

## Мониторинг

### Логи

```bash
# Все сервисы (dev-профили gateway+worker)
make local-logs

# Только Gateway
make gateway-logs

# Только Worker
make worker-logs
```

### Метрики

Логи содержат:
- Создание/завершение jobs
- Ошибки подключения
- Время обработки
- Статусы retry

## Производительность

### Целевые метрики

- **Задержка**: < 500ms между появлением чанка и доставкой клиенту
- **Одновременные jobs**: Настраивается через `WORKER_CONCURRENCY`
- **Размер промпта**: до 4K токенов

### Рекомендации по WORKER_CONCURRENCY

Выбор значения зависит от размера модели и доступных ресурсов:

| Модель        | VRAM на инференс | Рекомендуемое значение |
|---------------|------------------|------------------------|
| 7B параметров | ~8GB            | 4-6                    |
| 13B-20B       | ~16GB           | 2-4                    |
| 70B           | ~48GB           | 1-2                    |
| 120B+         | ~80GB+          | 1                      |

**Примеры конфигурации:**

```yaml
# Для gpt-oss:20b на сервере с 32GB VRAM
environment:
  - WORKER_CONCURRENCY=3

# Для gpt-oss:120b на сервере с 96GB VRAM
environment:
  - WORKER_CONCURRENCY=1
```

**Как определить оптимальное значение:**
1. Запустите с `WORKER_CONCURRENCY=1`
2. Отправьте тестовый запрос и замерьте использование VRAM
3. Разделите доступную VRAM на VRAM одного запроса
4. Установите значение с запасом 20-30% для стабильности

### Масштабирование

#### Горизонтальное

Worker можно масштабировать горизонтально:

```yaml
worker:
  deploy:
    replicas: 3
```

Все инстансы используют общий Redis для координации.

#### Вертикальное

Увеличьте `WORKER_CONCURRENCY` для обработки большего количества параллельных jobs.

## Безопасность

### API ключи

Включите проверку API ключей:

```bash
export OPENAI_API_KEY_REQUIRED=true
export OPENAI_API_KEY=sk-your-secret-key
```

Клиент должен передавать:

```bash
curl -H "Authorization: Bearer sk-your-secret-key" ...
```

### TLS

Для production используйте reverse proxy (nginx/traefik) с TLS:

```nginx
server {
    listen 443 ssl;
    server_name your-domain.com;
    
    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;
    
    location / {
        proxy_pass http://gateway:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_buffering off;
    }
}
```

## Устранение неполадок

### Gateway не может подключиться к Worker

Проверьте `BACKEND_PROXY_URL` и доступность сети:

```bash
docker compose -f compose.yaml --profile gateway exec gateway wget -O- http://worker:5345/health
```

### Worker не может подключиться к Ollama

Проверьте `OLLAMA_BASE_URL`:

```bash
docker compose -f compose.yaml --profile worker exec worker wget -O- http://192.168.1.218:11434/api/tags
```

### Redis ошибки

Проверьте подключение:

```bash
docker compose -f compose.yaml --profile worker exec redis redis-cli ping
```

### Высокая задержка

- Уменьшите `POLL_INTERVAL_MS` (баланс между задержкой и нагрузкой)
- Увеличьте `WORKER_CONCURRENCY` (если позволяют ресурсы)
- Проверьте сетевую задержку между серверами

### Запросы отклоняются с 429

Это означает перегрузку системы:
- Увеличьте `WORKER_CONCURRENCY` если есть свободная VRAM
- Реализуйте retry logic на стороне клиента
- Рассмотрите горизонтальное масштабирование Worker

### Мониторинг загрузки

Проверяйте статистику регулярно:

```bash
watch -n 1 'curl -s http://localhost:18080/v1/stats | jq'
```

Алерты на:
- `active == capacity` - система работает на максимуме
- `queued > capacity/2` - высокая нагрузка
- Частые HTTP 429 - нужно масштабирование

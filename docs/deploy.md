# Production Deployment Guide

## Рекомендации по развертыванию в production

### 1. Настройка WORKER_CONCURRENCY

**Критически важный параметр** для стабильной работы.

#### Расчет значения

```bash
# Шаг 1: Измерьте VRAM для одного запроса
# Запустите с WORKER_CONCURRENCY=1
docker compose -f compose.yaml --profile worker exec worker nvidia-smi

# Шаг 2: Запишите базовое использование VRAM (без нагрузки)
BASE_VRAM=8GB

# Шаг 3: Отправьте тестовый запрос
curl -X POST http://localhost:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-oss:20b","messages":[{"role":"user","content":"Test"}],"stream":false}'

# Шаг 4: Измерьте VRAM под нагрузкой
docker compose -f compose.yaml --profile worker exec worker nvidia-smi
LOADED_VRAM=20GB

# Шаг 5: Вычислите VRAM на запрос
PER_REQUEST_VRAM = LOADED_VRAM - BASE_VRAM = 12GB

# Шаг 6: Вычислите максимум с запасом 30%
TOTAL_VRAM=32GB
MAX_CONCURRENT = (TOTAL_VRAM * 0.7) / PER_REQUEST_VRAM = 1.86 ≈ 1

# Установите WORKER_CONCURRENCY=1
```

#### Таблица рекомендаций

| GPU             | VRAM  | Model Size | Рекомендуемое значение |
|-----------------|-------|------------|------------------------|
| RTX 3090        | 24GB  | 7B-13B     | 3-4                    |
| RTX 4090        | 24GB  | 7B-13B     | 3-4                    |
| A100 40GB       | 40GB  | 20B-30B    | 2-3                    |
| A100 80GB       | 80GB  | 70B        | 1-2                    |
| H100            | 80GB  | 70B-120B   | 1-2                    |
| 2x A100 80GB    | 160GB | 120B+      | 2-3                    |

### 2. Настройка Redis

#### Оптимизация производительности

```conf
# redis.conf

# Увеличить максимум подключений
maxclients 10000

# Настроить memory policy
maxmemory 4gb
maxmemory-policy allkeys-lru

# Persistence для надежности
save 900 1
save 300 10
save 60 10000

# AOF для дополнительной надежности
appendonly yes
appendfsync everysec
```

#### Docker Compose с настройками

```yaml
redis:
  image: redis:7-alpine
  command: >
    redis-server
    --maxmemory 4gb
    --maxmemory-policy allkeys-lru
    --appendonly yes
    --appendfsync everysec
  volumes:
    - redis_data:/data
  deploy:
    resources:
      limits:
        memory: 5G
```

### 3. Мониторинг и алерты

#### Prometheus метрики

Добавьте endpoints для экспорта метрик:

```go
// worker/metrics.go
func (s *Server) PrometheusMetrics(w http.ResponseWriter, r *http.Request) {
    stats := s.worker.GetStats()
    
    fmt.Fprintf(w, "# HELP proxy_active_jobs Current active jobs\n")
    fmt.Fprintf(w, "# TYPE proxy_active_jobs gauge\n")
    fmt.Fprintf(w, "proxy_active_jobs %d\n", stats["active"])
    
    fmt.Fprintf(w, "# HELP proxy_queued_jobs Current queued jobs\n")
    fmt.Fprintf(w, "# TYPE proxy_queued_jobs gauge\n")
    fmt.Fprintf(w, "proxy_queued_jobs %d\n", stats["queued"])
    
    fmt.Fprintf(w, "# HELP proxy_capacity Worker capacity\n")
    fmt.Fprintf(w, "# TYPE proxy_capacity gauge\n")
    fmt.Fprintf(w, "proxy_capacity %d\n", stats["capacity"])
}
```

#### Grafana Dashboard

Важные метрики для отслеживания:

- **Active Jobs / Capacity**: Утилизация воркеров
- **Queue Length**: Длина очереди ожиданий
- **HTTP 429 Rate**: Частота отклонений запросов
- **Job Duration P50/P95/P99**: Латентность обработки
- **Redis Memory Usage**: Использование памяти

#### AlertManager правила

```yaml
groups:
  - name: ollama_proxy
    interval: 30s
    rules:
      - alert: HighUtilization
        expr: proxy_active_jobs / proxy_capacity > 0.9
        for: 5m
        annotations:
          summary: "Proxy at 90%+ capacity"
          
      - alert: QueueBuildup
        expr: proxy_queued_jobs > proxy_capacity
        for: 2m
        annotations:
          summary: "Queue size exceeds capacity"
          
      - alert: HighRejectionRate
        expr: rate(http_requests_total{code="429"}[5m]) > 10
        for: 1m
        annotations:
          summary: "High rate of 429 responses"
```

### 4. Балансировка нагрузки

#### Nginx конфигурация

```nginx
upstream gateway_backend {
    least_conn;
    server gateway-1:8080 max_fails=3 fail_timeout=30s;
    server gateway-2:8080 max_fails=3 fail_timeout=30s;
    server gateway-3:8080 max_fails=3 fail_timeout=30s;
}

upstream worker_backend {
    least_conn;
    server worker-1:5345 max_fails=3 fail_timeout=30s;
    server worker-2:5345 max_fails=3 fail_timeout=30s;
}

server {
    listen 443 ssl http2;
    server_name api.example.com;
    
    ssl_certificate /etc/ssl/certs/cert.pem;
    ssl_certificate_key /etc/ssl/private/key.pem;
    
    # SSE требует отключения буферизации
    proxy_buffering off;
    proxy_cache off;
    
    location / {
        proxy_pass http://gateway_backend;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        
        # Увеличенные таймауты для long-running requests
        proxy_connect_timeout 10s;
        proxy_send_timeout 300s;
        proxy_read_timeout 300s;
    }
}
```

### 5. Безопасность

#### API ключи

```yaml
# compose.yaml
gateway:
  environment:
    - OPENAI_API_KEY_REQUIRED=true
    - OPENAI_API_KEY=${API_KEY}
```

```bash
# Генерация ключа
API_KEY=$(openssl rand -base64 32)
echo "API_KEY=$API_KEY" >> .env
```

#### Rate limiting на уровне nginx

```nginx
limit_req_zone $binary_remote_addr zone=api_limit:10m rate=10r/s;

location /v1/ {
    limit_req zone=api_limit burst=20 nodelay;
    proxy_pass http://gateway_backend;
}
```

#### Network isolation

```yaml
networks:
  frontend:
    driver: bridge
  backend:
    driver: bridge
    internal: true

services:
  gateway:
    networks:
      - frontend
      - backend
  
  worker:
    networks:
      - backend
  
  redis:
    networks:
      - backend
```

### 6. Backup и Recovery

#### Redis backup

```bash
# Автоматический backup каждые 6 часов
0 */6 * * * docker compose -f compose.yaml --profile worker exec redis redis-cli BGSAVE
0 */6 * * * docker cp redis:/data/dump.rdb /backup/redis-$(date +\%Y\%m\%d-\%H\%M\%S).rdb
```

#### Job recovery после сбоя

Jobs в статусе `running` при перезапуске нужно либо:
- Перезапустить (установить статус `queued`)
- Или пометить как `failed`

```go
// При старте Worker
func (w *Worker) RecoverJobs(storage *Storage) {
    // Получить все jobs в статусе running
    runningJobs := storage.GetJobsByStatus(StatusRunning)
    
    for _, jobID := range runningJobs {
        // Вариант 1: Пометить как failed
        storage.UpdateJobStatus(jobID, StatusFailed, 
            time.Now().Format(time.RFC3339), 
            "Worker restart")
            
        // Вариант 2: Переставить в очередь
        // storage.UpdateJobStatus(jobID, StatusQueued, "", "")
        // w.Enqueue(jobID)
    }
}
```

### 7. Логирование

#### Structured logging

```go
import "go.uber.org/zap"

logger, _ := zap.NewProduction()
defer logger.Sync()

logger.Info("job created",
    zap.String("job_id", jobID),
    zap.String("model", model),
    zap.Int("queue_size", queueSize))
```

#### Централизованное логирование

```yaml
# compose.yaml
services:
  gateway:
    logging:
      driver: "fluentd"
      options:
        fluentd-address: localhost:24224
        tag: gateway
```

### 8. Тестирование нагрузки

#### Локальный load test

```bash
# Установка k6
brew install k6  # macOS
# или скачать с https://k6.io/

# load_test.js
import http from 'k6/http';
import { check } from 'k6';

export let options = {
  stages: [
    { duration: '1m', target: 10 },   // Разогрев
    { duration: '5m', target: 50 },   // Нагрузка
    { duration: '1m', target: 0 },    // Остывание
  ],
};

export default function() {
  let payload = JSON.stringify({
    model: 'gpt-oss:20b',
    messages: [{ role: 'user', content: 'Hello' }],
    stream: false,
  });

  let res = http.post('http://localhost:18080/v1/chat/completions', payload, {
    headers: { 'Content-Type': 'application/json' },
  });

  check(res, {
    'status is 200': (r) => r.status === 200,
    'status is not 429': (r) => r.status !== 429,
  });
}
```

```bash
k6 run load_test.js
```

### 9. Checklist для production

- [ ] Вычислен и установлен правильный `WORKER_CONCURRENCY`
- [ ] Redis настроен с persistence и memory limits
- [ ] Настроены health checks для всех сервисов
- [ ] Включено TLS на внешних endpoints
- [ ] Настроена аутентификация (API keys)
- [ ] Включено structured logging
- [ ] Настроен мониторинг (Prometheus + Grafana)
- [ ] Настроены alerts для критичных метрик
- [ ] Протестирована нагрузка с помощью load testing
- [ ] Настроен автоматический backup Redis
- [ ] Документированы процедуры восстановления
- [ ] Network isolation между компонентами
- [ ] Rate limiting на уровне API gateway

### 10. Примеры конфигураций

#### Малая нагрузка (< 100 req/day)

```yaml
worker:
  environment:
    - WORKER_CONCURRENCY=1
  deploy:
    replicas: 1

redis:
  deploy:
    resources:
      limits:
        memory: 1G
```

#### Средняя нагрузка (1000-10000 req/day)

```yaml
worker:
  environment:
    - WORKER_CONCURRENCY=3
  deploy:
    replicas: 2

redis:
  deploy:
    resources:
      limits:
        memory: 4G
```

#### Высокая нагрузка (> 10000 req/day)

```yaml
worker:
  environment:
    - WORKER_CONCURRENCY=5
  deploy:
    replicas: 3

redis:
  image: redis:7-cluster
  deploy:
    resources:
      limits:
        memory: 8G
```

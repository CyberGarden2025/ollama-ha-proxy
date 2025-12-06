curl -N http://localhost:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-oss:20b",
    "messages": [{"role": "user", "content": "Расскажи короткую историю"}],
    "stream": true
  }'

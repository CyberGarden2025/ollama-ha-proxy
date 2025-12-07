#!/usr/bin/env python3
"""
Примеры использования Ollama Proxy с OpenAI-совместимым API
"""

import os
import requests
import json
import time
from typing import Iterator, Optional

BASE_URL = os.getenv("BASE_URL", "http://localhost:18080")
REQUEST_TIMEOUT = float(os.getenv("REQUEST_TIMEOUT", "60"))
STREAM_TIMEOUT = float(os.getenv("STREAM_TIMEOUT", "90"))


def _print_error(resp: requests.Response, prefix: str = "Error"):
    try:
        body = resp.text
    except Exception:
        body = "<unreadable body>"
    print(f"{prefix}: {resp.status_code}")
    if body:
        print(body)

def example_streaming_request():
    """Пример streaming запроса с обработкой SSE"""
    print("=== Streaming Request ===\n")
    
    payload = {
        "model": "gpt-oss:20b",
        "messages": [
            {"role": "user", "content": "Напиши короткое стихотворение про программирование"}
        ],
        "stream": True,
        "temperature": 0.7
    }
    
    try:
        response = requests.post(
            f"{BASE_URL}/v1/chat/completions",
            json=payload,
            stream=True,
            timeout=REQUEST_TIMEOUT
        )
    except requests.RequestException as e:
        print(f"Streaming request failed: {e}")
        return

    if response.status_code != 200:
        _print_error(response, "Streaming request failed")
        return
    
    print("Response chunks:")
    start = time.time()
    for line in response.iter_lines():
        if time.time() - start > STREAM_TIMEOUT:
            print("\n[Stream aborted due to timeout]")
            break
        if line:
            line = line.decode('utf-8')
            if line.startswith('data: '):
                data = line[6:]
                if data == '[DONE]':
                    print("\n[Stream completed]")
                    break
                try:
                    chunk = json.loads(data)
                    choice = chunk['choices'][0]
                    delta = choice.get('delta', {}) or {}
                    content = delta.get('content') or choice.get('message', {}).get('content')
                    if content:
                        print(content, end='', flush=True)
                    else:
                        # fallback: show raw chunk for debugging empty payloads
                        print(f"\n[chunk no content] {json.dumps(chunk, ensure_ascii=False)}")
                except json.JSONDecodeError:
                    pass
    print()


def example_non_streaming_request():
    """Пример обычного запроса без streaming"""
    print("\n=== Non-Streaming Request ===\n")
    
    payload = {
        "model": "gpt-oss:20b",
        "messages": [
            {"role": "user", "content": "Что такое рекурсия?"}
        ],
        "stream": False,
        "max_tokens": 100
    }
    
    response = requests.post(
        f"{BASE_URL}/v1/chat/completions",
        json=payload,
        timeout=REQUEST_TIMEOUT
    )

    try:
        response.raise_for_status()
        result = response.json()
        print("Response:")
        print(result['choices'][0]['message']['content'])
        print(f"\nFinish reason: {result['choices'][0]['finish_reason']}")
    except requests.RequestException as e:
        print(f"Non-streaming request failed: {e}")
        _print_error(response)


def example_get_models():
    """Получение списка доступных моделей"""
    print("\n=== Available Models ===\n")
    
    response = requests.get(f"{BASE_URL}/v1/models")
    
    if response.status_code == 200:
        models = response.json()
        for model in models['data']:
            print(f"- {model['id']} (owned by: {model['owned_by']})")
    else:
        _print_error(response)


def example_get_stats():
    """Получение статистики загрузки системы"""
    print("\n=== System Stats ===\n")
    
    response = requests.get(f"{BASE_URL}/v1/stats")
    
    if response.status_code == 200:
        stats = response.json()
        print(f"Active workers: {stats['active']}/{stats['capacity']}")
        print(f"Queued jobs: {stats['queued']}")
        print(f"Max queue size: {stats['max_queue']}")
        
        utilization = (stats['active'] / stats['capacity']) * 100
        print(f"Utilization: {utilization:.1f}%")
        
        if stats['active'] == stats['capacity']:
            print("⚠️  System at full capacity")
        if stats['queued'] > stats['capacity'] // 2:
            print("⚠️  High queue load")
    else:
        _print_error(response)


def example_rate_limit_handling():
    """Пример обработки rate limiting с retry"""
    print("\n=== Rate Limit Handling ===\n")
    
    def make_request_with_retry(max_retries=3, backoff=2):
        payload = {
            "model": "gpt-oss:20b",
            "messages": [{"role": "user", "content": "Hello"}],
            "stream": False
        }
        
        for attempt in range(max_retries):
            try:
                response = requests.post(
                    f"{BASE_URL}/v1/chat/completions",
                    json=payload,
                    timeout=300
                )
                
                if response.status_code == 200:
                    return response.json()
                elif response.status_code == 429:
                    print(f"Rate limited, retry {attempt + 1}/{max_retries}")
                    if attempt < max_retries - 1:
                        time.sleep(backoff ** attempt)
                        continue
                    else:
                        print("Max retries exceeded")
                        return None
                else:
                    _print_error(response)
                    return None
            except Exception as e:
                print(f"Request failed: {e}")
                return None
        
        return None
    
    result = make_request_with_retry()
    if result:
        print("Request successful!")
        print(result['choices'][0]['message']['content'][:100] + "...")


def example_conversation():
    """Пример мультитурной беседы"""
    print("\n=== Multi-turn Conversation ===\n")
    
    messages = [
        {"role": "user", "content": "Привет! Как тебя зовут?"}
    ]
    
    for turn in range(2):
        payload = {
            "model": "gpt-oss:20b",
            "messages": messages,
            "stream": False,
            "max_tokens": 100
        }
        
        response = requests.post(
            f"{BASE_URL}/v1/chat/completions",
            json=payload,
            timeout=REQUEST_TIMEOUT
        )
        
        if response.status_code == 200:
            result = response.json()
            assistant_message = result['choices'][0]['message']['content']
            
            print(f"User: {messages[-1]['content']}")
            print(f"Assistant: {assistant_message}\n")
            
            messages.append({"role": "assistant", "content": assistant_message})
            
            if turn == 0:
                messages.append({"role": "user", "content": "Расскажи короткую шутку"})
        else:
            _print_error(response)
            break


def example_monitor_stats(duration_seconds=10):
    """Мониторинг статистики в реальном времени"""
    print(f"\n=== Monitoring Stats for {duration_seconds}s ===\n")
    
    end_time = time.time() + duration_seconds
    
    while time.time() < end_time:
        response = requests.get(f"{BASE_URL}/v1/stats")
        
        if response.status_code == 200:
            stats = response.json()
            timestamp = time.strftime("%H:%M:%S")
            print(f"[{timestamp}] Active: {stats['active']}/{stats['capacity']} | "
                  f"Queued: {stats['queued']} | "
                  f"Available: {stats['capacity'] - stats['active']}")
        
        time.sleep(1)


def example_openai_compatible():
    """Пример использования с библиотекой OpenAI"""
    print("\n=== OpenAI Library Compatible ===\n")
    
    try:
        from openai import OpenAI
        
        client = OpenAI(
            api_key="not-needed",
            base_url=BASE_URL + "/v1"
        )
        
        response = client.chat.completions.create(
            model="gpt-oss:20b",
            messages=[
                {"role": "user", "content": "Привет!"}
            ],
            stream=False
        )
        
        print("Using OpenAI library:")
        print(response.choices[0].message.content)
        
    except ImportError:
        print("OpenAI library not installed. Install with: pip install openai")


if __name__ == "__main__":
    print("Ollama Proxy API Examples\n" + "="*50)
    
    mode = os.getenv("EXAMPLES_MODE", "basic").lower()
    if mode == "full":
        examples = [
            example_get_models,
            example_get_stats,
            example_non_streaming_request,
            example_streaming_request,
            example_conversation,
            example_rate_limit_handling,
            example_openai_compatible,
        ]
    else:
        examples = [
            example_get_models,
            example_get_stats,
        ]

    for fn in examples:
        try:
            fn()
        except KeyboardInterrupt:
            print("\n\nInterrupted by user")
            break
        except Exception as e:
            print(f"\n[{fn.__name__}] failed: {e}")

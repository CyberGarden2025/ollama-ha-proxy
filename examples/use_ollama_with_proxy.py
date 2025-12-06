#!/usr/bin/env python3
"""
Примеры использования Ollama Proxy с OpenAI-совместимым API
"""

import requests
import json
import time
from typing import Iterator

BASE_URL = "http://localhost:18080"

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
    
    response = requests.post(
        f"{BASE_URL}/v1/chat/completions",
        json=payload,
        stream=True,
        timeout=300
    )
    
    print("Response chunks:")
    for line in response.iter_lines():
        if line:
            line = line.decode('utf-8')
            if line.startswith('data: '):
                data = line[6:]
                if data == '[DONE]':
                    print("\n[Stream completed]")
                    break
                try:
                    chunk = json.loads(data)
                    content = chunk['choices'][0]['delta'].get('content', '')
                    if content:
                        print(content, end='', flush=True)
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
        timeout=300
    )
    
    if response.status_code == 200:
        result = response.json()
        print("Response:")
        print(result['choices'][0]['message']['content'])
        print(f"\nFinish reason: {result['choices'][0]['finish_reason']}")
    else:
        print(f"Error: {response.status_code}")
        print(response.text)


def example_get_models():
    """Получение списка доступных моделей"""
    print("\n=== Available Models ===\n")
    
    response = requests.get(f"{BASE_URL}/v1/models")
    
    if response.status_code == 200:
        models = response.json()
        for model in models['data']:
            print(f"- {model['id']} (owned by: {model['owned_by']})")
    else:
        print(f"Error: {response.status_code}")


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
        print(f"Error: {response.status_code}")


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
                    print(f"Error: {response.status_code}")
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
            timeout=300
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
            print(f"Error: {response.status_code}")
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
    
    try:
        example_get_models()
        example_get_stats()
        example_non_streaming_request()
        example_streaming_request()
        example_conversation()
        example_rate_limit_handling()
        
        # example_monitor_stats(10)
        # example_openai_compatible()
        
    except KeyboardInterrupt:
        print("\n\nInterrupted by user")
    except Exception as e:
        print(f"\n\nError: {e}")

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

type OpenAIChatRequest struct {
	Model       string                   `json:"model"`
	Messages    []map[string]interface{} `json:"messages"`
	Stream      bool                     `json:"stream"`
	Temperature float64                  `json:"temperature,omitempty"`
	TopP        float64                  `json:"top_p,omitempty"`
	MaxTokens   int                      `json:"max_tokens,omitempty"`
}

type OpenAIChatResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []OpenAIChatChoice     `json:"choices"`
	Usage   map[string]interface{} `json:"usage,omitempty"`
}

type OpenAIChatChoice struct {
	Index        int                    `json:"index"`
	Message      map[string]interface{} `json:"message,omitempty"`
	Delta        map[string]interface{} `json:"delta,omitempty"`
	FinishReason string                 `json:"finish_reason,omitempty"`
}

type BackendJobRequest struct {
	Model    string                   `json:"model"`
	Messages []map[string]interface{} `json:"messages,omitempty"`
	Options  map[string]interface{}   `json:"options,omitempty"`
}

type BackendJobResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

type BackendEventsResponse struct {
	Status string              `json:"status"`
	Chunks []BackendChunkData  `json:"chunks"`
}

type BackendChunkData struct {
	Seq          int    `json:"seq"`
	Delta        string `json:"delta"`
	Done         bool   `json:"done"`
	FinishReason string `json:"finish_reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

type Config struct {
	BackendURL         string
	PollIntervalMS     int
	RetryBackoffInitMS int
	RetryBackoffMaxMS  int
	JobTimeoutMS       int
	APIKeyRequired     bool
	APIKey             string
}

func LoadConfig() *Config {
	cfg := &Config{
		BackendURL:         getEnv("BACKEND_PROXY_URL", "http://localhost:5345"),
		PollIntervalMS:     getEnvInt("POLL_INTERVAL_MS", 500),
		RetryBackoffInitMS: getEnvInt("RETRY_BACKOFF_INIT_MS", 1000),
		RetryBackoffMaxMS:  getEnvInt("RETRY_BACKOFF_MAX_MS", 30000),
		JobTimeoutMS:       getEnvInt("JOB_TIMEOUT_MS", 1800000),
		APIKeyRequired:     getEnv("OPENAI_API_KEY_REQUIRED", "false") == "true",
		APIKey:             getEnv("OPENAI_API_KEY", ""),
	}
	return cfg
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

type Proxy1Server struct {
	config     *Config
	httpClient *http.Client
}

func NewProxy1Server(cfg *Config) *Proxy1Server {
	return &Proxy1Server{
		config: cfg,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.JobTimeoutMS) * time.Millisecond,
		},
	}
}

func (s *Proxy1Server) validateAuth(r *http.Request) error {
	if !s.config.APIKeyRequired {
		return nil
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return fmt.Errorf("missing authorization header")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token != s.config.APIKey {
		return fmt.Errorf("invalid api key")
	}

	return nil
}

func (s *Proxy1Server) createBackendJob(req OpenAIChatRequest) (string, error) {
	options := make(map[string]interface{})
	if req.Temperature != 0 {
		options["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		options["top_p"] = req.TopP
	}
	if req.MaxTokens != 0 {
		options["num_predict"] = req.MaxTokens
	}

	backendReq := BackendJobRequest{
		Model:    req.Model,
		Messages: req.Messages,
		Options:  options,
	}

	payload, err := json.Marshal(backendReq)
	if err != nil {
		return "", err
	}

	resp, err := http.Post(s.config.BackendURL+"/jobs", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		return "", &RateLimitError{Message: string(body)}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("backend error: %s", string(body))
	}

	var jobResp BackendJobResponse
	if err := json.NewDecoder(resp.Body).Decode(&jobResp); err != nil {
		return "", err
	}

	return jobResp.JobID, nil
}

type RateLimitError struct {
	Message string
}

func (e *RateLimitError) Error() string {
	return e.Message
}

func (s *Proxy1Server) pollBackendEvents(ctx context.Context, jobID string, lastSeq int) (*BackendEventsResponse, error) {
	url := fmt.Sprintf("%s/jobs/%s/events?from_seq=%d", s.config.BackendURL, jobID, lastSeq)
	
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend status: %d", resp.StatusCode)
	}

	var eventsResp BackendEventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&eventsResp); err != nil {
		return nil, err
	}

	return &eventsResp, nil
}

func (s *Proxy1Server) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if err := s.validateAuth(r); err != nil {
		s.writeError(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var req OpenAIChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Stream {
		s.handleStreamingChat(w, r, req)
	} else {
		s.handleNonStreamingChat(w, r, req)
	}
}

func (s *Proxy1Server) handleStreamingChat(w http.ResponseWriter, r *http.Request, req OpenAIChatRequest) {
	jobID, err := s.createBackendJob(req)
	if err != nil {
		if _, ok := err.(*RateLimitError); ok {
			s.writeError(w, "Service overloaded. Please try again later.", http.StatusTooManyRequests)
			return
		}
		s.writeError(w, fmt.Sprintf("failed to create job: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.config.JobTimeoutMS)*time.Millisecond)
	defer cancel()

	chatID := fmt.Sprintf("chatcmpl-%s", uuid.New().String())
	lastSeq := -1
	retryDelay := s.config.RetryBackoffInitMS
	created := time.Now().Unix()

	for {
		select {
		case <-ctx.Done():
			s.writeSSEError(w, "timeout exceeded")
			flusher.Flush()
			return
		default:
		}

		events, err := s.pollBackendEvents(ctx, jobID, lastSeq)
		if err != nil {
			log.Printf("Poll error for job %s: %v", jobID, err)
			time.Sleep(time.Duration(retryDelay) * time.Millisecond)
			retryDelay = min(retryDelay*2, s.config.RetryBackoffMaxMS)
			continue
		}

		retryDelay = s.config.RetryBackoffInitMS

		for _, chunk := range events.Chunks {
			if chunk.Seq <= lastSeq {
				continue
			}
			lastSeq = chunk.Seq

			if chunk.Error != "" {
				s.writeSSEError(w, chunk.Error)
				flusher.Flush()
				return
			}

			delta := map[string]interface{}{}
			if chunk.Delta != "" {
				delta["content"] = chunk.Delta
			}
			if chunk.Delta == "" && chunk.Done {
				delta["content"] = ""
			}

			sseData := OpenAIChatResponse{
				ID:      chatID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   req.Model,
				Choices: []OpenAIChatChoice{
					{
						Index:        0,
						Delta:        delta,
						FinishReason: chunk.FinishReason,
					},
				},
			}

			data, _ := json.Marshal(sseData)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()

			if chunk.Done {
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
		}

		if events.Status == "completed" || events.Status == "failed" || events.Status == "cancelled" {
			if events.Status != "completed" {
				s.writeSSEError(w, fmt.Sprintf("job %s", events.Status))
			} else {
				fmt.Fprintf(w, "data: [DONE]\n\n")
			}
			flusher.Flush()
			return
		}

		time.Sleep(time.Duration(s.config.PollIntervalMS) * time.Millisecond)
	}
}

func (s *Proxy1Server) handleNonStreamingChat(w http.ResponseWriter, r *http.Request, req OpenAIChatRequest) {
	jobID, err := s.createBackendJob(req)
	if err != nil {
		if _, ok := err.(*RateLimitError); ok {
			s.writeError(w, "Service overloaded. Please try again later.", http.StatusTooManyRequests)
			return
		}
		s.writeError(w, fmt.Sprintf("failed to create job: %v", err), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.config.JobTimeoutMS)*time.Millisecond)
	defer cancel()

	lastSeq := -1
	retryDelay := s.config.RetryBackoffInitMS
	var fullContent strings.Builder
	var finishReason string

	for {
		select {
		case <-ctx.Done():
			s.writeError(w, "timeout exceeded", http.StatusGatewayTimeout)
			return
		default:
		}

		events, err := s.pollBackendEvents(ctx, jobID, lastSeq)
		if err != nil {
			log.Printf("Poll error for job %s: %v", jobID, err)
			time.Sleep(time.Duration(retryDelay) * time.Millisecond)
			retryDelay = min(retryDelay*2, s.config.RetryBackoffMaxMS)
			continue
		}

		retryDelay = s.config.RetryBackoffInitMS

		for _, chunk := range events.Chunks {
			if chunk.Seq <= lastSeq {
				continue
			}
			lastSeq = chunk.Seq

			if chunk.Error != "" {
				s.writeError(w, chunk.Error, http.StatusInternalServerError)
				return
			}

			fullContent.WriteString(chunk.Delta)

			if chunk.Done {
				finishReason = chunk.FinishReason
				break
			}
		}

		if events.Status == "completed" {
			resp := OpenAIChatResponse{
				ID:      fmt.Sprintf("chatcmpl-%s", uuid.New().String()),
				Object:  "chat.completion",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []OpenAIChatChoice{
					{
						Index: 0,
						Message: map[string]interface{}{
							"role":    "assistant",
							"content": fullContent.String(),
						},
						FinishReason: finishReason,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		if events.Status == "failed" || events.Status == "cancelled" {
			s.writeError(w, fmt.Sprintf("job %s", events.Status), http.StatusInternalServerError)
			return
		}

		time.Sleep(time.Duration(s.config.PollIntervalMS) * time.Millisecond)
	}
}

func (s *Proxy1Server) Models(w http.ResponseWriter, r *http.Request) {
	if err := s.validateAuth(r); err != nil {
		s.writeError(w, err.Error(), http.StatusUnauthorized)
		return
	}

	models := map[string]interface{}{
		"object": "list",
		"data": []map[string]interface{}{
			{
				"id":       "gpt-oss:20b",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "ollama",
			},
			{
				"id":       "gpt-oss:120b",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "ollama",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

func (s *Proxy1Server) BackendStats(w http.ResponseWriter, r *http.Request) {
	if err := s.validateAuth(r); err != nil {
		s.writeError(w, err.Error(), http.StatusUnauthorized)
		return
	}

	resp, err := http.Get(s.config.BackendURL + "/stats")
	if err != nil {
		s.writeError(w, fmt.Sprintf("failed to get backend stats: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.writeError(w, "backend stats unavailable", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

func (s *Proxy1Server) writeError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "server_error",
			"code":    statusCode,
		},
	})
}

func (s *Proxy1Server) writeSSEError(w http.ResponseWriter, message string) {
	errData := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "server_error",
		},
	}
	data, _ := json.Marshal(errData)
	fmt.Fprintf(w, "data: %s\n\n", string(data))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	cfg := LoadConfig()
	server := NewProxy1Server(cfg)

	r := mux.NewRouter()
	r.HandleFunc("/v1/chat/completions", server.ChatCompletions).Methods("POST")
	r.HandleFunc("/v1/models", server.Models).Methods("GET")
	r.HandleFunc("/v1/stats", server.BackendStats).Methods("GET")

	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}).Methods("GET")

	port := getEnv("PORT", "8080")
	log.Printf("Gateway listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}

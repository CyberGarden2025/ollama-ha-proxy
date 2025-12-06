package main

import (
	"bufio"
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
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

type JobRequest struct {
	Model    string                   `json:"model"`
	Messages []map[string]interface{} `json:"messages,omitempty"`
	Prompt   string                   `json:"prompt,omitempty"`
	Options  map[string]interface{}   `json:"options,omitempty"`
}

type JobResponse struct {
	JobID  string    `json:"job_id"`
	Status JobStatus `json:"status"`
}

type ChunkData struct {
	Seq          int    `json:"seq"`
	Delta        string `json:"delta"`
	Done         bool   `json:"done"`
	FinishReason string `json:"finish_reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

type EventsResponse struct {
	Status JobStatus   `json:"status"`
	Chunks []ChunkData `json:"chunks"`
}

type StatusResponse struct {
	Status      JobStatus `json:"status"`
	CreatedAt   string    `json:"created_at"`
	CompletedAt string    `json:"completed_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type JobMeta struct {
	Status      JobStatus `json:"status"`
	Model       string    `json:"model"`
	CreatedAt   string    `json:"created_at"`
	CompletedAt string    `json:"completed_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type OllamaStreamResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done               bool   `json:"done"`
	DoneReason         string `json:"done_reason,omitempty"`
	TotalDuration      int64  `json:"total_duration,omitempty"`
	LoadDuration       int64  `json:"load_duration,omitempty"`
	PromptEvalCount    int    `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64  `json:"prompt_eval_duration,omitempty"`
	EvalCount          int    `json:"eval_count,omitempty"`
	EvalDuration       int64  `json:"eval_duration,omitempty"`
}

type Storage struct {
	rdb *redis.Client
	ctx context.Context
}

func NewStorage(redisURL string) (*Storage, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}

	rdb := redis.NewClient(opt)
	ctx := context.Background()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}

	return &Storage{rdb: rdb, ctx: ctx}, nil
}

func (s *Storage) CreateJob(jobID string, meta JobMeta) error {
	key := fmt.Sprintf("job:%s:meta", jobID)
	data := map[string]interface{}{
		"status":     string(meta.Status),
		"model":      meta.Model,
		"created_at": meta.CreatedAt,
	}
	return s.rdb.HSet(s.ctx, key, data).Err()
}

func (s *Storage) GetJobMeta(jobID string) (*JobMeta, error) {
	key := fmt.Sprintf("job:%s:meta", jobID)
	result, err := s.rdb.HGetAll(s.ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("job not found")
	}

	meta := &JobMeta{
		Status:      JobStatus(result["status"]),
		Model:       result["model"],
		CreatedAt:   result["created_at"],
		CompletedAt: result["completed_at"],
		Error:       result["error"],
	}
	return meta, nil
}

func (s *Storage) UpdateJobStatus(jobID string, status JobStatus, completedAt string, errorMsg string) error {
	key := fmt.Sprintf("job:%s:meta", jobID)
	data := map[string]interface{}{
		"status": string(status),
	}
	if completedAt != "" {
		data["completed_at"] = completedAt
	}
	if errorMsg != "" {
		data["error"] = errorMsg
	}
	return s.rdb.HSet(s.ctx, key, data).Err()
}

func (s *Storage) AddChunk(jobID string, chunk ChunkData) error {
	key := fmt.Sprintf("job:%s:chunks", jobID)
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return s.rdb.RPush(s.ctx, key, string(data)).Err()
}

func (s *Storage) GetChunks(jobID string, fromSeq int) ([]ChunkData, error) {
	key := fmt.Sprintf("job:%s:chunks", jobID)
	result, err := s.rdb.LRange(s.ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}

	chunks := make([]ChunkData, 0)
	for _, item := range result {
		var chunk ChunkData
		if err := json.Unmarshal([]byte(item), &chunk); err != nil {
			continue
		}
		if chunk.Seq > fromSeq {
			chunks = append(chunks, chunk)
			if len(chunks) >= 1000 {
				break
			}
		}
	}
	return chunks, nil
}

func (s *Storage) IncrSeq(jobID string) (int, error) {
	key := fmt.Sprintf("job:%s:seq", jobID)
	val, err := s.rdb.Incr(s.ctx, key).Result()
	if err != nil {
		return 0, err
	}
	return int(val), nil
}

func (s *Storage) SetTTL(jobID string, ttl time.Duration) error {
	keys := []string{
		fmt.Sprintf("job:%s:meta", jobID),
		fmt.Sprintf("job:%s:chunks", jobID),
		fmt.Sprintf("job:%s:seq", jobID),
	}
	for _, key := range keys {
		if err := s.rdb.Expire(s.ctx, key, ttl).Err(); err != nil {
			return err
		}
	}
	return nil
}

type Worker struct {
	storage        *Storage
	ollamaURL      string
	concurrency    int
	maxQueueSize   int
	queue          chan string
	wg             sync.WaitGroup
	mu             sync.RWMutex
	cancelled      map[string]bool
	activeJobs     int
	queuedJobs     int
}

func NewWorker(storage *Storage, ollamaURL string, concurrency int) *Worker {
	maxQueueSize := concurrency * 2
	w := &Worker{
		storage:      storage,
		ollamaURL:    ollamaURL,
		concurrency:  concurrency,
		maxQueueSize: maxQueueSize,
		queue:        make(chan string, maxQueueSize),
		cancelled:    make(map[string]bool),
	}

	for i := 0; i < concurrency; i++ {
		w.wg.Add(1)
		go w.run()
	}

	return w
}

func (w *Worker) Enqueue(jobID string) error {
	w.mu.Lock()
	currentActive := w.activeJobs
	currentQueued := len(w.queue)
	w.mu.Unlock()

	totalLoad := currentActive + currentQueued
	if totalLoad >= w.maxQueueSize {
		return fmt.Errorf("queue full: active=%d, queued=%d, max=%d", currentActive, currentQueued, w.maxQueueSize)
	}

	select {
	case w.queue <- jobID:
		w.mu.Lock()
		w.queuedJobs++
		w.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("queue channel full")
	}
}

func (w *Worker) GetStats() map[string]int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return map[string]int{
		"active":   w.activeJobs,
		"queued":   len(w.queue),
		"capacity": w.concurrency,
		"max_queue": w.maxQueueSize,
	}
}

func (w *Worker) Cancel(jobID string) {
	w.mu.Lock()
	w.cancelled[jobID] = true
	w.mu.Unlock()
}

func (w *Worker) isCancelled(jobID string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cancelled[jobID]
}

func (w *Worker) run() {
	defer w.wg.Done()
	for jobID := range w.queue {
		w.mu.Lock()
		w.activeJobs++
		w.queuedJobs--
		w.mu.Unlock()

		w.processJob(jobID)

		w.mu.Lock()
		w.activeJobs--
		w.mu.Unlock()
	}
}

func (w *Worker) processJob(jobID string) {
	meta, err := w.storage.GetJobMeta(jobID)
	if err != nil {
		log.Printf("Failed to get job meta %s: %v", jobID, err)
		return
	}

	if err := w.storage.UpdateJobStatus(jobID, StatusRunning, "", ""); err != nil {
		log.Printf("Failed to update job status %s: %v", jobID, err)
		return
	}

	messages, _ := w.storage.rdb.HGet(context.Background(), fmt.Sprintf("job:%s:meta", jobID), "messages").Result()
	options, _ := w.storage.rdb.HGet(context.Background(), fmt.Sprintf("job:%s:meta", jobID), "options").Result()

	reqBody := map[string]interface{}{
		"model":  meta.Model,
		"stream": true,
	}

	if messages != "" {
		var msgs []map[string]interface{}
		if err := json.Unmarshal([]byte(messages), &msgs); err == nil {
			reqBody["messages"] = msgs
		}
	}

	if options != "" {
		var opts map[string]interface{}
		if err := json.Unmarshal([]byte(options), &opts); err == nil {
			for k, v := range opts {
				reqBody[k] = v
			}
		}
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		w.finishJobWithError(jobID, fmt.Sprintf("marshal error: %v", err))
		return
	}

	req, err := http.NewRequest("POST", w.ollamaURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		w.finishJobWithError(jobID, fmt.Sprintf("create request error: %v", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		w.finishJobWithError(jobID, fmt.Sprintf("ollama request error: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.finishJobWithError(jobID, fmt.Sprintf("ollama status: %d", resp.StatusCode))
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	// allow large tokens for streaming responses
	buf := make([]byte, 0, 1024*64)
	scanner.Buffer(buf, 1024*1024)
	finishReason := "stop"

	for scanner.Scan() {
		if w.isCancelled(jobID) {
			w.finishJobWithError(jobID, "cancelled")
			return
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			break
		}

		var streamResp OllamaStreamResponse
		if err := json.Unmarshal([]byte(payload), &streamResp); err != nil {
			w.finishJobWithError(jobID, fmt.Sprintf("decode error: %v", err))
			return
		}

		seq, err := w.storage.IncrSeq(jobID)
		if err != nil {
			log.Printf("Failed to incr seq for job %s: %v", jobID, err)
			continue
		}

		chunk := ChunkData{
			Seq:   seq,
			Delta: streamResp.Message.Content,
			Done:  streamResp.Done,
		}

		if streamResp.Done {
			if streamResp.DoneReason == "length" {
				finishReason = "length"
			}
			chunk.FinishReason = finishReason
		}

		if err := w.storage.AddChunk(jobID, chunk); err != nil {
			log.Printf("Failed to add chunk for job %s: %v", jobID, err)
		}

		if streamResp.Done {
			break
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		w.finishJobWithError(jobID, fmt.Sprintf("stream error: %v", err))
		return
	}

	now := time.Now().Format(time.RFC3339)
	if err := w.storage.UpdateJobStatus(jobID, StatusCompleted, now, ""); err != nil {
		log.Printf("Failed to complete job %s: %v", jobID, err)
	}

	ttl := 24 * time.Hour
	if err := w.storage.SetTTL(jobID, ttl); err != nil {
		log.Printf("Failed to set TTL for job %s: %v", jobID, err)
	}
}

func (w *Worker) finishJobWithError(jobID string, errorMsg string) {
	seq, _ := w.storage.IncrSeq(jobID)
	chunk := ChunkData{
		Seq:          seq,
		Delta:        "",
		Done:         true,
		FinishReason: "error",
		Error:        errorMsg,
	}
	w.storage.AddChunk(jobID, chunk)
	now := time.Now().Format(time.RFC3339)
	w.storage.UpdateJobStatus(jobID, StatusFailed, now, errorMsg)
}

type Server struct {
	storage *Storage
	worker  *Worker
}

func (s *Server) CreateJob(w http.ResponseWriter, r *http.Request) {
	var req JobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stats := s.worker.GetStats()
	log.Printf("Worker stats before enqueue: %+v", stats)

	jobID := uuid.New().String()
	now := time.Now().Format(time.RFC3339)

	meta := JobMeta{
		Status:    StatusQueued,
		Model:     req.Model,
		CreatedAt: now,
	}

	if err := s.storage.CreateJob(jobID, meta); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(req.Messages) > 0 {
		data, _ := json.Marshal(req.Messages)
		s.storage.rdb.HSet(context.Background(), fmt.Sprintf("job:%s:meta", jobID), "messages", string(data))
	}

	if len(req.Options) > 0 {
		data, _ := json.Marshal(req.Options)
		s.storage.rdb.HSet(context.Background(), fmt.Sprintf("job:%s:meta", jobID), "options", string(data))
	}

	if err := s.worker.Enqueue(jobID); err != nil {
		s.storage.UpdateJobStatus(jobID, StatusFailed, now, err.Error())
		
		errorResp := map[string]interface{}{
			"error": map[string]interface{}{
				"message": fmt.Sprintf("Service overloaded: %v", err),
				"type":    "server_error",
				"code":    "rate_limit_exceeded",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(errorResp)
		return
	}

	resp := JobResponse{
		JobID:  jobID,
		Status: StatusQueued,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) GetEvents(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	jobID := vars["job_id"]

	fromSeq := -1
	if seqStr := r.URL.Query().Get("from_seq"); seqStr != "" {
		if seq, err := strconv.Atoi(seqStr); err == nil {
			fromSeq = seq
		}
	}

	meta, err := s.storage.GetJobMeta(jobID)
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	chunks, err := s.storage.GetChunks(jobID, fromSeq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := EventsResponse{
		Status: meta.Status,
		Chunks: chunks,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) GetStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	jobID := vars["job_id"]

	meta, err := s.storage.GetJobMeta(jobID)
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	resp := StatusResponse{
		Status:      meta.Status,
		CreatedAt:   meta.CreatedAt,
		CompletedAt: meta.CompletedAt,
		Error:       meta.Error,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) CancelJob(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	jobID := vars["job_id"]

	s.worker.Cancel(jobID)

	now := time.Now().Format(time.RFC3339)
	if err := s.storage.UpdateJobStatus(jobID, StatusCancelled, now, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) GetWorkerStats(w http.ResponseWriter, r *http.Request) {
	stats := s.worker.GetStats()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func main() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/0"
	}

	ollamaURL := os.Getenv("OLLAMA_BASE_URL")
	if ollamaURL == "" {
		ollamaURL = "http://127.0.0.1:11434"
	}

	concurrency := 10
	if c := os.Getenv("WORKER_CONCURRENCY"); c != "" {
		if val, err := strconv.Atoi(c); err == nil {
			concurrency = val
		}
	}

	storage, err := NewStorage(redisURL)
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	worker := NewWorker(storage, ollamaURL, concurrency)
	server := &Server{storage: storage, worker: worker}

	r := mux.NewRouter()
	r.HandleFunc("/jobs", server.CreateJob).Methods("POST")
	r.HandleFunc("/jobs/{job_id}/events", server.GetEvents).Methods("GET")
	r.HandleFunc("/jobs/{job_id}/status", server.GetStatus).Methods("GET")
	r.HandleFunc("/jobs/{job_id}/cancel", server.CancelJob).Methods("POST")
	r.HandleFunc("/stats", server.GetWorkerStats).Methods("GET")

	port := os.Getenv("PORT")
	if port == "" {
		port = "5345"
	}

	log.Printf("Worker listening on :%s (concurrency: %d, max queue: %d)", port, concurrency, concurrency*2)
	log.Fatal(http.ListenAndServe(":"+port, r))
}

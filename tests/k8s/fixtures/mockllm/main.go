// mockllm is a tiny OpenAI-compatible mock LLM server used by the k8s test
// fixtures. It returns deterministic canned responses and exposes ground-truth
// token totals at GET /__stats and a reset at POST /__reset.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// Trimmed pricing/model-parameters datasheet (~6 large US providers + a
// self-hosted openai-compatible entry), served so Bifrost can point its pricing
// URLs here instead of fetching the full ~1000-entry sheet from the network at
// startup (avoids the dependency and the pricing-sync churn).
//
//go:embed datasheet/pricing.json
var pricingDatasheet []byte

//go:embed datasheet/model-parameters.json
var modelParametersDatasheet []byte

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

type chatChoice struct {
	Index        int `json:"index"`
	Message      struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   usage        `json:"usage"`
}

type stats struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

var (
	requests         atomic.Int64
	promptTokens     atomic.Int64
	completionTokens atomic.Int64
)

// Per-request token counts. Deterministic so the ledger reconciles exactly.
const (
	canonicalPromptTokens     = 10
	canonicalCompletionTokens = 20
)

func main() {
	addr := os.Getenv("MOCKLLM_ADDR")
	if addr == "" {
		addr = ":8000"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/v1/messages", handleAnthropicMessages)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/datasheet", serveJSONBytes(pricingDatasheet))
	mux.HandleFunc("/datasheet/model-parameters", serveJSONBytes(modelParametersDatasheet))
	mux.HandleFunc("/__stats", handleStats)
	mux.HandleFunc("/__reset", handleReset)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	log.Printf("mock-llm listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	requests.Add(1)
	promptTokens.Add(canonicalPromptTokens)
	completionTokens.Add(canonicalCompletionTokens)

	resp := chatResponse{
		ID:      fmt.Sprintf("mock-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []chatChoice{{
			Index: 0,
			Message: struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}{Role: "assistant", Content: "mock response"},
			FinishReason: "stop",
		}},
		Usage: usage{
			PromptTokens:     canonicalPromptTokens,
			CompletionTokens: canonicalCompletionTokens,
			TotalTokens:      canonicalPromptTokens + canonicalCompletionTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// Minimal Anthropic-compatible response shape for when tests route via the
// anthropic provider against this mock.
func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requests.Add(1)
	promptTokens.Add(canonicalPromptTokens)
	completionTokens.Add(canonicalCompletionTokens)
	body := map[string]any{
		"id":   fmt.Sprintf("mock-%d", time.Now().UnixNano()),
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{
			{"type": "text", "text": "mock response"},
		},
		"model":       "claude-mock",
		"stop_reason": "end_turn",
		"usage": map[string]int{
			"input_tokens":  canonicalPromptTokens,
			"output_tokens": canonicalCompletionTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// handleModels advertises a small set of OpenAI-shaped model names so
// Bifrost's provider discovery (GET /v1/models on key-add) succeeds and
// populates the model catalog. Without this endpoint, Bifrost falls back to
// the static datasheet, which doesn't include "gpt-4o-mock" — every inference
// request then returns DecisionModelBlocked (403).
func handleModels(w http.ResponseWriter, _ *http.Request) {
	body := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "gpt-4o", "object": "model", "created": time.Now().Unix(), "owned_by": "openai"},
			{"id": "gpt-4o-mini", "object": "model", "created": time.Now().Unix(), "owned_by": "openai"},
			{"id": "gpt-4o-mock", "object": "model", "created": time.Now().Unix(), "owned_by": "mock"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// serveJSONBytes returns a handler that writes the given pre-marshaled JSON.
// Used to serve the embedded pricing / model-parameters datasheet.
func serveJSONBytes(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func handleStats(w http.ResponseWriter, _ *http.Request) {
	s := stats{
		Requests:         requests.Load(),
		PromptTokens:     promptTokens.Load(),
		CompletionTokens: completionTokens.Load(),
		TotalTokens:      promptTokens.Load() + completionTokens.Load(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}

func handleReset(w http.ResponseWriter, _ *http.Request) {
	requests.Store(0)
	promptTokens.Store(0)
	completionTokens.Store(0)
	w.WriteHeader(http.StatusNoContent)
}

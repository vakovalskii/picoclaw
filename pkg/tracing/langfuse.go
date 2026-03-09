// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// Tracer sends LLM call traces to Langfuse.
type Tracer struct {
	baseURL   string
	publicKey string
	secretKey string
	client    *http.Client

	mu    sync.Mutex
	batch []event
	done  chan struct{}
}

// Config holds Langfuse connection settings.
type Config struct {
	Enabled   bool   `json:"enabled"`
	SecretKey string `json:"secret_key"`
	PublicKey string `json:"public_key"`
	BaseURL   string `json:"base_url"`
}

// UsageInfo mirrors token usage from the LLM response.
type UsageInfo struct {
	PromptTokens     int `json:"input"`
	CompletionTokens int `json:"output"`
	TotalTokens      int `json:"total"`
}

// TraceParams starts a new trace (one per user message / agent turn).
type TraceParams struct {
	ID       string
	Name     string
	Input    any
	AgentID  string
	Channel  string
	ChatID   string
	Tags     []string
}

// GenerationParams records a single LLM call within a trace.
type GenerationParams struct {
	ID       string
	TraceID  string
	Name     string
	Model    string
	Input    any
	Output   any
	StartAt  time.Time
	EndAt    time.Time
	Usage    *UsageInfo
	Metadata map[string]any
}

// SpanParams records a span (e.g. tool execution) within a trace.
type SpanParams struct {
	ID                  string
	TraceID             string
	ParentObservationID string // links span to parent generation
	Name                string
	Input               any
	Output              any
	StartAt             time.Time
	EndAt               time.Time
}

// UpdateTraceParams updates a trace with final output.
type UpdateTraceParams struct {
	ID     string
	Output any
}

type event struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Body      any    `json:"body"`
}

type traceBody struct {
	ID        string         `json:"id"`
	Timestamp string         `json:"timestamp,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     any            `json:"input,omitempty"`
	Output    any            `json:"output,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Tags      []string       `json:"tags,omitempty"`
}

type generationBody struct {
	ID           string         `json:"id"`
	TraceID      string         `json:"traceId"`
	Name         string         `json:"name,omitempty"`
	Model        string         `json:"model,omitempty"`
	Input        any            `json:"input,omitempty"`
	Output       any            `json:"output,omitempty"`
	StartTime    string         `json:"startTime,omitempty"`
	EndTime      string         `json:"endTime,omitempty"`
	UsageDetails *UsageInfo     `json:"usageDetails,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type spanBody struct {
	ID                  string `json:"id"`
	TraceID             string `json:"traceId"`
	ParentObservationID string `json:"parentObservationId,omitempty"`
	Name                string `json:"name,omitempty"`
	Input               any    `json:"input,omitempty"`
	Output              any    `json:"output,omitempty"`
	StartTime           string `json:"startTime,omitempty"`
	EndTime             string `json:"endTime,omitempty"`
}

type ingestionRequest struct {
	Batch []event `json:"batch"`
}

const (
	maxBatchSize  = 30
	flushInterval = 3 * time.Second
)

// New creates a new Langfuse tracer. Returns nil if config is disabled or incomplete.
func New(cfg Config) *Tracer {
	if !cfg.Enabled || cfg.SecretKey == "" || cfg.PublicKey == "" || cfg.BaseURL == "" {
		return nil
	}
	t := &Tracer{
		baseURL:   cfg.BaseURL,
		publicKey: cfg.PublicKey,
		secretKey: cfg.SecretKey,
		client:    &http.Client{Timeout: 10 * time.Second},
		done:      make(chan struct{}),
	}
	go t.flusher()
	return t
}

// CreateTrace starts a new trace for an agent turn.
func (t *Tracer) CreateTrace(params TraceParams) {
	if t == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tags := append([]string{"picoclaw"}, params.Tags...)
	t.addEvent(event{
		ID:        params.ID,
		Type:      "trace-create",
		Timestamp: now,
		Body: traceBody{
			ID:        params.ID,
			Timestamp: now,
			Name:      params.Name,
			Input:     params.Input,
			Metadata: map[string]any{
				"agent_id": params.AgentID,
				"channel":  params.Channel,
				"chat_id":  params.ChatID,
			},
			Tags: tags,
		},
	})
}

// UpdateTrace updates a trace with final output.
func (t *Tracer) UpdateTrace(params UpdateTraceParams) {
	if t == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	t.addEvent(event{
		ID:        params.ID + "-upd",
		Type:      "trace-create",
		Timestamp: now,
		Body: traceBody{
			ID:     params.ID,
			Output: params.Output,
		},
	})
}

// CreateGeneration records a single LLM call.
func (t *Tracer) CreateGeneration(params GenerationParams) {
	if t == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	t.addEvent(event{
		ID:        params.ID,
		Type:      "generation-create",
		Timestamp: now,
		Body: generationBody{
			ID:           params.ID,
			TraceID:      params.TraceID,
			Name:         params.Name,
			Model:        params.Model,
			Input:        params.Input,
			Output:       params.Output,
			StartTime:    params.StartAt.UTC().Format(time.RFC3339Nano),
			EndTime:      params.EndAt.UTC().Format(time.RFC3339Nano),
			UsageDetails: params.Usage,
			Metadata:     params.Metadata,
		},
	})
}

// CreateSpan records a span (tool call, processing step).
func (t *Tracer) CreateSpan(params SpanParams) {
	if t == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	t.addEvent(event{
		ID:        params.ID,
		Type:      "span-create",
		Timestamp: now,
		Body: spanBody{
			ID:                  params.ID,
			TraceID:             params.TraceID,
			ParentObservationID: params.ParentObservationID,
			Name:                params.Name,
			Input:               params.Input,
			Output:              params.Output,
			StartTime:           params.StartAt.UTC().Format(time.RFC3339Nano),
			EndTime:             params.EndAt.UTC().Format(time.RFC3339Nano),
		},
	})
}

// Shutdown flushes remaining events and stops the background flusher.
func (t *Tracer) Shutdown() {
	if t == nil {
		return
	}
	close(t.done)
	t.flush()
}

func (t *Tracer) addEvent(evt event) {
	t.mu.Lock()
	t.batch = append(t.batch, evt)
	needsFlush := len(t.batch) >= maxBatchSize
	t.mu.Unlock()
	if needsFlush {
		go t.flush()
	}
}

func (t *Tracer) flusher() {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.flush()
		case <-t.done:
			return
		}
	}
}

func (t *Tracer) flush() {
	t.mu.Lock()
	if len(t.batch) == 0 {
		t.mu.Unlock()
		return
	}
	events := t.batch
	t.batch = nil
	t.mu.Unlock()

	body, err := json.Marshal(ingestionRequest{Batch: events})
	if err != nil {
		logger.ErrorCF("tracing", "Failed to marshal Langfuse batch", map[string]any{"error": err.Error()})
		return
	}

	url := fmt.Sprintf("%s/api/public/ingestion", t.baseURL)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		logger.ErrorCF("tracing", "Failed to create Langfuse request", map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(t.publicKey, t.secretKey)

	resp, err := t.client.Do(req)
	if err != nil {
		logger.WarnCF("tracing", "Failed to send Langfuse batch", map[string]any{
			"error":        err.Error(),
			"events_count": len(events),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		logger.WarnCF("tracing", "Langfuse ingestion error", map[string]any{
			"status":        resp.StatusCode,
			"events_count":  len(events),
			"response_body": string(respBody),
		})
		return
	}

	// Collect event types for the debug log
	types := make(map[string]int, len(events))
	for _, e := range events {
		types[e.Type]++
	}
	logger.DebugCF("tracing", "Langfuse batch sent", map[string]any{
		"events_count": len(events),
		"event_types":  types,
	})
}

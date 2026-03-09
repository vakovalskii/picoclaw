// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package tracing

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// capturedBatch stores raw ingestion requests from a mock Langfuse server.
type capturedBatch struct {
	mu       sync.Mutex
	requests []ingestionRequest
}

func (c *capturedBatch) add(req ingestionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
}

func (c *capturedBatch) all() []ingestionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ingestionRequest, len(c.requests))
	copy(out, c.requests)
	return out
}

func (c *capturedBatch) allEvents() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var events []map[string]any
	for _, req := range c.requests {
		for _, evt := range req.Batch {
			// Re-marshal to get a generic map (body is typed struct in memory)
			raw, _ := json.Marshal(evt)
			var m map[string]any
			json.Unmarshal(raw, &m)
			events = append(events, m)
		}
	}
	return events
}

// startMockLangfuse creates an httptest server that captures ingestion batches.
func startMockLangfuse(t *testing.T) (*httptest.Server, *capturedBatch) {
	t.Helper()
	cap := &capturedBatch{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/ingestion" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "method not allowed", 405)
			return
		}
		// Verify basic auth
		user, pass, ok := r.BasicAuth()
		if !ok || user == "" || pass == "" {
			t.Errorf("missing basic auth")
			http.Error(w, "unauthorized", 401)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req ingestionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("invalid JSON: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		cap.add(req)
		w.WriteHeader(200)
		w.Write([]byte(`{"successes":[],"errors":[]}`))
	}))
	return srv, cap
}

func newTestTracer(t *testing.T, baseURL string) *Tracer {
	t.Helper()
	tr := New(Config{
		Enabled:   true,
		SecretKey: "sk-test",
		PublicKey: "pk-test",
		BaseURL:   baseURL,
	})
	if tr == nil {
		t.Fatal("expected non-nil tracer")
	}
	return tr
}

func TestNew_DisabledReturnsNil(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"disabled", Config{Enabled: false, SecretKey: "sk", PublicKey: "pk", BaseURL: "http://x"}},
		{"missing secret", Config{Enabled: true, SecretKey: "", PublicKey: "pk", BaseURL: "http://x"}},
		{"missing public", Config{Enabled: true, SecretKey: "sk", PublicKey: "", BaseURL: "http://x"}},
		{"missing url", Config{Enabled: true, SecretKey: "sk", PublicKey: "pk", BaseURL: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := New(tt.cfg)
			if tr != nil {
				tr.Shutdown()
				t.Fatalf("expected nil tracer for %s", tt.name)
			}
		})
	}
}

func TestNew_EnabledReturnsTracer(t *testing.T) {
	srv, _ := startMockLangfuse(t)
	defer srv.Close()

	tr := newTestTracer(t, srv.URL)
	defer tr.Shutdown()

	if tr.baseURL != srv.URL {
		t.Errorf("expected baseURL %s, got %s", srv.URL, tr.baseURL)
	}
}

func TestNilTracer_NoPanic(t *testing.T) {
	var tr *Tracer
	// All methods must be nil-safe
	tr.CreateTrace(TraceParams{ID: "t1"})
	tr.CreateGeneration(GenerationParams{ID: "g1"})
	tr.CreateSpan(SpanParams{ID: "s1"})
	tr.UpdateTrace(UpdateTraceParams{ID: "t1"})
	tr.Shutdown()
}

// TestFullAgentCycle simulates a complete agent turn:
// trace-create → generation-create → span-create (tool) → trace-update
// and verifies the JSON structure matches Langfuse ingestion API format.
func TestFullAgentCycle(t *testing.T) {
	srv, cap := startMockLangfuse(t)
	defer srv.Close()

	tr := newTestTracer(t, srv.URL)

	traceID := "trace-abc-123"
	now := time.Now()

	// 1. Create trace (agent turn start)
	tr.CreateTrace(TraceParams{
		ID:      traceID,
		Name:    "agent/default",
		Input:   "Hello, what's the weather?",
		AgentID: "default",
		Channel: "telegram",
		ChatID:  "chat-42",
		Tags:    []string{"default", "telegram"},
	})

	// 2. Create generation (LLM call)
	tr.CreateGeneration(GenerationParams{
		ID:      traceID + "-gen-1",
		TraceID: traceID,
		Name:    "llm/gpt-5.3-codex/iter-1",
		Model:   "gpt-5.3-codex",
		Input:   []map[string]any{{"role": "user", "content": "Hello, what's the weather?"}},
		Output:  "I'll check the weather for you.",
		StartAt: now,
		EndAt:   now.Add(2 * time.Second),
		Usage: &UsageInfo{
			PromptTokens:     150,
			CompletionTokens: 30,
			TotalTokens:      180,
		},
		Metadata: map[string]any{
			"iteration":  1,
			"tool_calls": 1,
		},
	})

	// 3. Create span (tool call — child of generation)
	genID := traceID + "-gen-1"
	tr.CreateSpan(SpanParams{
		ID:                  traceID + "-tool-1-call_abc",
		TraceID:             traceID,
		ParentObservationID: genID,
		Name:                "tool/web_fetch",
		Input:               map[string]any{"url": "https://weather.example.com"},
		Output:              "Temperature: 15°C, sunny",
		StartAt:             now.Add(2 * time.Second),
		EndAt:               now.Add(3 * time.Second),
	})

	// 4. Second generation (after tool result)
	tr.CreateGeneration(GenerationParams{
		ID:      traceID + "-gen-2",
		TraceID: traceID,
		Name:    "llm/gpt-5.3-codex/iter-2",
		Model:   "gpt-5.3-codex",
		Input:   []map[string]any{{"role": "user", "content": "Hello, what's the weather?"}},
		Output:  "The weather is 15°C and sunny!",
		StartAt: now.Add(3 * time.Second),
		EndAt:   now.Add(4 * time.Second),
		Usage: &UsageInfo{
			PromptTokens:     200,
			CompletionTokens: 20,
			TotalTokens:      220,
		},
		Metadata: map[string]any{
			"iteration":  2,
			"tool_calls": 0,
		},
	})

	// 5. Update trace with final output
	tr.UpdateTrace(UpdateTraceParams{
		ID:     traceID,
		Output: "The weather is 15°C and sunny!",
	})

	// Flush and shutdown
	tr.Shutdown()

	// Verify events
	events := cap.allEvents()
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d: %+v", len(events), events)
	}

	// Verify event types in order
	expectedTypes := []string{
		"trace-create",
		"generation-create",
		"span-create",
		"generation-create",
		"trace-create", // update is also trace-create with same ID
	}
	for i, evt := range events {
		gotType, _ := evt["type"].(string)
		if gotType != expectedTypes[i] {
			t.Errorf("event[%d]: expected type %q, got %q", i, expectedTypes[i], gotType)
		}
	}

	// Verify all events have required fields: id, type, timestamp, body
	for i, evt := range events {
		for _, field := range []string{"id", "type", "timestamp", "body"} {
			if _, ok := evt[field]; !ok {
				t.Errorf("event[%d]: missing required field %q", i, field)
			}
		}
	}

	// Verify trace body structure
	traceEvt := events[0]
	body := traceEvt["body"].(map[string]any)
	if body["id"] != traceID {
		t.Errorf("trace body id: expected %q, got %v", traceID, body["id"])
	}
	if body["name"] != "agent/default" {
		t.Errorf("trace body name: expected %q, got %v", "agent/default", body["name"])
	}
	if body["input"] != "Hello, what's the weather?" {
		t.Errorf("trace body input: expected user message, got %v", body["input"])
	}
	metadata := body["metadata"].(map[string]any)
	if metadata["agent_id"] != "default" {
		t.Errorf("trace metadata agent_id: expected %q, got %v", "default", metadata["agent_id"])
	}
	if metadata["channel"] != "telegram" {
		t.Errorf("trace metadata channel: expected %q, got %v", "telegram", metadata["channel"])
	}
	tags := body["tags"].([]any)
	if len(tags) < 1 || tags[0] != "picoclaw" {
		t.Errorf("trace tags: expected first tag 'picoclaw', got %v", tags)
	}

	// Verify generation body structure
	genEvt := events[1]
	genBody := genEvt["body"].(map[string]any)
	if genBody["traceId"] != traceID {
		t.Errorf("generation traceId: expected %q, got %v", traceID, genBody["traceId"])
	}
	if genBody["model"] != "gpt-5.3-codex" {
		t.Errorf("generation model: expected %q, got %v", "gpt-5.3-codex", genBody["model"])
	}
	if genBody["startTime"] == "" || genBody["startTime"] == nil {
		t.Error("generation missing startTime")
	}
	if genBody["endTime"] == "" || genBody["endTime"] == nil {
		t.Error("generation missing endTime")
	}
	// Verify usageDetails (not "usage")
	usage := genBody["usageDetails"].(map[string]any)
	if usage["input"] != float64(150) {
		t.Errorf("generation usage input: expected 150, got %v", usage["input"])
	}
	if usage["output"] != float64(30) {
		t.Errorf("generation usage output: expected 30, got %v", usage["output"])
	}
	if usage["total"] != float64(180) {
		t.Errorf("generation usage total: expected 180, got %v", usage["total"])
	}
	// Verify metadata
	genMeta := genBody["metadata"].(map[string]any)
	if genMeta["iteration"] != float64(1) {
		t.Errorf("generation metadata iteration: expected 1, got %v", genMeta["iteration"])
	}
	if genMeta["tool_calls"] != float64(1) {
		t.Errorf("generation metadata tool_calls: expected 1, got %v", genMeta["tool_calls"])
	}

	// Verify span body structure
	spanEvt := events[2]
	spanBody := spanEvt["body"].(map[string]any)
	if spanBody["traceId"] != traceID {
		t.Errorf("span traceId: expected %q, got %v", traceID, spanBody["traceId"])
	}
	if spanBody["name"] != "tool/web_fetch" {
		t.Errorf("span name: expected %q, got %v", "tool/web_fetch", spanBody["name"])
	}
	if spanBody["parentObservationId"] != genID {
		t.Errorf("span parentObservationId: expected %q, got %v", genID, spanBody["parentObservationId"])
	}
	if spanBody["startTime"] == "" || spanBody["startTime"] == nil {
		t.Error("span missing startTime")
	}
	if spanBody["endTime"] == "" || spanBody["endTime"] == nil {
		t.Error("span missing endTime")
	}
	// Span input should be the tool arguments
	spanInput := spanBody["input"].(map[string]any)
	if spanInput["url"] != "https://weather.example.com" {
		t.Errorf("span input url: expected weather URL, got %v", spanInput["url"])
	}
	if spanBody["output"] != "Temperature: 15°C, sunny" {
		t.Errorf("span output: expected weather result, got %v", spanBody["output"])
	}

	// Verify trace update (last event)
	updEvt := events[4]
	updBody := updEvt["body"].(map[string]any)
	if updBody["id"] != traceID {
		t.Errorf("update trace id: expected %q, got %v", traceID, updBody["id"])
	}
	if updBody["output"] != "The weather is 15°C and sunny!" {
		t.Errorf("update trace output: expected final response, got %v", updBody["output"])
	}
	// Update event ID should have "-upd" suffix
	if updEvt["id"] != traceID+"-upd" {
		t.Errorf("update event id: expected %q, got %v", traceID+"-upd", updEvt["id"])
	}
}

func TestBatchFlushOnLimit(t *testing.T) {
	srv, cap := startMockLangfuse(t)
	defer srv.Close()

	tr := newTestTracer(t, srv.URL)

	// Add maxBatchSize events to trigger auto-flush
	for i := 0; i < maxBatchSize+5; i++ {
		tr.CreateSpan(SpanParams{
			ID:      "span-" + string(rune('a'+i%26)) + "-" + time.Now().Format("150405.000"),
			TraceID: "trace-batch-test",
			Name:    "tool/test",
			StartAt: time.Now(),
			EndAt:   time.Now(),
		})
	}

	// Give goroutine time to flush
	time.Sleep(200 * time.Millisecond)
	tr.Shutdown()

	reqs := cap.all()
	if len(reqs) == 0 {
		t.Fatal("expected at least one flush request")
	}

	// Total events across all batches should be maxBatchSize+5
	totalEvents := 0
	for _, req := range reqs {
		totalEvents += len(req.Batch)
	}
	if totalEvents != maxBatchSize+5 {
		t.Errorf("expected %d total events, got %d", maxBatchSize+5, totalEvents)
	}
}

func TestFlushEmptyBatch(t *testing.T) {
	srv, cap := startMockLangfuse(t)
	defer srv.Close()

	tr := newTestTracer(t, srv.URL)
	// Shutdown without adding any events
	tr.Shutdown()

	reqs := cap.all()
	if len(reqs) != 0 {
		t.Errorf("expected no requests for empty batch, got %d", len(reqs))
	}
}

func TestBasicAuthSent(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		w.WriteHeader(200)
		w.Write([]byte(`{"successes":[],"errors":[]}`))
	}))
	defer srv.Close()

	tr := New(Config{
		Enabled:   true,
		SecretKey: "my-secret",
		PublicKey: "my-public",
		BaseURL:   srv.URL,
	})
	tr.CreateTrace(TraceParams{ID: "t1", Name: "test"})
	tr.Shutdown()

	if gotUser != "my-public" {
		t.Errorf("expected basic auth user %q, got %q", "my-public", gotUser)
	}
	if gotPass != "my-secret" {
		t.Errorf("expected basic auth pass %q, got %q", "my-secret", gotPass)
	}
}

// TestJSONFormat_LangfuseV3 verifies raw JSON matches what Langfuse v3 API expects.
func TestJSONFormat_LangfuseV3(t *testing.T) {
	srv, cap := startMockLangfuse(t)
	defer srv.Close()

	tr := newTestTracer(t, srv.URL)

	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	tr.CreateGeneration(GenerationParams{
		ID:      "gen-1",
		TraceID: "trace-1",
		Name:    "llm/test",
		Model:   "gpt-5.3-codex",
		Input:   "test input",
		Output:  "test output",
		StartAt: now,
		EndAt:   now.Add(time.Second),
		Usage:   &UsageInfo{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	})
	tr.Shutdown()

	reqs := cap.all()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}

	// Re-marshal to raw JSON and verify field names
	raw, err := json.Marshal(reqs[0])
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	json.Unmarshal(raw, &parsed)

	batch := parsed["batch"].([]any)
	evt := batch[0].(map[string]any)

	// Event-level required fields
	if evt["type"] != "generation-create" {
		t.Errorf("expected type generation-create, got %v", evt["type"])
	}

	body := evt["body"].(map[string]any)

	// Langfuse uses "traceId" not "trace_id"
	if _, ok := body["traceId"]; !ok {
		t.Error("body missing 'traceId' field (Langfuse uses camelCase)")
	}

	// Langfuse uses "usageDetails" not "usage"
	if _, ok := body["usageDetails"]; !ok {
		t.Error("body missing 'usageDetails' field")
	}
	if _, ok := body["usage"]; ok {
		t.Error("body has 'usage' field — Langfuse v3 uses 'usageDetails'")
	}

	// Langfuse uses "startTime"/"endTime" not "start_time"/"end_time"
	if _, ok := body["startTime"]; !ok {
		t.Error("body missing 'startTime' field")
	}
	if _, ok := body["endTime"]; !ok {
		t.Error("body missing 'endTime' field")
	}

	// UsageDetails uses "input"/"output"/"total" per Langfuse v3
	ud := body["usageDetails"].(map[string]any)
	for _, key := range []string{"input", "output", "total"} {
		if _, ok := ud[key]; !ok {
			t.Errorf("usageDetails missing %q field", key)
		}
	}
}

func TestConcurrentEventAdds(t *testing.T) {
	srv, cap := startMockLangfuse(t)
	defer srv.Close()

	tr := newTestTracer(t, srv.URL)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr.CreateSpan(SpanParams{
				ID:      fmt.Sprintf("span-concurrent-%d", i),
				TraceID: "trace-concurrent",
				Name:    "tool/concurrent",
				StartAt: time.Now(),
				EndAt:   time.Now(),
			})
		}(i)
	}
	wg.Wait()
	tr.Shutdown()

	events := cap.allEvents()
	if len(events) != n {
		t.Errorf("expected %d events, got %d", n, len(events))
	}
}

// TestRawJSON_AgentCycle captures and prints the exact JSON that goes to Langfuse.
// Run with -v to inspect the payload format visually.
func TestRawJSON_AgentCycle(t *testing.T) {
	var rawBodies [][]byte
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		rawBodies = append(rawBodies, body)
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"successes":[],"errors":[]}`))
	}))
	defer srv.Close()

	tr := newTestTracer(t, srv.URL)
	now := time.Date(2026, 3, 9, 14, 30, 0, 0, time.UTC)

	traceID := "trace-e2e-test"
	genID := traceID + "-gen-1"

	// Full agent cycle
	tr.CreateTrace(TraceParams{
		ID: traceID, Name: "agent/default",
		Input: "Какая погода?", AgentID: "default",
		Channel: "telegram", ChatID: "chat-1", Tags: []string{"default"},
	})
	tr.CreateGeneration(GenerationParams{
		ID: genID, TraceID: traceID,
		Name: "llm/gpt-5.3-codex/iter-1", Model: "gpt-5.3-codex",
		Input:  []map[string]any{{"role": "user", "content": "Какая погода?"}},
		Output: "Сейчас проверю.",
		StartAt: now, EndAt: now.Add(time.Second),
		Usage:    &UsageInfo{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60},
		Metadata: map[string]any{"iteration": 1, "tool_calls": 1},
	})
	tr.CreateSpan(SpanParams{
		ID: traceID + "-tool-1-call_xyz", TraceID: traceID,
		ParentObservationID: genID,
		Name: "tool/web_fetch",
		Input:  map[string]any{"url": "https://api.weather.com/moscow"},
		Output: `{"temp": 5, "desc": "cloudy"}`,
		StartAt: now.Add(time.Second), EndAt: now.Add(2 * time.Second),
	})
	tr.CreateGeneration(GenerationParams{
		ID: traceID + "-gen-2", TraceID: traceID,
		Name: "llm/gpt-5.3-codex/iter-2", Model: "gpt-5.3-codex",
		Input:  []map[string]any{{"role": "user", "content": "Какая погода?"}, {"role": "assistant", "content": "Сейчас проверю."}},
		Output: "В Москве 5°C, облачно.",
		StartAt: now.Add(2 * time.Second), EndAt: now.Add(3 * time.Second),
		Usage:    &UsageInfo{PromptTokens: 100, CompletionTokens: 15, TotalTokens: 115},
		Metadata: map[string]any{"iteration": 2, "tool_calls": 0},
	})
	tr.UpdateTrace(UpdateTraceParams{ID: traceID, Output: "В Москве 5°C, облачно."})

	tr.Shutdown()

	if len(rawBodies) == 0 {
		t.Fatal("no requests captured")
	}

	// Pretty-print the JSON for visual inspection
	for i, body := range rawBodies {
		var pretty json.RawMessage = body
		formatted, _ := json.MarshalIndent(pretty, "", "  ")
		t.Logf("=== Langfuse batch request %d ===\n%s", i+1, string(formatted))
	}

	// Parse and verify structure
	var req ingestionRequest
	if err := json.Unmarshal(rawBodies[0], &req); err != nil {
		t.Fatalf("failed to parse batch: %v", err)
	}

	if len(req.Batch) != 5 {
		t.Fatalf("expected 5 events in batch, got %d", len(req.Batch))
	}

	// Verify event order: trace, gen, span, gen, trace-update
	wantTypes := []string{"trace-create", "generation-create", "span-create", "generation-create", "trace-create"}
	for i, evt := range req.Batch {
		if evt.Type != wantTypes[i] {
			t.Errorf("event[%d] type: want %q, got %q", i, wantTypes[i], evt.Type)
		}
		if evt.ID == "" {
			t.Errorf("event[%d] missing id", i)
		}
		if evt.Timestamp == "" {
			t.Errorf("event[%d] missing timestamp", i)
		}
	}
}

// TestGenerationParentage verifies spans and generations reference the trace.
func TestGenerationParentage(t *testing.T) {
	srv, cap := startMockLangfuse(t)
	defer srv.Close()

	tr := newTestTracer(t, srv.URL)

	traceID := "parent-trace-999"
	tr.CreateTrace(TraceParams{ID: traceID, Name: "test"})
	tr.CreateGeneration(GenerationParams{
		ID:      "gen-child",
		TraceID: traceID,
		Name:    "llm/test",
		StartAt: time.Now(),
		EndAt:   time.Now(),
	})
	tr.CreateSpan(SpanParams{
		ID:      "span-child",
		TraceID: traceID,
		Name:    "tool/test",
		StartAt: time.Now(),
		EndAt:   time.Now(),
	})
	tr.Shutdown()

	events := cap.allEvents()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Generation and span should reference the trace
	genBody := events[1]["body"].(map[string]any)
	if genBody["traceId"] != traceID {
		t.Errorf("generation traceId: expected %q, got %v", traceID, genBody["traceId"])
	}
	spanBody := events[2]["body"].(map[string]any)
	if spanBody["traceId"] != traceID {
		t.Errorf("span traceId: expected %q, got %v", traceID, spanBody["traceId"])
	}
}

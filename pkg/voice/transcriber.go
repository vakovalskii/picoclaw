package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/utils"
)

type Transcriber interface {
	Name() string
	Transcribe(ctx context.Context, audioFilePath string) (*TranscriptionResponse, error)
}

type GroqTranscriber struct {
	apiKey     string
	apiBase    string
	httpClient *http.Client
}

type TranscriptionResponse struct {
	Text     string  `json:"text"`
	Language string  `json:"language,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

func NewGroqTranscriber(apiKey string) *GroqTranscriber {
	logger.DebugCF("voice", "Creating Groq transcriber", map[string]any{"has_api_key": apiKey != ""})

	apiBase := "https://api.groq.com/openai/v1"
	return &GroqTranscriber{
		apiKey:  apiKey,
		apiBase: apiBase,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (t *GroqTranscriber) Transcribe(ctx context.Context, audioFilePath string) (*TranscriptionResponse, error) {
	logger.InfoCF("voice", "Starting transcription", map[string]any{"audio_file": audioFilePath})

	audioFile, err := os.Open(audioFilePath)
	if err != nil {
		logger.ErrorCF("voice", "Failed to open audio file", map[string]any{"path": audioFilePath, "error": err})
		return nil, fmt.Errorf("failed to open audio file: %w", err)
	}
	defer audioFile.Close()

	fileInfo, err := audioFile.Stat()
	if err != nil {
		logger.ErrorCF("voice", "Failed to get file info", map[string]any{"path": audioFilePath, "error": err})
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}

	logger.DebugCF("voice", "Audio file details", map[string]any{
		"size_bytes": fileInfo.Size(),
		"file_name":  filepath.Base(audioFilePath),
	})

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	part, err := writer.CreateFormFile("file", filepath.Base(audioFilePath))
	if err != nil {
		logger.ErrorCF("voice", "Failed to create form file", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}

	copied, err := io.Copy(part, audioFile)
	if err != nil {
		logger.ErrorCF("voice", "Failed to copy file content", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to copy file content: %w", err)
	}

	logger.DebugCF("voice", "File copied to request", map[string]any{"bytes_copied": copied})

	if err = writer.WriteField("model", "whisper-large-v3"); err != nil {
		logger.ErrorCF("voice", "Failed to write model field", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to write model field: %w", err)
	}

	if err = writer.WriteField("response_format", "json"); err != nil {
		logger.ErrorCF("voice", "Failed to write response_format field", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to write response_format field: %w", err)
	}

	if err = writer.Close(); err != nil {
		logger.ErrorCF("voice", "Failed to close multipart writer", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	url := t.apiBase + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, &requestBody)
	if err != nil {
		logger.ErrorCF("voice", "Failed to create request", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+t.apiKey)

	logger.DebugCF("voice", "Sending transcription request to Groq API", map[string]any{
		"url":                url,
		"request_size_bytes": requestBody.Len(),
		"file_size_bytes":    fileInfo.Size(),
	})

	resp, err := t.httpClient.Do(req)
	if err != nil {
		logger.ErrorCF("voice", "Failed to send request", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.ErrorCF("voice", "Failed to read response", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		logger.ErrorCF("voice", "API error", map[string]any{
			"status_code": resp.StatusCode,
			"response":    string(body),
		})
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	logger.DebugCF("voice", "Received response from Groq API", map[string]any{
		"status_code":         resp.StatusCode,
		"response_size_bytes": len(body),
	})

	var result TranscriptionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		logger.ErrorCF("voice", "Failed to unmarshal response", map[string]any{"error": err})
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	logger.InfoCF("voice", "Transcription completed successfully", map[string]any{
		"text_length":           len(result.Text),
		"language":              result.Language,
		"duration_seconds":      result.Duration,
		"transcription_preview": utils.Truncate(result.Text, 50),
	})

	return &result, nil
}

func (t *GroqTranscriber) Name() string {
	return "groq"
}

// WhisperTranscriber works with any OpenAI-compatible Whisper endpoint.
type WhisperTranscriber struct {
	apiBase    string
	apiKey     string
	model      string
	language   string
	httpClient *http.Client
}

func NewWhisperTranscriber(apiBase, apiKey, model, language string) *WhisperTranscriber {
	if model == "" {
		model = "whisper-1"
	}
	return &WhisperTranscriber{
		apiBase:    strings.TrimRight(apiBase, "/"),
		apiKey:     apiKey,
		model:      model,
		language:   language,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

func (t *WhisperTranscriber) Name() string {
	return "whisper"
}

func (t *WhisperTranscriber) Transcribe(ctx context.Context, audioFilePath string) (*TranscriptionResponse, error) {
	logger.InfoCF("voice", "Whisper transcription starting", map[string]any{
		"audio_file": audioFilePath,
		"api_base":   t.apiBase,
		"model":      t.model,
	})

	audioFile, err := os.Open(audioFilePath)
	if err != nil {
		return nil, fmt.Errorf("open audio file: %w", err)
	}
	defer audioFile.Close()

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	part, err := writer.CreateFormFile("file", filepath.Base(audioFilePath))
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err = io.Copy(part, audioFile); err != nil {
		return nil, fmt.Errorf("copy file content: %w", err)
	}
	_ = writer.WriteField("model", t.model)
	_ = writer.WriteField("response_format", "json")
	if t.language != "" {
		_ = writer.WriteField("language", t.language)
	}
	if err = writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	url := t.apiBase + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, &requestBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		logger.ErrorCF("voice", "Whisper API error", map[string]any{
			"status": resp.StatusCode,
			"body":   string(body),
		})
		return nil, fmt.Errorf("whisper API error (status %d): %s", resp.StatusCode, string(body))
	}

	var result TranscriptionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	logger.InfoCF("voice", "Whisper transcription completed", map[string]any{
		"text_length": len(result.Text),
		"language":    result.Language,
		"duration":    result.Duration,
		"preview":     utils.Truncate(result.Text, 80),
	})
	return &result, nil
}

// DetectTranscriber inspects cfg and returns the appropriate Transcriber, or
// nil if no supported transcription provider is configured.
func DetectTranscriber(cfg *config.Config) Transcriber {
	// Explicit voice.transcription config takes priority.
	tc := cfg.Voice.Transcription
	if tc.Enabled && tc.APIBase != "" {
		return NewWhisperTranscriber(tc.APIBase, tc.APIKey, tc.Model, tc.Language)
	}
	// Direct Groq provider config.
	if key := cfg.Providers.Groq.APIKey; key != "" {
		return NewGroqTranscriber(key)
	}
	// Fall back to any model-list entry that uses the groq/ protocol.
	for _, mc := range cfg.ModelList {
		if strings.HasPrefix(mc.Model, "groq/") && mc.APIKey != "" {
			return NewGroqTranscriber(mc.APIKey)
		}
	}
	return nil
}

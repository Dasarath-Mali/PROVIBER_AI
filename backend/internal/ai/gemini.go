// internal/ai/gemini.go — Gemini API Client
//
// Wraps the Google Gemini REST API (v1beta) for multi-turn conversation.
// Uses the chat-style "contents" array so the agentic loop can send
// the full prompt+error history for context-aware self-fixing.

package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────
// CONSTANTS
// ─────────────────────────────────────────────────────────────

const (
	// Gemini model — use "gemini-1.5-flash" for free-tier speed.
	// Swap to "gemini-2.5-pro" for higher quality if you have quota.
	geminiModel = "gemini-2.5-pro"

	// Base URL for the Gemini generateContent REST endpoint
	geminiBaseURL = "https://generativelanguage.googleapis.com/v1beta/models"

	// Request timeout
	requestTimeout = 60 * time.Second
)

// ─────────────────────────────────────────────────────────────
// TYPES
// ─────────────────────────────────────────────────────────────

// Message represents a single turn in the conversation.
// Role must be "user" or "model".
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ── Gemini REST API request/response structs ──────────────────

type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerationConfig struct {
	Temperature     float64 `json:"temperature"`
	MaxOutputTokens int     `json:"maxOutputTokens"`
	TopP            float64 `json:"topP"`
}

type geminiResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	UsageMetadata struct {
		TotalTokenCount int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// ─────────────────────────────────────────────────────────────
// CLIENT
// ─────────────────────────────────────────────────────────────

// GeminiClient handles communication with the Gemini API.
type GeminiClient struct {
	apiKey     string
	httpClient *http.Client
}

// NewGeminiClient creates a new client with the given API key.
func NewGeminiClient(apiKey string) *GeminiClient {
	return &GeminiClient{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
	}
}

// Generate sends the full conversation history to Gemini and returns
// the model's text response along with the number of tokens consumed.
func (c *GeminiClient) Generate(history []Message) (string, int, error) {
	// Convert our internal Message slice to Gemini's content format
	contents := make([]geminiContent, 0, len(history))
	for _, m := range history {
		// Gemini uses "user" and "model" roles (not "assistant")
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: m.Content}},
		})
	}

	body := geminiRequest{
		Contents: contents,
		GenerationConfig: geminiGenerationConfig{
			Temperature:     0.2,  // low temperature = deterministic, compilable code
			MaxOutputTokens: 4096, // generous ceiling for larger programs
			TopP:            0.95,
		},
	}

	reqJSON, err := json.Marshal(body)
	if err != nil {
		return "", 0, fmt.Errorf("marshal request: %w", err)
	}

	// Build URL: POST .../gemini-1.5-flash:generateContent?key=...
	url := fmt.Sprintf("%s/%s:generateContent?key=%s", geminiBaseURL, geminiModel, c.apiKey)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqJSON))
	if err != nil {
		return "", 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("Gemini API status %d: %s", resp.StatusCode, string(respBytes))
	}

	var gemResp geminiResponse
	if err := json.Unmarshal(respBytes, &gemResp); err != nil {
		return "", 0, fmt.Errorf("unmarshal response: %w", err)
	}

	// Surface API-level errors embedded in the response body
	if gemResp.Error != nil {
		return "", 0, fmt.Errorf("Gemini error %d (%s): %s",
			gemResp.Error.Code, gemResp.Error.Status, gemResp.Error.Message)
	}

	if len(gemResp.Candidates) == 0 || len(gemResp.Candidates[0].Content.Parts) == 0 {
		return "", 0, fmt.Errorf("Gemini returned an empty response")
	}

	text := gemResp.Candidates[0].Content.Parts[0].Text
	tokens := gemResp.UsageMetadata.TotalTokenCount

	return text, tokens, nil
}

// ─────────────────────────────────────────────────────────────
// CODE EXTRACTION
// ─────────────────────────────────────────────────────────────

// codeBlockRe matches a markdown code block.
// Group 1 → optional language tag (e.g., "go")
// Group 2 → the code content
var codeBlockRe = regexp.MustCompile("(?s)```(?:go|golang|Go)?\n?(.*?)```")

// ExtractCode pulls the first Go code block out of a Gemini response.
// It also tries to infer the filename from a "// filename:" comment.
// Returns (code, filename).
func ExtractCode(response string) (string, string) {
	matches := codeBlockRe.FindStringSubmatch(response)
	if len(matches) < 2 {
		// Fallback: if the response itself looks like Go code (starts with "package")
		trimmed := strings.TrimSpace(response)
		if strings.HasPrefix(trimmed, "package ") {
			return trimmed, inferFilename(trimmed)
		}
		return "", ""
	}

	code := strings.TrimSpace(matches[1])
	filename := inferFilename(code)
	return code, filename
}

// inferFilename looks for a "// filename: foo.go" or "// file: foo.go" comment
// at the top of the code block; falls back to "main.go".
func inferFilename(code string) string {
	lines := strings.SplitN(code, "\n", 5)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"// filename:", "// file:", "// File:", "// Filename:"} {
			if strings.HasPrefix(line, prefix) {
				name := strings.TrimSpace(strings.TrimPrefix(line, prefix))
				if strings.HasSuffix(name, ".go") {
					return name
				}
			}
		}
	}
	return "main.go"
}

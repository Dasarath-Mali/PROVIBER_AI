// internal/ai/gemini.go — Gemini API Client

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

const (
	geminiModel    = "gemini-2.5-flash"
	geminiBaseURL  = "https://generativelanguage.googleapis.com/v1beta/models" // ✅ GOOD: Plain string
	requestTimeout = 60 * time.Second
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

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

type GeminiClient struct {
	apiKey     string
	httpClient *http.Client
}

func NewGeminiClient(apiKey string) *GeminiClient {
	return &GeminiClient{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
	}
}

func (c *GeminiClient) Generate(history []Message) (string, int, error) {
	contents := make([]geminiContent, 0, len(history))
	for _, m := range history {
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
			Temperature:     0.2,
			MaxOutputTokens: 4096,
			TopP:            0.95,
		},
	}

	reqJSON, err := json.Marshal(body)
	if err != nil {
		return "", 0, fmt.Errorf("marshal request: %w", err)
	}

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

// 🚀 CHANGED: Regex now captures the language tag dynamically
var codeBlockRe = regexp.MustCompile(`(?s)` + "```" + `([a-zA-Z0-9]+)?\n?(.*?)` + "```")

func ExtractCode(response string) (string, string) {
	matches := codeBlockRe.FindStringSubmatch(response)
	if len(matches) < 3 {
		return "", ""
	}

	lang := strings.ToLower(strings.TrimSpace(matches[1]))
	code := strings.TrimSpace(matches[2])

	filename := inferFilename(code, lang)
	return code, filename
}

// 🚀 CHANGED: Infers correct file extension based on AI's chosen language
func inferFilename(code, lang string) string {
	// Look for explicit file name comments
	lines := strings.SplitN(code, "\n", 5)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"// filename:", "// file:", "# filename:", "# file:"} {
			if strings.HasPrefix(strings.ToLower(line), prefix) {
				name := strings.TrimSpace(line[len(prefix):])
				if name != "" {
					return name
				}
			}
		}
	}

	// Fallback based on detected language
	switch lang {
	case "python", "py":
		return "main.py"
	case "javascript", "js", "node":
		return "main.js"
	case "go", "golang":
		return "main.go"
	case "html":
		return "index.html"
	default:
		return "main.txt"
	}
}

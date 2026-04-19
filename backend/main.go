// main.go — ProViber Backend

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"

	"proviber/internal/ai"
	"proviber/internal/executor"
)

const (
	defaultPort    = "8080"
	allowedOrigin  = "https://YOUR_PROVIBER_FRONTEND_VERCEL_APP" // Replace before deploy
	maxFixAttempts = 5
)

type ClientMessage struct {
	Type      string `json:"type"`
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
	Filename  string `json:"filename"`
}

type ServerMessage struct {
	Type     string `json:"type"`
	Payload  string `json:"payload"`
	Filename string `json:"filename,omitempty"`
}

type Session struct {
	stopCh chan struct{}
	once   sync.Once
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]*Session)}
}

func (s *SessionStore) Create(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &Session{stopCh: make(chan struct{})}
	s.sessions[id] = sess
	return sess.stopCh
}

func (s *SessionStore) Stop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.once.Do(func() { close(sess.stopCh) })
	}
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

type Server struct {
	upgrader     websocket.Upgrader
	rateLimiter  *RateLimiter
	sessions     *SessionStore
	geminiClient *ai.GeminiClient
}

func NewServer(apiKey string) *Server {
	return &Server{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				return origin == allowedOrigin ||
					strings.HasPrefix(origin, "http://localhost") ||
					strings.HasPrefix(origin, "http://127.0.0.1") ||
					origin == ""
			},
		},
		rateLimiter:  NewRateLimiter(5, time.Minute),
		sessions:     NewSessionStore(),
		geminiClient: ai.NewGeminiClient(apiKey),
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok","service":"proviber"}`)
}

func (s *Server) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == allowedOrigin ||
			strings.HasPrefix(origin, "http://localhost") ||
			strings.HasPrefix(origin, "http://127.0.0.1") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)
	if !s.rateLimiter.Allow(clientIP) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}
	defer conn.Close()

	var writeMu sync.Mutex
	send := func(msgType, payload, filename string) {
		msg := ServerMessage{Type: msgType, Payload: payload, Filename: filename}
		data, _ := json.Marshal(msg)
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.WriteMessage(websocket.TextMessage, data)
	}

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg ClientMessage
		if jsonErr := json.Unmarshal(raw, &msg); jsonErr != nil {
			continue
		}

		switch msg.Type {
		case "vibe":
			stopCh := s.sessions.Create(msg.SessionID)
			go s.runAgentLoop(msg, send, stopCh)
		case "stop":
			s.sessions.Stop(msg.SessionID)
			send("log", "Stop signal received.", "")
		case "get_file":
			content, ferr := executor.ReadWorkspaceFile(msg.SessionID, msg.Filename)
			if ferr != nil {
				send("error", fmt.Sprintf("Could not read file: %v", ferr), "")
			} else {
				send("code", content, msg.Filename)
			}
		}
	}
}

func (s *Server) runAgentLoop(msg ClientMessage, send func(string, string, string), stopCh chan struct{}) {
	defer s.sessions.Delete(msg.SessionID)

	sessionID := msg.SessionID
	prompt := msg.Prompt

	send("status", "Generating initial code...", "")
	send("log", fmt.Sprintf("Starting polyglot agentic loop for session %s", sessionID), "")

	conversationHistory := []ai.Message{}
	var lastCode string
	var lastFilename string

	for attempt := 1; attempt <= maxFixAttempts; attempt++ {
		select {
		case <-stopCh:
			send("status", "Stopped by user.", "")
			send("log", "Agent loop stopped.", "")
			return
		default:
		}

		send("attempt", fmt.Sprintf("%d", attempt), "")

		var geminiPrompt string
		if attempt == 1 {
			geminiPrompt = buildInitialPrompt(prompt)
		} else {
			geminiPrompt = buildFixPrompt(lastCode, lastFilename)
		}

		conversationHistory = append(conversationHistory, ai.Message{
			Role: "user", Content: geminiPrompt,
		})

		send("status", "Calling Gemini AI...", "")
		resp, tokensUsed, genErr := s.geminiClient.Generate(conversationHistory)
		if genErr != nil {
			send("error", fmt.Sprintf("Gemini API error: %v", genErr), "")
			send("fatal", "Cannot continue — Gemini unavailable.", "")
			return
		}

		send("token_count", fmt.Sprintf("%d", tokensUsed), "")

		conversationHistory = append(conversationHistory, ai.Message{
			Role: "model", Content: resp,
		})

		code, filename := ai.ExtractCode(resp)
		if code == "" {
			send("error", "Gemini returned no code block. Retrying...", "")
			conversationHistory = append(conversationHistory, ai.Message{
				Role:    "user",
				Content: "Your response contained no code block. Please respond with ONLY a code block wrapped in ```language ... ```.",
			})
			continue
		}

		lastCode = code
		lastFilename = filename

		send("code", code, lastFilename)
		send("status", "Writing code to workspace...", "")

		writeErr := executor.WriteToWorkspace(sessionID, lastFilename, code)
		if writeErr != nil {
			send("error", fmt.Sprintf("Could not write workspace: %v", writeErr), "")
			send("fatal", "Workspace write failed.", "")
			return
		}

		files, _ := executor.ListWorkspaceFiles(sessionID)
		filesJSON, _ := json.Marshal(files)
		send("files", string(filesJSON), "")

		send("status", fmt.Sprintf("Running %s...", lastFilename), "")

		// 🚀 CHANGED: Using generic RunCode instead of RunGoCode
		stdout, stderr, runErr := executor.RunCode(sessionID, lastFilename)

		if runErr == nil && stderr == "" {
			output := stdout
			if output == "" {
				output = "(no output — program exited cleanly)"
			}
			send("status", "Success!", "")
			send("success", output, "")
			send("log", fmt.Sprintf("Completed in %d attempt(s).", attempt), "")
			return
		}

		errMsg := stderr
		if errMsg == "" && runErr != nil {
			errMsg = runErr.Error()
		}
		send("error", errMsg, "")
		send("log", fmt.Sprintf("Attempt %d/%d failed — asking Gemini to fix...", attempt, maxFixAttempts), "")

		conversationHistory = append(conversationHistory, ai.Message{
			Role: "user",
			Content: fmt.Sprintf(
				"The code produced this error:\n\n```\n%s\n```\n\nPlease fix it and return ONLY the corrected code block.",
				errMsg,
			),
		})

		select {
		case <-stopCh:
			return
		case <-time.After(1 * time.Second):
		}
	}

	send("fatal", fmt.Sprintf("Could not produce working code after %d attempts.", maxFixAttempts), "")
}

// 🚀 CHANGED: Multi-language system prompt
func buildInitialPrompt(userVibe string) string {
	return fmt.Sprintf(
		"You are an expert polyglot programmer.\n"+
			"The user wants you to write code that does the following:\n\n"+
			"%s\n\n"+
			"Requirements:\n"+
			"- Write complete, working code.\n"+
			"- Choose the best language for the task (Go, Python, or JavaScript/Node.js), unless the user specifies one.\n"+
			"- Return ONLY the code wrapped in a single markdown code block with the correct language tag (e.g., ```python, ```go, ```javascript).\n"+
			"- Do not include any explanation text outside the code block.",
		userVibe,
	)
}

func buildFixPrompt(lastCode, filename string) string {
	return fmt.Sprintf(
		"The previous code in %s failed to run.\n"+
			"Please review the error (sent in this conversation) and return a corrected version.\n"+
			"Return ONLY the fixed code in a code block. No explanations.",
		filename,
	)
}

func getClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		parts := strings.SplitN(ip, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

func main() {
	// 🚀 CHANGED: Load .env file automatically
	if err := godotenv.Load(); err != nil {
		log.Println("[startup] No .env file found, relying on system environment variables.")
	}

	geminiKey := os.Getenv("GEMINI_API_KEY")
	if geminiKey == "" {
		log.Fatal("[startup] GEMINI_API_KEY environment variable is not set")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	srv := NewServer(geminiKey)

	http.HandleFunc("/health", srv.corsMiddleware(healthHandler))
	http.HandleFunc("/ws", srv.corsMiddleware(srv.wsHandler))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "ProViber backend v1.0 — WebSocket at /ws")
	})

	log.Printf("[startup] ProViber backend starting on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("[startup] server failed: %v", err)
	}
}

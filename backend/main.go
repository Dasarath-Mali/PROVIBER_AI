// main.go — ProViber Backend
//
// A Go WebSocket server that:
//  1. Accepts a "vibe" (natural language prompt) from the browser.
//  2. Calls the Gemini AI API to generate Go source code.
//  3. Writes the code to a temporary workspace directory.
//  4. Compiles and runs it, capturing stdout/stderr.
//  5. If there are errors, sends stderr back to Gemini for a
//     self-fix loop — repeating until clean or max attempts hit.
//
// Deployment:
//   Backend  → Render (persistent web service)
//   Frontend → Vercel (static site)
//
// Replace the placeholder constants below before deploying.

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

	"proviber/internal/ai"
	"proviber/internal/executor"
)

// ─────────────────────────────────────────────────────────────
// CONFIGURATION — Replace these before deploying
// ─────────────────────────────────────────────────────────────

const (
	// Port the server listens on (Render sets $PORT automatically)
	defaultPort = "8080"

	// Allowed origin for CORS.
	// Replace with your Vercel frontend URL:
	// e.g. "https://proviber.vercel.app"
	allowedOrigin = "https://YOUR_PROVIBER_FRONTEND_VERCEL_APP" // ← paste Vercel URL

	// Maximum self-fix iterations the agentic loop will attempt
	// before giving up and reporting failure.
	maxFixAttempts = 5
)

// ─────────────────────────────────────────────────────────────
// MESSAGE TYPES (client ↔ server JSON protocol)
// ─────────────────────────────────────────────────────────────

// ClientMessage is a message sent FROM the browser TO the server.
type ClientMessage struct {
	Type      string `json:"type"`       // "vibe" | "stop" | "get_file"
	Prompt    string `json:"prompt"`     // user's natural-language request
	SessionID string `json:"session_id"` // unique session identifier
	Filename  string `json:"filename"`   // for "get_file" requests
}

// ServerMessage is a message sent FROM the server TO the browser.
type ServerMessage struct {
	Type     string `json:"type"` // see handleServerMessage() in script.js
	Payload  string `json:"payload"`
	Filename string `json:"filename,omitempty"`
}

// ─────────────────────────────────────────────────────────────
// SESSION TRACKING (for stop signals)
// ─────────────────────────────────────────────────────────────

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

// Create registers a new session and returns its stop channel.
func (s *SessionStore) Create(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &Session{stopCh: make(chan struct{})}
	s.sessions[id] = sess
	return sess.stopCh
}

// Stop signals a running session to halt.
func (s *SessionStore) Stop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.once.Do(func() { close(sess.stopCh) })
	}
}

// Delete removes a session from the store.
func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// ─────────────────────────────────────────────────────────────
// SERVER
// ─────────────────────────────────────────────────────────────

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
			// Allow only our Vercel frontend; also allow localhost in dev.
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				return origin == allowedOrigin ||
					strings.HasPrefix(origin, "http://localhost") ||
					strings.HasPrefix(origin, "http://127.0.0.1") ||
					origin == "" // non-browser clients (curl, tests)
			},
		},
		rateLimiter:  NewRateLimiter(5, time.Minute), // 5 req / min / IP
		sessions:     NewSessionStore(),
		geminiClient: ai.NewGeminiClient(apiKey),
	}
}

// ─────────────────────────────────────────────────────────────
// HTTP HANDLERS
// ─────────────────────────────────────────────────────────────

// healthHandler responds to Render's health-check pings.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok","service":"proviber"}`)
}

// corsMiddleware injects permissive-but-targeted CORS headers.
func (s *Server) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Reflect the allowed origin (never wildcard "*" when using credentials)
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

// wsHandler upgrades the HTTP connection to WebSocket, then handles messages.
func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	// Rate-limit per client IP
	clientIP := getClientIP(r)
	if !s.rateLimiter.Allow(clientIP) {
		log.Printf("[rate-limit] blocked %s", clientIP)
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("[ws] client connected: %s", clientIP)

	// Protect concurrent writes with a mutex
	var writeMu sync.Mutex
	send := func(msgType, payload, filename string) {
		msg := ServerMessage{Type: msgType, Payload: payload, Filename: filename}
		data, _ := json.Marshal(msg)
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.WriteMessage(websocket.TextMessage, data)
	}

	// Message loop
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[ws] unexpected close from %s: %v", clientIP, err)
			}
			break
		}

		var msg ClientMessage
		if jsonErr := json.Unmarshal(raw, &msg); jsonErr != nil {
			log.Printf("[ws] bad JSON from %s: %v", clientIP, jsonErr)
			continue
		}

		switch msg.Type {

		case "vibe":
			// Each vibe runs in its own goroutine so the read loop stays responsive
			stopCh := s.sessions.Create(msg.SessionID)
			go s.runAgentLoop(msg, send, stopCh)

		case "stop":
			s.sessions.Stop(msg.SessionID)
			send("log", "Stop signal received.", "")

		case "get_file":
			// Return the content of a generated workspace file
			content, ferr := executor.ReadWorkspaceFile(msg.SessionID, msg.Filename)
			if ferr != nil {
				send("error", fmt.Sprintf("Could not read file: %v", ferr), "")
			} else {
				send("code", content, msg.Filename)
			}

		default:
			log.Printf("[ws] unknown message type: %s", msg.Type)
		}
	}

	log.Printf("[ws] client disconnected: %s", clientIP)
}

// ─────────────────────────────────────────────────────────────
// AGENTIC SELF-FIX LOOP
// ─────────────────────────────────────────────────────────────

// runAgentLoop is the heart of ProViber:
//
//  1. Send user prompt → Gemini → get Go source code
//  2. Write code to workspace/
//  3. go build + run, capture stderr
//  4. If errors: send stderr back to Gemini as a "fix" prompt
//  5. Repeat until clean output OR maxFixAttempts exceeded
func (s *Server) runAgentLoop(
	msg ClientMessage,
	send func(string, string, string),
	stopCh chan struct{},
) {
	defer s.sessions.Delete(msg.SessionID)

	sessionID := msg.SessionID
	prompt := msg.Prompt

	send("status", "Generating initial code...", "")
	send("log", fmt.Sprintf("Starting agentic loop for session %s", sessionID), "")

	// conversationHistory accumulates the prompt+response pairs
	// so Gemini has full context during the fix loop.
	conversationHistory := []ai.Message{}

	var lastCode string
	var lastFilename string

	for attempt := 1; attempt <= maxFixAttempts; attempt++ {
		// Check for stop signal
		select {
		case <-stopCh:
			send("status", "Stopped by user.", "")
			send("log", "Agent loop stopped.", "")
			return
		default:
		}

		send("attempt", fmt.Sprintf("%d", attempt), "")

		// ── Step 1: Build the prompt for Gemini ──────────────────
		var geminiPrompt string
		if attempt == 1 {
			geminiPrompt = buildInitialPrompt(prompt)
		} else {
			geminiPrompt = buildFixPrompt(lastCode, lastFilename)
		}

		conversationHistory = append(conversationHistory, ai.Message{
			Role: "user", Content: geminiPrompt,
		})

		// ── Step 2: Call Gemini AI ────────────────────────────────
		send("status", "Calling Gemini AI...", "")
		resp, tokensUsed, genErr := s.geminiClient.Generate(conversationHistory)
		if genErr != nil {
			send("error", fmt.Sprintf("Gemini API error: %v", genErr), "")
			send("fatal", "Cannot continue — Gemini unavailable.", "")
			return
		}

		send("token_count", fmt.Sprintf("%d", tokensUsed), "")

		// Add Gemini's response to history
		conversationHistory = append(conversationHistory, ai.Message{
			Role: "model", Content: resp,
		})

		// ── Step 3: Extract code from the response ────────────────
		code, filename := ai.ExtractCode(resp)
		if code == "" {
			send("error", "Gemini returned no code block. Retrying...", "")
			// Inject an error message so next iteration asks more clearly
			conversationHistory = append(conversationHistory, ai.Message{
				Role:    "user",
				Content: "Your response contained no Go code block. Please respond with ONLY a Go code block wrapped in ```go ... ```.",
			})
			continue
		}

		lastCode = code
		lastFilename = filename
		if lastFilename == "" {
			lastFilename = "main.go"
		}

		send("code", code, lastFilename)
		send("status", "Writing code to workspace...", "")

		// ── Step 4: Write to workspace and compile ────────────────
		writeErr := executor.WriteToWorkspace(sessionID, lastFilename, code)
		if writeErr != nil {
			send("error", fmt.Sprintf("Could not write workspace: %v", writeErr), "")
			send("fatal", "Workspace write failed.", "")
			return
		}

		// Send file list to frontend
		files, _ := executor.ListWorkspaceFiles(sessionID)
		filesJSON, _ := json.Marshal(files)
		send("files", string(filesJSON), "")

		send("status", "Compiling & running...", "")
		stdout, stderr, runErr := executor.RunGoCode(sessionID, lastFilename)

		if runErr == nil && stderr == "" {
			// ── ✅ SUCCESS ─────────────────────────────────────────
			output := stdout
			if output == "" {
				output = "(no output — program exited cleanly)"
			}
			send("status", "Success!", "")
			send("success", output, "")
			send("log", fmt.Sprintf("Completed in %d attempt(s).", attempt), "")
			return
		}

		// ── Step 5: Report the error, continue loop ──────────────
		errMsg := stderr
		if errMsg == "" && runErr != nil {
			errMsg = runErr.Error()
		}
		send("error", errMsg, "")
		send("log", fmt.Sprintf("Attempt %d/%d failed — asking Gemini to fix...", attempt, maxFixAttempts), "")

		// Append the error to history so Gemini has context
		conversationHistory = append(conversationHistory, ai.Message{
			Role: "user",
			Content: fmt.Sprintf(
				"The code produced this error:\n\n```\n%s\n```\n\nPlease fix it and return ONLY the corrected Go code block.",
				errMsg,
			),
		})

		// Brief pause to avoid hammering the API
		select {
		case <-stopCh:
			return
		case <-time.After(1 * time.Second):
		}
	}

	// Exhausted all attempts
	send("fatal", fmt.Sprintf(
		"Could not produce working code after %d attempts. Last error is shown above.",
		maxFixAttempts,
	), "")
}

// buildInitialPrompt wraps the user's vibe in a structured system instruction.
// Note: Go raw string literals (backtick strings) cannot contain backticks,
// so we use string concatenation to embed the markdown fence characters.
func buildInitialPrompt(userVibe string) string {
	fence := "```"
	return fmt.Sprintf(
		"You are an expert Go programmer.\n"+
			"The user wants you to write Go code that does the following:\n\n"+
			"%s\n\n"+
			"Requirements:\n"+
			"- Write complete, compilable Go code.\n"+
			"- The code must be a standalone package main with a main() function.\n"+
			"- Import only standard library packages unless otherwise specified.\n"+
			"- Return ONLY the Go code wrapped in a single code block like this:\n\n"+
			"%sgo\n"+
			"package main\n\n"+
			"import \"fmt\"\n\n"+
			"func main() {\n"+
			"    fmt.Println(\"Hello, ProViber!\")\n"+
			"}\n"+
			"%s\n\n"+
			"Do not include any explanation text outside the code block.",
		userVibe, fence, fence,
	)
}

// buildFixPrompt builds the retry prompt when we already have errored code.
func buildFixPrompt(lastCode, filename string) string {
	return fmt.Sprintf(
		"The previous code in %s failed to compile or run.\n"+
			"Please review the error (sent in this conversation) and return a corrected version.\n"+
			"Return ONLY the fixed Go code in a code block. No explanations.",
		filename,
	)
}

// ─────────────────────────────────────────────────────────────
// UTILITIES
// ─────────────────────────────────────────────────────────────

// getClientIP extracts the real client IP, respecting Render's proxy headers.
func getClientIP(r *http.Request) string {
	// Render (and most reverse proxies) set X-Forwarded-For
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		// X-Forwarded-For can be a comma-separated list; take the first
		parts := strings.SplitN(ip, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	// Fallback to RemoteAddr (strip port)
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

// ─────────────────────────────────────────────────────────────
// MAIN
// ─────────────────────────────────────────────────────────────

func main() {
	// Read Gemini API key from environment
	geminiKey := os.Getenv("GEMINI_API_KEY")
	if geminiKey == "" {
		log.Fatal("[startup] GEMINI_API_KEY environment variable is not set")
	}

	// Determine port (Render injects $PORT)
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	srv := NewServer(geminiKey)

	// Routes
	http.HandleFunc("/health", srv.corsMiddleware(healthHandler))
	http.HandleFunc("/ws", srv.corsMiddleware(srv.wsHandler))

	// Root route (useful for Render health checks)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "ProViber backend v1.0 — WebSocket at /ws")
	})

	log.Printf("[startup] ProViber backend starting on port %s", port)
	log.Printf("[startup] Allowed CORS origin: %s", allowedOrigin)
	log.Printf("[startup] Rate limit: 5 requests/minute/IP")
	log.Printf("[startup] Max fix attempts: %d", maxFixAttempts)

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("[startup] server failed: %v", err)
	}
}

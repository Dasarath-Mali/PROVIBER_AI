# ProViber 📟

> **Agentic self-debugging vibe coder.** Describe what you want in plain English — ProViber calls Gemini AI, writes Go code, compiles it, and auto-fixes any errors in a loop until your code runs clean.

```
  ██████╗ ██████╗  ██████╗ ██╗   ██╗██╗██████╗ ███████╗██████╗
  ██╔══██╗██╔══██╗██╔═══██╗██║   ██║██║██╔══██╗██╔════╝██╔══██╗
  ██████╔╝██████╔╝██║   ██║██║   ██║██║██████╔╝█████╗  ██████╔╝
  ██╔═══╝ ██╔══██╗██║   ██║╚██╗ ██╔╝██║██╔══██╗██╔══╝  ██╔══██╗
  ██║     ██║  ██║╚██████╔╝ ╚████╔╝ ██║██████╔╝███████╗██║  ██║
  ╚═╝     ╚═╝  ╚═╝ ╚═════╝   ╚═══╝  ╚═╝╚═════╝ ╚══════╝╚═╝  ╚═╝
```

---

## Architecture

```
Browser (Vercel)          Backend (Render)          External
──────────────────        ──────────────────        ──────────
index.html + CSS + JS ←→  main.go (WebSocket)  ←→  Gemini AI
                           ├── ratelimiter.go        (REST API)
                           ├── internal/ai/
                           │   └── gemini.go
                           └── internal/executor/
                               └── runner.go
```

### Agentic Loop Flow

```
User Prompt
    │
    ▼
[Gemini API] → Generate Go code
    │
    ▼
[Executor] → go run main.go
    │
    ├── ✅ Success → send output to browser
    │
    └── ❌ Error (stderr)
            │
            ▼
        [Gemini API] → Fix code with error context
            │
            └── repeat (max 5 attempts)
```

---

## Local Development

### Prerequisites

- **Go 1.21+** — [install](https://go.dev/dl/)
- **Gemini API Key** — [get one free](https://aistudio.google.com/app/apikey)
- A static file server (Python, Caddy, VS Code Live Server, etc.)

### 1. Clone & configure

```bash
git clone https://github.com/YOUR_USERNAME/proviber.git
cd proviber
cp .env.example .env
# Edit .env and set GEMINI_API_KEY=your_key_here
```

### 2. Start the backend

```bash
cd backend
go mod tidy          # download dependencies
export GEMINI_API_KEY=your_key_here
go run .             # starts on :8080
```

You should see:
```
[startup] ProViber backend starting on port 8080
[startup] Allowed CORS origin: https://YOUR_PROVIBER_FRONTEND_VERCEL_APP
```

### 3. Start the frontend

```bash
# From the repo root
cd frontend
python3 -m http.server 3000
# Visit http://localhost:3000
```

The `script.js` auto-detects `localhost` and connects to `ws://localhost:8080/ws`.

---

## Deployment

### Backend → Render

1. Push to GitHub.
2. Go to [Render Dashboard](https://dashboard.render.com) → **New** → **Blueprint**.
3. Connect your repo — Render detects `backend/render.yaml` automatically.
4. In the Render dashboard, add the environment variable:
   ```
   GEMINI_API_KEY = your_actual_key
   ```
5. After deploy, copy your service URL: `https://proviber-backend.onrender.com`

6. **Update `backend/main.go`:**
   ```go
   const allowedOrigin = "https://YOUR_VERCEL_URL.vercel.app"
   ```

### Frontend → Vercel

1. Go to [Vercel](https://vercel.com) → **New Project** → import your repo.
2. Set the **Root Directory** to `frontend`.
3. No build command needed (it's a static site).
4. After deploy, copy your Vercel URL.

5. **Update `frontend/script.js`:**
   ```js
   const RENDER_BACKEND_HOST = "proviber-backend.onrender.com";
   ```

6. Redeploy the frontend.

---

## Project Structure

```
ProViber/
├── frontend/                 ← Deploy to Vercel
│   ├── index.html            # 2000s CRT Monitor UI
│   ├── style.css             # Retro beige + phosphor green styling
│   └── script.js             # WebSocket client + UI logic
│
├── backend/                  ← Deploy to Render
│   ├── main.go               # Go WebSocket server + agentic loop
│   ├── ratelimiter.go        # Token-bucket rate limiter (5 req/min/IP)
│   ├── go.mod
│   ├── go.sum
│   ├── render.yaml           # Render Blueprint
│   └── internal/
│       ├── ai/
│       │   └── gemini.go     # Gemini REST API client + code extractor
│       └── executor/
│           └── runner.go     # Workspace management + go run subprocess
│
├── .env.example              # Environment variable template
└── README.md
```

---

## WebSocket Protocol

All messages are JSON.

### Browser → Server

| `type`     | Fields                          | Description                   |
|------------|---------------------------------|-------------------------------|
| `vibe`     | `prompt`, `session_id`          | Start a new agentic session   |
| `stop`     | `session_id`                    | Stop the current loop         |
| `get_file` | `session_id`, `filename`        | Fetch a workspace file        |

### Server → Browser

| `type`        | `payload`                        | Description                   |
|---------------|----------------------------------|-------------------------------|
| `log`         | Info string                      | General log message           |
| `code`        | Go source code                   | Generated/fixed code          |
| `status`      | Phase string                     | Agent phase update            |
| `error`       | stderr string                    | Compilation/runtime error     |
| `attempt`     | Attempt number                   | Self-fix iteration counter    |
| `success`     | stdout string                    | Final clean output            |
| `files`       | JSON array of filenames          | Workspace file list           |
| `token_count` | Number                           | Tokens used in this call      |
| `rate_limit`  | Message                          | Rate limit warning            |
| `fatal`       | Message                          | Unrecoverable failure         |

---

## Rate Limiting

ProViber uses a **token-bucket** rate limiter (`ratelimiter.go`):

- **5 requests per minute per IP** on the free plan
- Buckets refill smoothly (not in discrete windows)
- Stale buckets are cleaned up every 5 minutes
- Returns HTTP 429 for over-limit HTTP requests; sends a `rate_limit` WebSocket message for in-session overages

---

## Security Notes

- ⚠️ **Arbitrary code execution** — the executor runs AI-generated Go code in a subprocess. For production use, wrap it in a container sandbox (gVisor, Firecracker, or a Docker container with `--no-new-privileges`).
- The subprocess environment is stripped to only `PATH`, `GOPATH`, and `HOME=/tmp`.
- Execution is hard-killed after **15 seconds**.
- Output is capped at **16 KB** to prevent memory exhaustion.

---

## Keyboard Shortcuts

| Shortcut      | Action              |
|---------------|---------------------|
| `Ctrl+Enter`  | Send vibe           |
| `Escape`      | Stop the loop       |
| `Ctrl+K`      | Clear terminal      |

---

## License

MIT — do whatever you want with this, just vibe responsibly.
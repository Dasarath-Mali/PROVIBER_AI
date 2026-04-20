/**
 * ProViber — Frontend Script
 * Handles WebSocket connection to Go backend, UI interactions,
 * agent loop visualization, and retro CRT effects.
 */

/* ══════════════════════════════════════════════════════════════
   CONFIGURATION
══════════════════════════════════════════════════════════════ */

/**
 * Dynamic WebSocket URL:
 * - In development  → ws://localhost:8080/ws
 * - In production   → wss://YOUR_RENDER_URL_HERE/ws
 *
 * Replace YOUR_RENDER_URL_HERE with your actual Render service URL
 * e.g. "proviber-backend.onrender.com"
 */
const RENDER_BACKEND_HOST = "https://proviber-backend.onrender.com"; // ← paste your Render URL here

const WS_URL = (function () {
  const isDev = location.hostname === "localhost" || location.hostname === "127.0.0.1";
  if (isDev) {
    return "ws://localhost:8080/ws";
  }
  return `wss://${RENDER_BACKEND_HOST}/ws`;
})();

const MAX_RETRY_DELAY_MS = 30_000;
const INITIAL_RETRY_DELAY_MS = 1_000;

/* ══════════════════════════════════════════════════════════════
   STATE
══════════════════════════════════════════════════════════════ */

let ws = null;
let wsRetryDelay = INITIAL_RETRY_DELAY_MS;
let wsRetryTimer = null;
let isConnected = false;
let isStopped = false;
let sessionId = null;
let generatedCode = "";
let attemptNumber = 0;
let errorCount = 0;
let tokenTotal = 0;
let workspaceFiles = [];

/* ══════════════════════════════════════════════════════════════
   WEBSOCKET
══════════════════════════════════════════════════════════════ */

function connectWS() {
  if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) return;

  logTerminal("system", `> Connecting to ${WS_URL} ...`);
  setConnectionState("connecting");

  ws = new WebSocket(WS_URL);

  ws.onopen = function () {
    wsRetryDelay = INITIAL_RETRY_DELAY_MS;
    isConnected = true;
    setConnectionState("connected");
    logTerminal("success", "> ✅ WebSocket connected — backend is alive!");
    if (wsRetryTimer) clearTimeout(wsRetryTimer);
  };

  ws.onmessage = function (evt) {
    try {
      const msg = JSON.parse(evt.data);
      handleServerMessage(msg);
    } catch (e) {
      // Plain text fallback
      logTerminal("info", evt.data);
    }
  };

  ws.onerror = function () {
    logTerminal("error", "> ❌ WebSocket error — check backend connection.");
  };

  ws.onclose = function (evt) {
    isConnected = false;
    setConnectionState("disconnected");
    logTerminal("system", `> 🔌 WebSocket closed (code ${evt.code}). Reconnecting in ${wsRetryDelay / 1000}s...`);

    // Exponential backoff reconnect
    wsRetryTimer = setTimeout(() => {
      wsRetryDelay = Math.min(wsRetryDelay * 2, MAX_RETRY_DELAY_MS);
      connectWS();
    }, wsRetryDelay);
  };
}

/**
 * Handle structured JSON messages from the Go backend.
 * Message schema: { type, payload }
 *
 * Types:
 *   "log"         – plain terminal output
 *   "code"        – generated Go code
 *   "status"      – agent phase update
 *   "error"       – compilation / runtime error
 *   "success"     – final success signal
 *   "attempt"     – iteration counter
 *   "files"       – workspace file list
 *   "token_count" – total tokens used
 *   "rate_limit"  – rate limit warning
 */
function handleServerMessage(msg) {
  switch (msg.type) {

    case "log":
      logTerminal("info", `> ${msg.payload}`);
      break;

    case "code":
      generatedCode = msg.payload;
      updateCodePanel(msg.payload, msg.filename || "main.go");
      logTerminal("ai", `> 🤖 Gemini generated code (${msg.payload.length} chars)`);
      setLoopNodeState("gemini", "done");
      setLoopNodeState("exec", "active");
      break;

    case "status":
      updatePhase(msg.payload);
      logTerminal("system", `> [AGENT] Phase: ${msg.payload}`);
      break;

    case "error":
      errorCount++;
      document.getElementById("errorCount").textContent = errorCount;
      logTerminal("error", `> ⚠️  STDERR: ${msg.payload}`);
      setLoopNodeState("exec", "error");
      setLoopNodeState("fix", "active");
      break;

    case "attempt":
      attemptNumber = parseInt(msg.payload, 10);
      document.getElementById("attemptCount").textContent = `#${attemptNumber}`;
      logTerminal("ai", `> 🔄 Self-fix attempt #${attemptNumber}...`);
      resetLoopNodes();
      setLoopNodeState("prompt", "done");
      setLoopNodeState("gemini", "active");
      break;

    case "success":
      logTerminal("success", "> ✅ Code compiled and ran successfully!");
      logTerminal("success", `> Output:\n${msg.payload}`);
      setLoopNodeState("exec", "done");
      setLoopNodeState("fix", "done");
      updatePhase("✅ Done");
      document.getElementById("sendBtn").disabled = false;
      document.getElementById("sendBtn").textContent = "▶ VIBE IT";
      setSBSession(`Done in ${attemptNumber} attempt(s)`);
      break;

    case "files":
      try {
        workspaceFiles = JSON.parse(msg.payload);
        renderFileTree(workspaceFiles);
      } catch {
        workspaceFiles = [msg.payload];
        renderFileTree(workspaceFiles);
      }
      break;

    case "token_count":
      tokenTotal += parseInt(msg.payload, 10);
      document.getElementById("tokenCount").textContent = tokenTotal.toLocaleString();
      break;

    case "rate_limit":
      logTerminal("error", `> 🚫 RATE LIMIT: ${msg.payload}`);
      document.getElementById("sb-rate").textContent = "⚠️ Rate limited";
      document.getElementById("sb-rate").style.color = "red";
      setTimeout(() => {
        document.getElementById("sb-rate").textContent = "Rate: OK";
        document.getElementById("sb-rate").style.color = "";
      }, 60_000);
      break;

    case "fatal":
      logTerminal("error", `> 💀 FATAL: ${msg.payload}`);
      updatePhase("❌ Failed");
      document.getElementById("sendBtn").disabled = false;
      document.getElementById("sendBtn").textContent = "▶ VIBE IT";
      break;

    default:
      logTerminal("system", `> [${msg.type}] ${msg.payload}`);
  }
}

/* ══════════════════════════════════════════════════════════════
   SEND VIBE
══════════════════════════════════════════════════════════════ */

function sendVibe() {
  const prompt = document.getElementById("vibeInput").value.trim();

  if (!prompt) {
    logTerminal("error", "> ❌ Please enter a vibe prompt first!");
    document.getElementById("vibeInput").focus();
    return;
  }

  if (!isConnected) {
    logTerminal("error", "> ❌ Not connected to backend. Trying to reconnect...");
    connectWS();
    return;
  }

  // Reset state
  isStopped = false;
  attemptNumber = 0;
  errorCount = 0;
  generatedCode = "";
  document.getElementById("attemptCount").textContent = "—";
  document.getElementById("errorCount").textContent = "0";
  document.getElementById("tokenCount").textContent = "0";
  resetLoopNodes();

  // Generate a session ID
  sessionId = "sess_" + Date.now().toString(36);
  document.getElementById("sb-session").textContent = `Session: ${sessionId}`;
  setSBSession(sessionId);

  // Update UI
  document.getElementById("sendBtn").disabled = true;
  document.getElementById("sendBtn").textContent = "⏳ Vibing...";
  document.getElementById("codeBlock").textContent = "// Generating code...";
  document.getElementById("codeFilename").textContent = "";
  clearWorkspaceFiles();
  updatePhase("Sending prompt...");
  setLoopNodeState("prompt", "active");

  logTerminal("info", `> 🚀 Sending vibe: "${prompt.substring(0, 60)}${prompt.length > 60 ? "..." : ""}"`);

  // Send to backend
  const payload = JSON.stringify({
    type: "vibe",
    prompt: prompt,
    session_id: sessionId,
  });

  ws.send(payload);

  // Add to terminal
  logTerminal("system", `> Session: ${sessionId}`);

  setTimeout(() => {
    setLoopNodeState("prompt", "done");
    setLoopNodeState("gemini", "active");
  }, 500);
}

function stopLoop() {
  if (ws && isConnected) {
    ws.send(JSON.stringify({ type: "stop", session_id: sessionId }));
    logTerminal("error", "> ⏹ Stop signal sent.");
  }
  isStopped = true;
  document.getElementById("sendBtn").disabled = false;
  document.getElementById("sendBtn").textContent = "▶ VIBE IT";
  updatePhase("Stopped");
}

/* ══════════════════════════════════════════════════════════════
   UI HELPERS
══════════════════════════════════════════════════════════════ */

function logTerminal(type, text) {
  const terminal = document.getElementById("terminal");
  const line = document.createElement("div");
  line.className = `terminal-line ${type}-line`;

  // Replace cursor blink line if present
  const blinkLine = terminal.querySelector(".blink-line");
  if (blinkLine) blinkLine.remove();

  line.textContent = text;
  terminal.appendChild(line);

  // Add new cursor line
  const cursor = document.createElement("div");
  cursor.className = "terminal-line blink-line";
  cursor.innerHTML = '> <span class="cursor">█</span>';
  terminal.appendChild(cursor);

  // Auto-scroll
  terminal.scrollTop = terminal.scrollHeight;

  // Typing pulse effect
  terminal.classList.add("typing");
  clearTimeout(terminal._pulseTimer);
  terminal._pulseTimer = setTimeout(() => terminal.classList.remove("typing"), 300);
}

function setConnectionState(state) {
  const light = document.getElementById("statusLight");
  const text  = document.getElementById("statusText");
  const sb    = document.getElementById("sb-connection");

  switch (state) {
    case "connected":
      light.textContent = "🟢";
      text.textContent  = "Connected";
      sb.textContent    = "🟢 Online";
      sb.style.color    = "green";
      break;
    case "connecting":
      light.textContent = "🟡";
      text.textContent  = "Connecting...";
      sb.textContent    = "🟡 Connecting";
      sb.style.color    = "darkorange";
      break;
    case "disconnected":
      light.textContent = "🔴";
      text.textContent  = "Disconnected";
      sb.textContent    = "🔴 Offline";
      sb.style.color    = "red";
      break;
  }
}

function updatePhase(phase) {
  document.getElementById("currentPhase").textContent = phase;
}

function setSBSession(text) {
  document.getElementById("sb-session").textContent = `Session: ${text}`;
}

function updateCodePanel(code, filename) {
  document.getElementById("codeBlock").textContent = code;
  document.getElementById("codeFilename").textContent = filename ? `· ${filename}` : "";

  // Simple Go syntax coloring via class names (no external lib needed)
  // Real projects should use highlight.js or Prism
  const ext = (filename || "").split(".").pop().toLowerCase();
  document.getElementById("codeLangBadge").textContent = ext.toUpperCase() || "GO";
}

function renderFileTree(files) {
  const tree = document.getElementById("fileTree");
  tree.innerHTML = '<div class="tree-item tree-root">📂 workspace/</div>';
  if (!files || files.length === 0) {
    tree.innerHTML += '<div class="tree-empty">No files yet</div>';
    return;
  }
  files.forEach(f => {
    const item = document.createElement("div");
    item.className = "tree-item tree-file";
    item.textContent = `  📄 ${f}`;
    item.title = `Click to view ${f}`;
    item.onclick = () => requestFile(f);
    tree.appendChild(item);
  });
}

function clearWorkspaceFiles() {
  renderFileTree([]);
}

function requestFile(filename) {
  if (ws && isConnected) {
    ws.send(JSON.stringify({ type: "get_file", filename }));
  }
}

/* ─── Loop Visualizer ─── */
function setLoopNodeState(nodeId, state) {
  const el = document.getElementById(`node-${nodeId}`);
  if (!el) return;
  el.className = `loop-node ${state}`;
}

function resetLoopNodes() {
  ["prompt", "gemini", "exec", "fix"].forEach(id => setLoopNodeState(id, ""));
}

/* ══════════════════════════════════════════════════════════════
   WINDOW MANAGEMENT (Drag & Drop, Open/Close)
══════════════════════════════════════════════════════════════ */

let dragState = null;

function startDrag(e, windowId) {
  if (e.target.closest(".title-bar-controls")) return; // don't drag on buttons

  const win = document.getElementById(windowId);
  const rect = win.getBoundingClientRect();
  dragState = {
    id: windowId,
    startX: e.clientX,
    startY: e.clientY,
    origLeft: parseInt(win.style.left) || 0,
    origTop:  parseInt(win.style.top)  || 0,
  };
  win.classList.add("dragging");
  win.style.zIndex = 900;

  document.onmousemove = onDrag;
  document.onmouseup   = stopDrag;
  e.preventDefault();
}

function onDrag(e) {
  if (!dragState) return;
  const win = document.getElementById(dragState.id);
  const dx = e.clientX - dragState.startX;
  const dy = e.clientY - dragState.startY;
  win.style.left = (dragState.origLeft + dx) + "px";
  win.style.top  = (dragState.origTop  + dy) + "px";
}

function stopDrag() {
  if (dragState) {
    const win = document.getElementById(dragState.id);
    win.classList.remove("dragging");
    dragState = null;
  }
  document.onmousemove = null;
  document.onmouseup   = null;
}

function openWindow(id) {
  const win = document.getElementById(id);
  if (win) {
    win.style.display = "flex";
    win.style.zIndex  = 850;
    // Add taskbar button
    addTaskbarBtn(id, win.querySelector(".title-bar-text")?.textContent || id);
  }
}

function closeWindow(id) {
  const win = document.getElementById(id);
  if (win) win.style.display = "none";
  removeTaskbarBtn(id);
}

function minimizeWindow(id) {
  const win = document.getElementById(id);
  if (win) win.style.display = "none";
}

function maximizeWindow(id) {
  const win = document.getElementById(id);
  if (!win) return;
  if (win._maximized) {
    win.style.left   = win._prevLeft;
    win.style.top    = win._prevTop;
    win.style.width  = win._prevWidth;
    win.style.height = win._prevHeight;
    win._maximized = false;
  } else {
    win._prevLeft   = win.style.left;
    win._prevTop    = win.style.top;
    win._prevWidth  = win.style.width;
    win._prevHeight = win.style.height;
    win.style.left   = "0";
    win.style.top    = "0";
    win.style.width  = "100%";
    win.style.height = "calc(100% - 28px)";
    win._maximized = true;
  }
}

function addTaskbarBtn(id, label) {
  if (document.getElementById(`task-${id}`)) return;
  const bar = document.querySelector(".win98-taskbar");
  const btn = document.createElement("button");
  btn.className = "taskbar-btn";
  btn.id = `task-${id}`;
  btn.textContent = label.substring(0, 24);
  btn.onclick = () => openWindow(id);
  bar.insertBefore(btn, bar.querySelector(".system-tray"));
}

function removeTaskbarBtn(id) {
  const btn = document.getElementById(`task-${id}`);
  if (btn) btn.remove();
}

/* ══════════════════════════════════════════════════════════════
   START MENU
══════════════════════════════════════════════════════════════ */

function toggleStartMenu() {
  const menu = document.getElementById("startMenu");
  menu.style.display = menu.style.display === "none" ? "block" : "none";
}

// Close start menu when clicking elsewhere
document.addEventListener("click", (e) => {
  const menu = document.getElementById("startMenu");
  const btn  = document.getElementById("startBtn");
  if (menu && !menu.contains(e.target) && e.target !== btn) {
    menu.style.display = "none";
  }
});

/* ══════════════════════════════════════════════════════════════
   CRT POWER TOGGLE
══════════════════════════════════════════════════════════════ */

let powerOn = true;

function togglePower() {
  const screen = document.querySelector(".crt-screen");
  const led    = document.getElementById("powerLed");

  if (powerOn) {
    screen.classList.add("powering-off");
    led.style.background = "#330000";
    led.style.boxShadow = "0 0 3px #330000";
    setTimeout(() => {
      screen.querySelector(".win98-desktop").style.display = "none";
    }, 600);
    powerOn = false;
  } else {
    screen.classList.remove("powering-off");
    screen.querySelector(".win98-desktop").style.display = "block";
    screen.classList.add("powering-on");
    led.style.background = "";
    led.style.boxShadow = "";
    setTimeout(() => screen.classList.remove("powering-on"), 900);
    powerOn = true;
  }
}

function toggleBrightness() {
  const screen = document.querySelector(".crt-screen");
  const current = parseFloat(screen.style.filter?.match(/brightness\((.+?)\)/)?.[1] || 1);
  const next = current >= 1.3 ? 0.7 : current + 0.2;
  screen.style.filter = `brightness(${next.toFixed(1)})`;
  logTerminal("system", `> Monitor brightness: ${Math.round(next * 100)}%`);
}

function showShutdown() {
  const confirmed = confirm("Are you sure you want to shut down ProViber?");
  if (confirmed) togglePower();
}

/* ══════════════════════════════════════════════════════════════
   UTILITIES
══════════════════════════════════════════════════════════════ */

function clearOutput() {
  const terminal = document.getElementById("terminal");
  terminal.innerHTML = '<div class="terminal-line blink-line">> <span class="cursor">█</span></div>';
  document.getElementById("codeBlock").textContent = "// Code cleared.";
  resetLoopNodes();
  updatePhase("Idle");
  document.getElementById("attemptCount").textContent = "—";
}

function copyCode() {
  if (!generatedCode) { logTerminal("error", "> Nothing to copy!"); return; }
  navigator.clipboard.writeText(generatedCode)
    .then(() => logTerminal("success", "> ✅ Code copied to clipboard!"))
    .catch(() => logTerminal("error", "> ❌ Could not copy to clipboard."));
}

function exportCode() {
  if (!generatedCode) { logTerminal("error", "> No code to export!"); return; }
  const blob = new Blob([generatedCode], { type: "text/plain" });
  const url  = URL.createObjectURL(blob);
  const a    = document.createElement("a");
  a.href     = url;
  a.download = "main.go";
  a.click();
  URL.revokeObjectURL(url);
  logTerminal("success", "> 💾 Code exported as main.go");
}

function newSession() {
  clearOutput();
  document.getElementById("vibeInput").value = "";
  document.getElementById("vibeInput").focus();
  sessionId = null;
  setSBSession("None");
  logTerminal("info", "> 🆕 New session started. Enter your vibe!");
}

function saveSettings() {
  const wsUrl   = document.getElementById("settingWsUrl").value;
  const fontSize = document.getElementById("settingFontSize").value;
  const crt     = document.getElementById("settingCrtEffect").checked;

  // Apply font size
  document.querySelector(".terminal").style.fontSize = fontSize;
  document.querySelector(".code-output").style.fontSize = fontSize;

  // Apply CRT toggle
  document.querySelector(".scanlines").style.display = crt ? "block" : "none";
  document.querySelector(".vignette").style.display  = crt ? "block" : "none";

  logTerminal("success", `> ⚙️ Settings saved. WS: ${wsUrl}`);
  closeWindow("settingsWindow");
}

/* ══════════════════════════════════════════════════════════════
   CLOCK
══════════════════════════════════════════════════════════════ */

function updateClocks() {
  const now = new Date();
  const hh  = String(now.getHours()).padStart(2, "0");
  const mm  = String(now.getMinutes()).padStart(2, "0");
  const ss  = String(now.getSeconds()).padStart(2, "0");

  const taskbar = document.getElementById("taskbarClock");
  const sb      = document.getElementById("sb-time");
  if (taskbar) taskbar.textContent = `${hh}:${mm}`;
  if (sb)      sb.textContent     = `${hh}:${mm}:${ss}`;
}
setInterval(updateClocks, 1000);
updateClocks();

/* ══════════════════════════════════════════════════════════════
   KEYBOARD SHORTCUTS
══════════════════════════════════════════════════════════════ */

document.addEventListener("keydown", (e) => {
  // Ctrl+Enter → send vibe
  if (e.ctrlKey && e.key === "Enter") {
    e.preventDefault();
    sendVibe();
  }
  // Escape → stop loop
  if (e.key === "Escape") {
    stopLoop();
  }
  // Ctrl+K → clear
  if (e.ctrlKey && e.key === "k") {
    e.preventDefault();
    clearOutput();
  }
});

/* ══════════════════════════════════════════════════════════════
   BOOT SEQUENCE
══════════════════════════════════════════════════════════════ */

window.addEventListener("load", () => {
  // CRT power-on effect
  const screen = document.querySelector(".crt-screen");
  screen.classList.add("powering-on");
  setTimeout(() => screen.classList.remove("powering-on"), 1000);

  // Boot log sequence
  const bootMessages = [
    { type: "system", text: "> BIOS v2.04 — ProViber BIOS (C) 2000-2025 ProViber Corp." },
    { type: "system", text: "> Detecting memory... 640K OK" },
    { type: "system", text: "> Initializing CRT display... OK" },
    { type: "system", text: "> Loading ProViber OS v1.0..." },
    { type: "info",   text: "> Mounting workspace volume..." },
    { type: "info",   text: "> Starting WebSocket client..." },
  ];

  bootMessages.forEach((msg, i) => {
    setTimeout(() => logTerminal(msg.type, msg.text), i * 200);
  });

  // Connect WebSocket after boot sequence
  setTimeout(() => connectWS(), bootMessages.length * 200 + 300);
});
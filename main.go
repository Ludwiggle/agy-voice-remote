package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var embeddedFiles embed.FS

type serverConfig struct {
	host        string
	port        int
	ttydAddr    string
	tmuxBin     string
	agyBin      string
	tmuxSession string
	gcpProject  string
	tokenFile   string
	homeDir     string
}

type folderOption struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type voiceServer struct {
	cfg      serverConfig
	webFS    fs.FS
	proxy    *httputil.ReverseProxy
	tmuxLock sync.Mutex
	workDir  string
}

func envOrDefault(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}

func resolveHomePath(p, homeDir string) string {
	if p == "~" {
		return homeDir
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir, p[2:])
	}
	return p
}

func (s *voiceServer) ensureToken() string {
	if data, err := os.ReadFile(s.cfg.tokenFile); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok
		}
	}

	dir := filepath.Dir(s.cfg.tokenFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("[agy-voice-ui] Warning: failed to create token directory %s: %v", dir, err)
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		log.Fatalf("[agy-voice-ui] Failed to generate random token: %v", err)
	}
	tok := hex.EncodeToString(raw)

	if err := os.WriteFile(s.cfg.tokenFile, []byte(tok+"\n"), 0600); err != nil {
		log.Printf("[agy-voice-ui] Warning: failed to write token file %s: %v", s.cfg.tokenFile, err)
	}
	return tok
}

func constantTimeEqual(a, b string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *voiceServer) isAuthorized(r *http.Request) bool {
	expected := s.ensureToken()

	// 1. URL query param ?token=<hex>
	if qTok := strings.TrimSpace(r.URL.Query().Get("token")); qTok != "" {
		if constantTimeEqual(qTok, expected) {
			return true
		}
	}

	// 2. Header X-AGY-Token
	if hTok := strings.TrimSpace(r.Header.Get("X-AGY-Token")); hTok != "" {
		if constantTimeEqual(hTok, expected) {
			return true
		}
	}

	// 3. Header Authorization: Bearer <hex>
	authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHdr) > 7 && strings.EqualFold(authHdr[:7], "Bearer ") {
		bTok := strings.TrimSpace(authHdr[7:])
		if constantTimeEqual(bTok, expected) {
			return true
		}
	}

	// 4. Cookie agy_token
	if c, err := r.Cookie("agy_token"); err == nil {
		cTok := strings.TrimSpace(c.Value)
		if constantTimeEqual(cTok, expected) {
			return true
		}
	}

	return false
}

func (s *voiceServer) setTokenCookie(w http.ResponseWriter) {
	tok := s.ensureToken()
	w.Header().Set("Set-Cookie", fmt.Sprintf("agy_token=%s; Path=/; Max-Age=31536000; SameSite=Lax", tok))
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"internal json error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *voiceServer) runTmux(args ...string) (string, string, error) {
	cmd := exec.Command(s.cfg.tmuxBin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (s *voiceServer) isSessionActive() bool {
	_, _, err := s.runTmux("has-session", "-t", s.cfg.tmuxSession)
	return err == nil
}

func (s *voiceServer) getPanePID() string {
	out, _, err := s.runTmux("list-panes", "-t", s.cfg.tmuxSession, "-F", "#{pane_pid}")
	if err != nil {
		return ""
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	return strings.TrimSpace(lines[0])
}

func (s *voiceServer) isPaneInCopyMode() bool {
	out, _, err := s.runTmux("list-panes", "-t", s.cfg.tmuxSession, "-F", "#{pane_in_mode}")
	if err != nil {
		return false
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return false
	}
	lines := strings.Split(trimmed, "\n")
	return strings.TrimSpace(lines[0]) == "1"
}

func (s *voiceServer) getPanePath() string {
	out, _, err := s.runTmux("list-panes", "-t", s.cfg.tmuxSession, "-F", "#{pane_current_path}")
	if err != nil {
		return ""
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	return strings.TrimSpace(lines[0])
}

func (s *voiceServer) validateFolder(folder string) (string, error) {
	resolved := resolveHomePath(strings.TrimSpace(folder), s.cfg.homeDir)
	cleanPath := filepath.Clean(resolved)
	homeClean := filepath.Clean(s.cfg.homeDir)

	if cleanPath != homeClean && !strings.HasPrefix(cleanPath, homeClean+string(filepath.Separator)) {
		return "", fmt.Errorf("folder must be inside %s", homeClean)
	}
	info, err := os.Stat(cleanPath)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("directory not found: %s", cleanPath)
	}
	return cleanPath, nil
}

func (s *voiceServer) currentWorkDir() string {
	if p := s.getPanePath(); p != "" {
		if valid, err := s.validateFolder(p); err == nil {
			return valid
		}
	}
	if s.workDir != "" {
		if valid, err := s.validateFolder(s.workDir); err == nil {
			return valid
		}
	}
	return s.cfg.homeDir
}

func (s *voiceServer) listFolders() []folderOption {
	folders := []folderOption{
		{
			Name: "~ (Home)",
			Path: s.cfg.homeDir,
		},
	}

	gitsDir := filepath.Join(s.cfg.homeDir, "Gits")
	if entries, err := os.ReadDir(gitsDir); err == nil {
		var names []string
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			fullPath := filepath.Join(gitsDir, name)
			if info, err := os.Stat(fullPath); err == nil && info.IsDir() {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			folders = append(folders, folderOption{
				Name: name,
				Path: filepath.Join(gitsDir, name),
			})
		}
	}

	activeDir := s.currentWorkDir()
	found := false
	for _, f := range folders {
		if f.Path == activeDir {
			found = true
			break
		}
	}
	if !found && activeDir != "" {
		folders = append(folders, folderOption{
			Name: filepath.Base(activeDir),
			Path: activeDir,
		})
	}

	return folders
}

func (s *voiceServer) ensureTmuxSession() {
	if !s.isSessionActive() {
		workDir := s.workDir
		if workDir == "" {
			workDir = s.cfg.homeDir
		}
		loopCmd := fmt.Sprintf("while true; do %s; sleep 1; done", s.cfg.agyBin)
		_, _, _ = s.runTmux(
			"new-session",
			"-d",
			"-s", s.cfg.tmuxSession,
			"-c", workDir,
			loopCmd,
		)
	}
	_, _, _ = s.runTmux("set-option", "-t", s.cfg.tmuxSession, "mouse", "on")
}

func (s *voiceServer) exitCopyModeIfActive() {
	_, _, _ = s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-X", "cancel")
	_, _, _ = s.runTmux("copy-mode", "-t", s.cfg.tmuxSession, "-q")
}

func (s *voiceServer) sendPromptToTmux(prompt string, submit bool) (bool, string) {
	s.tmuxLock.Lock()
	defer s.tmuxLock.Unlock()

	s.ensureTmuxSession()
	s.exitCopyModeIfActive()

	normalized := strings.ReplaceAll(prompt, "\r\n", "\n")
	cleaned := strings.TrimSpace(normalized)
	if cleaned == "" {
		if submit {
			_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Enter")
			if err != nil {
				return false, strings.TrimSpace(stderr)
			}
		}
		return true, ""
	}

	lines := strings.Split(cleaned, "\n")
	for idx, line := range lines {
		if line != "" {
			_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-l", "--", line)
			if err != nil {
				return false, strings.TrimSpace(stderr)
			}
		}
		if idx < len(lines)-1 {
			_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Enter")
			if err != nil {
				return false, strings.TrimSpace(stderr)
			}
			time.Sleep(40 * time.Millisecond)
		}
	}

	if submit {
		time.Sleep(60 * time.Millisecond)
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Enter")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
	}

	return true, ""
}

func (s *voiceServer) sendClearToTmuxLocked() (bool, string) {
	s.ensureTmuxSession()
	s.exitCopyModeIfActive()

	if _, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "C-u"); err != nil {
		return false, strings.TrimSpace(stderr)
	}
	time.Sleep(40 * time.Millisecond)

	if _, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-l", "--", "/clear"); err != nil {
		return false, strings.TrimSpace(stderr)
	}
	time.Sleep(60 * time.Millisecond)

	if _, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Enter"); err != nil {
		return false, strings.TrimSpace(stderr)
	}

	return true, ""
}

func (s *voiceServer) sendClearToTmux() (bool, string) {
	s.tmuxLock.Lock()
	defer s.tmuxLock.Unlock()
	return s.sendClearToTmuxLocked()
}

func (s *voiceServer) restartTmuxSession(targetFolder string) (bool, string) {
	s.tmuxLock.Lock()
	defer s.tmuxLock.Unlock()

	if strings.TrimSpace(targetFolder) != "" {
		validDir, err := s.validateFolder(targetFolder)
		if err != nil {
			return false, err.Error()
		}
		s.workDir = validDir
	} else if s.workDir == "" {
		s.workDir = s.currentWorkDir()
	}

	workDir := s.workDir
	if workDir == "" {
		workDir = s.cfg.homeDir
	}

	if s.isSessionActive() {
		s.exitCopyModeIfActive()
		loopCmd := fmt.Sprintf("while true; do %s; sleep 1; done", s.cfg.agyBin)
		_, stderr, err := s.runTmux(
			"respawn-pane",
			"-k",
			"-t", s.cfg.tmuxSession,
			"-c", workDir,
			loopCmd,
		)
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		_, _, _ = s.runTmux("clear-history", "-t", s.cfg.tmuxSession)
		_, _, _ = s.runTmux("set-option", "-t", s.cfg.tmuxSession, "mouse", "on")
		return true, ""
	}

	s.ensureTmuxSession()
	if !s.isSessionActive() {
		return false, "failed to recreate tmux session"
	}
	return true, ""
}

func (s *voiceServer) sendKeyToTmux(action string) (bool, string) {
	s.tmuxLock.Lock()
	defer s.tmuxLock.Unlock()

	s.ensureTmuxSession()

	switch action {
	case "ScrollBottom":
		s.exitCopyModeIfActive()
		return true, ""
	case "ScrollUp":
		_, _, _ = s.runTmux("copy-mode", "-t", s.cfg.tmuxSession, "-e")
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-X", "-N", "5", "scroll-up")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "ScrollDown":
		if s.isPaneInCopyMode() {
			_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-X", "-N", "5", "scroll-down")
			if err != nil {
				return false, strings.TrimSpace(stderr)
			}
		}
		return true, ""
	case "PageUp":
		_, _, _ = s.runTmux("copy-mode", "-t", s.cfg.tmuxSession, "-e")
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-X", "page-up")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "PageDown":
		if s.isPaneInCopyMode() {
			_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-X", "page-down")
			if err != nil {
				return false, strings.TrimSpace(stderr)
			}
		}
		return true, ""
	case "Up":
		if s.isPaneInCopyMode() {
			_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-X", "cursor-up")
			if err != nil {
				return false, strings.TrimSpace(stderr)
			}
			return true, ""
		}
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Up")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "Down":
		if s.isPaneInCopyMode() {
			_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-X", "cursor-down")
			if err != nil {
				return false, strings.TrimSpace(stderr)
			}
			return true, ""
		}
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Down")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "Left", "Right":
		if s.isPaneInCopyMode() {
			xKey := "cursor-left"
			if action == "Right" {
				xKey = "cursor-right"
			}
			_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-X", xKey)
			if err != nil {
				return false, strings.TrimSpace(stderr)
			}
			return true, ""
		}
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, action)
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	}

	s.exitCopyModeIfActive()

	switch action {
	case "/", "/slash":
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "-l", "--", "/")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "Escape", "Esc":
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Escape")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "Enter":
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Enter")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "Tab":
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Tab")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "Ctrl+C", "C-c", "Interrupt", "interrupt":
		// Use Escape (agy's native interrupt/cancel key) instead of C-c so
		// pressing Interrupt when no turn is running never exits agy.
		_, stderr, err := s.runTmux("send-keys", "-t", s.cfg.tmuxSession, "Escape")
		if err != nil {
			return false, strings.TrimSpace(stderr)
		}
		return true, ""
	case "/clear":
		return s.sendClearToTmuxLocked()
	default:
		return false, fmt.Sprintf("Unsupported action: %s", action)
	}
}

func (s *voiceServer) handleManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"ok":    false,
			"error": "Method not allowed",
		})
		return
	}
	manifest := map[string]interface{}{
		"name":             fmt.Sprintf("agy Voice Remote (%s)", s.cfg.gcpProject),
		"short_name":       "agy Voice",
		"start_url":        "/",
		"display":          "standalone",
		"background_color": "#0d1117",
		"theme_color":      "#0d1117",
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *voiceServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"ok":    false,
			"error": "Method not allowed",
		})
		return
	}
	if !s.isAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"ok":    false,
			"error": "Unauthorized",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":             true,
		"project":        s.cfg.gcpProject,
		"tmux_session":   s.cfg.tmuxSession,
		"session_active": s.isSessionActive(),
		"pane_pid":       s.getPanePID(),
		"workdir":        s.currentWorkDir(),
		"folders":        s.listFolders(),
		"in_copy_mode":   s.isPaneInCopyMode(),
	})
}

type sendRequest struct {
	Prompt string `json:"prompt"`
	Submit *bool  `json:"submit"`
}

func (s *voiceServer) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"ok":    false,
			"error": "Method not allowed",
		})
		return
	}
	if !s.isAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"ok":    false,
			"error": "Unauthorized",
		})
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok":    false,
			"error": "Failed to read request body",
		})
		return
	}

	var req sendRequest
	if len(strings.TrimSpace(string(bodyBytes))) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"ok":    false,
				"error": "Invalid JSON",
			})
			return
		}
	}

	submit := true
	if req.Submit != nil {
		submit = *req.Submit
	}

	ok, errMsg := s.sendPromptToTmux(req.Prompt, submit)
	if !ok {
		if errMsg == "" {
			errMsg = "tmux send-keys failed"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"ok":    false,
			"error": errMsg,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

type keyRequest struct {
	Action string `json:"action"`
}

func (s *voiceServer) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"ok":    false,
			"error": "Method not allowed",
		})
		return
	}
	if !s.isAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"ok":    false,
			"error": "Unauthorized",
		})
		return
	}

	ok, errMsg := s.sendClearToTmux()
	if !ok {
		if errMsg == "" {
			errMsg = "tmux clear failed"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"ok":    false,
			"error": errMsg,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

type restartRequest struct {
	Folder string `json:"folder"`
}

func (s *voiceServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"ok":    false,
			"error": "Method not allowed",
		})
		return
	}
	if !s.isAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"ok":    false,
			"error": "Unauthorized",
		})
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok":    false,
			"error": "Failed to read request body",
		})
		return
	}
	var req restartRequest
	if len(strings.TrimSpace(string(bodyBytes))) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"ok":    false,
				"error": "Invalid JSON",
			})
			return
		}
	}

	ok, errMsg := s.restartTmuxSession(req.Folder)
	if !ok {
		if errMsg == "" {
			errMsg = "tmux restart failed"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"ok":    false,
			"error": errMsg,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"workdir": s.currentWorkDir(),
	})
}

func (s *voiceServer) handleKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"ok":    false,
			"error": "Method not allowed",
		})
		return
	}
	if !s.isAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"ok":    false,
			"error": "Unauthorized",
		})
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok":    false,
			"error": "Failed to read request body",
		})
		return
	}
	var req keyRequest
	if len(strings.TrimSpace(string(bodyBytes))) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"ok":    false,
				"error": "Invalid JSON",
			})
			return
		}
	}

	action := strings.TrimSpace(req.Action)
	if action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok":    false,
			"error": "Missing action",
		})
		return
	}

	ok, errMsg := s.sendKeyToTmux(action)
	if !ok {
		if errMsg == "" {
			errMsg = fmt.Sprintf("tmux action %s failed", action)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"ok":    false,
			"error": errMsg,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	connHdr := r.Header.Get("Connection")
	for _, part := range strings.Split(connHdr, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
			return true
		}
	}
	return strings.Contains(strings.ToLower(connHdr), "upgrade")
}

func (s *voiceServer) proxyWebSocket(w http.ResponseWriter, r *http.Request) {
	backendConn, err := net.DialTimeout("tcp", s.cfg.ttydAddr, 5*time.Second)
	if err != nil {
		log.Printf("[agy-voice-ui] Failed to connect to ttyd backend %s: %v", s.cfg.ttydAddr, err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = backendConn.Close()
		http.Error(w, "WebSocket hijack not supported", http.StatusInternalServerError)
		return
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		_ = backendConn.Close()
		log.Printf("[agy-voice-ui] Hijack failed: %v", err)
		return
	}

	if err := r.Write(backendConn); err != nil {
		_ = clientConn.Close()
		_ = backendConn.Close()
		return
	}

	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buffered := make([]byte, clientBuf.Reader.Buffered())
		if n, err := clientBuf.Read(buffered); err == nil && n > 0 {
			if _, err := backendConn.Write(buffered[:n]); err != nil {
				_ = clientConn.Close()
				_ = backendConn.Close()
				return
			}
		}
	}

	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = clientConn.Close()
			_ = backendConn.Close()
		})
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(backendConn, clientConn)
		closeBoth()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, backendConn)
		closeBoth()
		done <- struct{}{}
	}()

	<-done
}

func (s *voiceServer) handleTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"ok":    false,
			"error": "Unauthorized",
		})
		return
	}

	if r.URL.Query().Get("token") != "" {
		s.setTokenCookie(w)
	}

	if r.URL.Path == "/terminal" {
		target := "/terminal/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}

	if isWebSocketUpgrade(r) {
		s.proxyWebSocket(w, r)
		return
	}

	s.proxy.ServeHTTP(w, r)
}

func (s *voiceServer) serveStaticAsset(w http.ResponseWriter, r *http.Request, name, contentType string, setCookieIfAuth bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"ok":    false,
			"error": "Method not allowed",
		})
		return
	}

	data, err := fs.ReadFile(s.webFS, name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"ok":    false,
			"error": "Not found",
		})
		return
	}

	if setCookieIfAuth && s.isAuthorized(r) {
		s.setTokenCookie(w)
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (s *voiceServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		s.serveStaticAsset(w, r, "index.html", "text/html; charset=utf-8", true)
	case r.URL.Path == "/style.css":
		s.serveStaticAsset(w, r, "style.css", "text/css; charset=utf-8", false)
	case r.URL.Path == "/app.js":
		s.serveStaticAsset(w, r, "app.js", "application/javascript; charset=utf-8", false)
	case r.URL.Path == "/manifest.json":
		s.handleManifest(w, r)
	case r.URL.Path == "/api/status":
		s.handleStatus(w, r)
	case r.URL.Path == "/api/send":
		s.handleSend(w, r)
	case r.URL.Path == "/api/clear":
		s.handleClear(w, r)
	case r.URL.Path == "/api/restart":
		s.handleRestart(w, r)
	case r.URL.Path == "/api/key":
		s.handleKey(w, r)
	case r.URL.Path == "/terminal" || strings.HasPrefix(r.URL.Path, "/terminal/"):
		s.handleTerminal(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"ok":    false,
			"error": "Not found",
		})
	}
}

func main() {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		homeDir = "/home/nicola"
	}

	defaultHost := envOrDefault("AGY_HOST", "0.0.0.0")
	defaultPort := 7680
	defaultTtydAddr := envOrDefault("AGY_TTYD_ADDR", "127.0.0.1:7681")

	hostFlag := flag.String("host", defaultHost, "Bind host address")
	portFlag := flag.Int("port", defaultPort, "Listen TCP port")
	ttydAddrFlag := flag.String("ttyd-addr", defaultTtydAddr, "Loopback ttyd address (host:port)")
	flag.Parse()

	cfg := serverConfig{
		host:        *hostFlag,
		port:        *portFlag,
		ttydAddr:    *ttydAddrFlag,
		tmuxBin:     envOrDefault("AGY_TMUX_BIN", filepath.Join(homeDir, ".local", "bin", "tmux")),
		agyBin:      envOrDefault("AGY_BIN", filepath.Join(homeDir, ".local", "bin", "agy")),
		tmuxSession: envOrDefault("AGY_TMUX_SESSION", "agy"),
		gcpProject:  envOrDefault("AGY_GCP_PROJECT", "evolvere-iot-poc"),
		tokenFile:   resolveHomePath(envOrDefault("AGY_TOKEN_FILE", filepath.Join(homeDir, ".config", "agy-remote", "token")), homeDir),
		homeDir:     homeDir,
	}

	webFS, err := fs.Sub(embeddedFiles, "web")
	if err != nil {
		log.Fatalf("[agy-voice-ui] Failed to initialize embedded web filesystem: %v", err)
	}

	ttydURL, err := url.Parse("http://" + cfg.ttydAddr)
	if err != nil {
		log.Fatalf("[agy-voice-ui] Invalid ttyd address %q: %v", cfg.ttydAddr, err)
	}

	srv := &voiceServer{
		cfg:     cfg,
		webFS:   webFS,
		proxy:   httputil.NewSingleHostReverseProxy(ttydURL),
		workDir: cfg.homeDir,
	}
	srv.workDir = srv.currentWorkDir()

	_ = srv.ensureToken()
	listenAddr := fmt.Sprintf("%s:%d", cfg.host, cfg.port)
	log.Printf("[agy-voice-ui] Token loaded from %s", cfg.tokenFile)
	log.Printf("[agy-voice-ui] Proxying /terminal/* -> http://%s", cfg.ttydAddr)
	log.Printf("[agy-voice-ui] Listening on http://%s", listenAddr)

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[agy-voice-ui] Server error: %v", err)
	}
}

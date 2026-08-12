package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		next.ServeHTTP(w, r)
	})
}

// makeLogger opens the log file at the given path. path always comes from
// -log / config.go's LogFile default — an operator-supplied CLI flag or
// config file, not remote/user input — so this isn't attacker-controlled
// path traversal (gosec G304).
func makeLogger(path string) *log.Logger {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) // #nosec G304 -- operator-supplied path via CLI/config; 0600: log lines can contain client IPs/emails, no need for group/world read (gosec G302)
	if err != nil {
		log.Fatalf("Log file: %v", err)
	}
	return log.New(io.MultiWriter(os.Stdout, f), "", log.Ldate|log.Ltime)
}

func wsHandler(cfg Config, hub *Hub, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.MaxClients > 0 && hub.Count() >= cfg.MaxClients {
			logger.Printf("Rejected %s: max clients (%d) reached", r.RemoteAddr, cfg.MaxClients)
			http.Error(w, "server full", http.StatusServiceUnavailable)
			return
		}

		var identity string

		if cfg.NoAuth {
			identity = "anonymous (auth disabled)"
		} else {
			token := r.URL.Query().Get("token")
			payload, err := validateJWT(token, cfg.JWTSecret)
			if err != nil {
				logger.Printf("Auth FAILED from %s: %v", r.RemoteAddr, err)
				http.Error(w, "unauthorized: invalid or expired token", http.StatusUnauthorized)
				return
			}
			identity = fmt.Sprintf("%s (level=%s)", payload.Email, payload.Level)
		}

		logger.Printf("Client authenticated: %s from %s", identity, r.RemoteAddr)

		ws, err := upgradeWS(w, r, logger)
		if err != nil {
			logger.Printf("WS upgrade error: %v", err)
			http.Error(w, "websocket required", http.StatusBadRequest)
			return
		}

		remote := ws.conn.RemoteAddr().String()
		hub.Add(ws)
		count := hub.Count()
		logger.Printf("Client connected: %s (total: %d)", remote, count)
		hub.BroadcastControl(fmt.Sprintf(`{"type":"client_count","count":%d}`, count))

		ws.Wait()

		hub.Remove(ws)
		count = hub.Count()
		logger.Printf("Client disconnected: %s (total: %d)", remote, count)
		hub.BroadcastControl(fmt.Sprintf(`{"type":"client_count","count":%d}`, count))
	}
}

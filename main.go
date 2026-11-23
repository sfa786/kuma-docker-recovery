package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ListenAddress string            `json:"listen_address"`
	WebhookToken  string            `json:"webhook_token"`
	DockerSocket  string            `json:"docker_socket"`
	DockerAPIVer  string            `json:"docker_api_version"`
	RestartDelay  int               `json:"restart_delay_seconds"`
	CooldownSecs  int               `json:"cooldown_seconds"`
	MaxRestarts   int               `json:"max_restarts"`
	WindowSecs    int               `json:"window_seconds"`
	Containers    map[string]string `json:"containers"`
}

type KumaPayload struct {
	Monitor struct {
		Name string `json:"name"`
	} `json:"monitor"`
	Heartbeat struct {
		Status int    `json:"status"`
		Msg    string `json:"msg"`
	} `json:"heartbeat"`
}

type RestartRecord struct {
	LastRestart time.Time
	Attempts    []time.Time
}

type App struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex
	state  map[string]RestartRecord
}

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.ListenAddress == "" {
		c.ListenAddress = ":8080"
	}
	if c.DockerSocket == "" {
		c.DockerSocket = "/var/run/docker.sock"
	}
	if c.DockerAPIVer == "" {
		c.DockerAPIVer = "v1.41"
	}
	if c.CooldownSecs <= 0 {
		c.CooldownSecs = 300
	}
	if c.MaxRestarts <= 0 {
		c.MaxRestarts = 3
	}
	if c.WindowSecs <= 0 {
		c.WindowSecs = 1800
	}
	if c.RestartDelay < 0 {
		c.RestartDelay = 0
	}
	if c.WebhookToken == "" {
		return c, errors.New("webhook_token must be configured")
	}
	if len(c.Containers) == 0 {
		return c, errors.New("containers mapping must not be empty")
	}
	return c, nil
}

func main() {
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = "/config/config.json"
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	// Docker Engine API is accessed over the mounted Unix socket.
	dockerTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", cfg.DockerSocket)
		},
	}
	app := &App{
		cfg:    cfg,
		client: &http.Client{Transport: dockerTransport, Timeout: 20 * time.Second},
		state:  make(map[string]RestartRecord),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/webhook/kuma/", app.handleWebhook)

	log.Printf("Kuma Docker Recovery listening on %s; configured monitors: %d", cfg.ListenAddress, len(cfg.Containers))
	server := &http.Server{Addr: cfg.ListenAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func (a *App) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	provided := strings.TrimPrefix(r.URL.Path, "/webhook/kuma/")
	if provided == "" || strings.Contains(provided, "/") ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(a.cfg.WebhookToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	defer r.Body.Close()
	var payload KumaPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}

	monitor := strings.TrimSpace(payload.Monitor.Name)
	if monitor == "" {
		http.Error(w, "monitor.name is required", http.StatusBadRequest)
		return
	}
	container, ok := a.cfg.Containers[monitor]
	if !ok {
		log.Printf("ignored unmapped monitor=%q status=%d", monitor, payload.Heartbeat.Status)
		http.Error(w, "monitor is not mapped", http.StatusNotFound)
		return
	}

	// Uptime Kuma heartbeat status: 0 = DOWN, 1 = UP, 2 = PENDING.
	if payload.Heartbeat.Status != 0 {
		writeJSON(w, http.StatusOK, map[string]string{"result": "ignored", "reason": "heartbeat is not DOWN"})
		return
	}

	if a.cfg.RestartDelay > 0 {
		time.Sleep(time.Duration(a.cfg.RestartDelay) * time.Second)
	}

	if err := a.allowRestart(monitor); err != nil {
		log.Printf("restart suppressed monitor=%q container=%q reason=%v", monitor, container, err)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"result": "suppressed", "reason": err.Error()})
		return
	}

	if err := a.restartContainer(container); err != nil {
		log.Printf("restart failed monitor=%q container=%q error=%v", monitor, container, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"result": "failed", "error": err.Error()})
		return
	}

	log.Printf("restarted container=%q for DOWN monitor=%q message=%q", container, monitor, payload.Heartbeat.Msg)
	writeJSON(w, http.StatusOK, map[string]string{"result": "restarted", "container": container})
}

func (a *App) allowRestart(monitor string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	rec := a.state[monitor]
	if !rec.LastRestart.IsZero() && now.Sub(rec.LastRestart) < time.Duration(a.cfg.CooldownSecs)*time.Second {
		return fmt.Errorf("cooldown active until %s", rec.LastRestart.Add(time.Duration(a.cfg.CooldownSecs)*time.Second).Format(time.RFC3339))
	}

	cutoff := now.Add(-time.Duration(a.cfg.WindowSecs) * time.Second)
	kept := rec.Attempts[:0]
	for _, t := range rec.Attempts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	rec.Attempts = kept
	if len(rec.Attempts) >= a.cfg.MaxRestarts {
		return fmt.Errorf("maximum restarts reached (%d in configured window)", a.cfg.MaxRestarts)
	}
	rec.LastRestart = now
	rec.Attempts = append(rec.Attempts, now)
	a.state[monitor] = rec
	return nil
}

func (a *App) restartContainer(name string) error {
	escaped := url.PathEscape(name)
	endpoint := fmt.Sprintf("http://docker/%s/containers/%s/restart?t=10", a.cfg.DockerAPIVer, escaped)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("Docker API request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("Docker API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("container %q not found", name)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

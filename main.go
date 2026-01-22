package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram struct {
		BotToken string `yaml:"bot_token" json:"bot_token"`
		ChatID   string `yaml:"chat_id" json:"chat_id"`
	} `yaml:"telegram" json:"telegram"`
	Settings struct {
		CheckInterval    string `yaml:"check_interval" json:"check_interval"`
		FailureThreshold int    `yaml:"failure_threshold" json:"failure_threshold"`
		SlowThreshold    string `yaml:"slow_threshold" json:"slow_threshold"`
		Port             string `yaml:"port" json:"port"`
		Timeout          string `yaml:"timeout" json:"timeout"`
	} `yaml:"settings" json:"settings"`
	Endpoints []Endpoint `yaml:"endpoints" json:"endpoints"`
}

type Endpoint struct {
	Name       string `yaml:"name" json:"name"`
	URL        string `yaml:"url" json:"url"`
	DisplayURL string `yaml:"display_url,omitempty" json:"display_url,omitempty"`
	Type       string `yaml:"type" json:"type"` // http, jsonrpc, tendermint, grpc, port, tcp, websocket

	// HTTP options
	Method      string            `yaml:"method,omitempty" json:"method,omitempty"`             // GET, POST, PUT, DELETE, HEAD, OPTIONS
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`           // Custom headers
	Body        string            `yaml:"body,omitempty" json:"body,omitempty"`                 // Request body
	ContentType string            `yaml:"content_type,omitempty" json:"content_type,omitempty"` // Content-Type header

	// Response validation
	Expected      int    `yaml:"expected,omitempty" json:"expected,omitempty"`               // Expected status code
	ExpectedCodes []int  `yaml:"expected_codes,omitempty" json:"expected_codes,omitempty"`   // Multiple valid codes
	Contains      string `yaml:"contains,omitempty" json:"contains,omitempty"`               // Response must contain
	NotContains   string `yaml:"not_contains,omitempty" json:"not_contains,omitempty"`       // Response must NOT contain
	MatchRegex    string `yaml:"match_regex,omitempty" json:"match_regex,omitempty"`         // Regex to match
	JSONPath      string `yaml:"json_path,omitempty" json:"json_path,omitempty"`             // Simple JSON key check (e.g., "status" or "data.healthy")
	JSONPathValue string `yaml:"json_path_value,omitempty" json:"json_path_value,omitempty"` // Expected value at JSON path

	// Authentication
	BasicAuth   *BasicAuth `yaml:"basic_auth,omitempty" json:"basic_auth,omitempty"`
	BearerToken string     `yaml:"bearer_token,omitempty" json:"bearer_token,omitempty"`

	// TLS/Connection options
	SkipTLSVerify bool   `yaml:"skip_tls_verify,omitempty" json:"skip_tls_verify,omitempty"`
	Timeout       string `yaml:"timeout,omitempty" json:"timeout,omitempty"` // Per-endpoint timeout

	// Port check
	Port int `yaml:"port,omitempty" json:"port,omitempty"`

	// JSON-RPC specific (kept for backward compat)
	RPCMethod string `yaml:"rpc_method,omitempty" json:"rpc_method,omitempty"`
}

type BasicAuth struct {
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
}

type EndpointStatus struct {
	Endpoint     Endpoint
	IsUp         bool
	LastCheck    time.Time
	LastError    string
	ResponseTime time.Duration
	Consecutive  int
	DownSince    time.Time
}

type Monitor struct {
	config           Config
	checkInterval    time.Duration
	failureThreshold int
	slowThreshold    time.Duration
	defaultTimeout   time.Duration
	statuses         map[string]*EndpointStatus
	mu               sync.RWMutex
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Expand environment variables in config
	expanded := os.ExpandEnv(string(data))
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, err
	}
	cfg.applyEnvOverrides()
	return &cfg, nil
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("TELEGRAM_BOT_TOKEN"); v != "" {
		c.Telegram.BotToken = v
	}
	if v := os.Getenv("TELEGRAM_CHAT_ID"); v != "" {
		c.Telegram.ChatID = v
	}
	if v := os.Getenv("CHECK_INTERVAL"); v != "" {
		c.Settings.CheckInterval = v
	}
	if v := os.Getenv("PORT"); v != "" {
		c.Settings.Port = v
	}
}

func NewMonitor(config Config) *Monitor {
	checkInterval, _ := time.ParseDuration(config.Settings.CheckInterval)
	if checkInterval == 0 {
		checkInterval = 60 * time.Second
	}
	slowThreshold, _ := time.ParseDuration(config.Settings.SlowThreshold)
	if slowThreshold == 0 {
		slowThreshold = 5 * time.Second
	}
	failureThreshold := config.Settings.FailureThreshold
	if failureThreshold == 0 {
		failureThreshold = 2
	}
	defaultTimeout, _ := time.ParseDuration(config.Settings.Timeout)
	if defaultTimeout == 0 {
		defaultTimeout = 10 * time.Second
	}

	return &Monitor{
		config:           config,
		checkInterval:    checkInterval,
		failureThreshold: failureThreshold,
		slowThreshold:    slowThreshold,
		defaultTimeout:   defaultTimeout,
		statuses:         make(map[string]*EndpointStatus),
	}
}

func (m *Monitor) createHTTPClient(ep Endpoint) *http.Client {
	timeout := m.defaultTimeout
	if ep.Timeout != "" {
		if t, err := time.ParseDuration(ep.Timeout); err == nil {
			timeout = t
		}
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: ep.SkipTLSVerify,
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}
}

func (m *Monitor) CheckHTTP(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	start := time.Now()
	client := m.createHTTPClient(ep)

	method := strings.ToUpper(ep.Method)
	if method == "" {
		method = "GET"
	}

	var body *bytes.Reader
	if ep.Body != "" {
		body = bytes.NewReader([]byte(ep.Body))
	} else {
		body = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, ep.URL, body)
	if err != nil {
		return false, fmt.Sprintf("request error: %v", err), 0
	}

	// Set headers
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}
	if ep.ContentType != "" {
		req.Header.Set("Content-Type", ep.ContentType)
	}

	// Set auth
	if ep.BasicAuth != nil {
		req.SetBasicAuth(ep.BasicAuth.Username, ep.BasicAuth.Password)
	}
	if ep.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+ep.BearerToken)
	}

	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("failed: %v", err), elapsed
	}
	defer resp.Body.Close()

	// Check status code
	expectedCodes := ep.ExpectedCodes
	if len(expectedCodes) == 0 {
		expected := ep.Expected
		if expected == 0 {
			expected = 200
		}
		expectedCodes = []int{expected}
	}

	statusOK := false
	for _, code := range expectedCodes {
		if resp.StatusCode == code {
			statusOK = true
			break
		}
	}
	if !statusOK {
		return false, fmt.Sprintf("status %d (expected %v)", resp.StatusCode, expectedCodes), elapsed
	}

	// Read body for validation
	if ep.Contains != "" || ep.NotContains != "" || ep.MatchRegex != "" || ep.JSONPath != "" {
		bodyBytes := make([]byte, 1024*1024) // 1MB max
		n, _ := resp.Body.Read(bodyBytes)
		bodyStr := string(bodyBytes[:n])

		if ep.Contains != "" && !strings.Contains(bodyStr, ep.Contains) {
			return false, fmt.Sprintf("response missing '%s'", ep.Contains), elapsed
		}
		if ep.NotContains != "" && strings.Contains(bodyStr, ep.NotContains) {
			return false, fmt.Sprintf("response contains forbidden '%s'", ep.NotContains), elapsed
		}
		if ep.MatchRegex != "" {
			matched, err := regexp.MatchString(ep.MatchRegex, bodyStr)
			if err != nil {
				return false, fmt.Sprintf("regex error: %v", err), elapsed
			}
			if !matched {
				return false, fmt.Sprintf("response doesn't match regex '%s'", ep.MatchRegex), elapsed
			}
		}
		if ep.JSONPath != "" {
			if ok, errMsg := m.checkJSONPath(bodyStr, ep.JSONPath, ep.JSONPathValue); !ok {
				return false, errMsg, elapsed
			}
		}
	}

	return true, "", elapsed
}

func (m *Monitor) checkJSONPath(body, path, expectedValue string) (bool, string) {
	var data interface{}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return false, fmt.Sprintf("invalid JSON: %v", err)
	}

	// Simple dot-notation path traversal
	parts := strings.Split(path, ".")
	current := data

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			var ok bool
			current, ok = v[part]
			if !ok {
				return false, fmt.Sprintf("JSON path '%s' not found (missing '%s')", path, part)
			}
		default:
			return false, fmt.Sprintf("JSON path '%s' not found (cannot traverse '%s')", path, part)
		}
	}

	if expectedValue != "" {
		actualValue := fmt.Sprintf("%v", current)
		if actualValue != expectedValue {
			return false, fmt.Sprintf("JSON '%s' = '%s' (expected '%s')", path, actualValue, expectedValue)
		}
	}

	return true, ""
}

func (m *Monitor) CheckJSONRPC(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	start := time.Now()
	client := m.createHTTPClient(ep)

	method := ep.RPCMethod
	if method == "" {
		method = ep.Method // backward compat
	}
	if method == "" {
		method = "eth_blockNumber"
	}
	payload := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":[],"id":1}`, method))

	req, err := http.NewRequestWithContext(ctx, "POST", ep.URL, bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Sprintf("request error: %v", err), 0
	}
	req.Header.Set("Content-Type", "application/json")

	// Set custom headers and auth
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}
	if ep.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+ep.BearerToken)
	}

	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("failed: %v", err), elapsed
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("status %d", resp.StatusCode), elapsed
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Sprintf("decode error: %v", err), elapsed
	}
	if _, ok := result["result"]; !ok {
		if errObj, ok := result["error"]; ok {
			return false, fmt.Sprintf("RPC error: %v", errObj), elapsed
		}
		return false, "no result", elapsed
	}
	return true, "", elapsed
}

func (m *Monitor) CheckTendermint(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	start := time.Now()
	client := m.createHTTPClient(ep)

	url := strings.TrimSuffix(ep.URL, "/") + "/status"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, fmt.Sprintf("request error: %v", err), 0
	}

	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("failed: %v", err), elapsed
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("status %d", resp.StatusCode), elapsed
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Sprintf("decode error: %v", err), elapsed
	}
	if _, ok := result["result"]; !ok {
		return false, "no result", elapsed
	}
	return true, "", elapsed
}

func (m *Monitor) CheckGRPC(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	start := time.Now()

	timeout := m.defaultTimeout
	if ep.Timeout != "" {
		if t, err := time.ParseDuration(ep.Timeout); err == nil {
			timeout = t
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var opts []grpc.DialOption
	if ep.SkipTLSVerify {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	}
	opts = append(opts, grpc.WithBlock())

	conn, err := grpc.DialContext(dialCtx, ep.URL, opts...)
	elapsed := time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("connection failed: %v", err), elapsed
	}
	defer conn.Close()

	client := grpc_health_v1.NewHealthClient(conn)
	resp, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return true, "", elapsed // Connection worked, health not implemented
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return false, fmt.Sprintf("not serving: %v", resp.Status), elapsed
	}
	return true, "", elapsed
}

func (m *Monitor) CheckPort(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	start := time.Now()

	timeout := m.defaultTimeout
	if ep.Timeout != "" {
		if t, err := time.ParseDuration(ep.Timeout); err == nil {
			timeout = t
		}
	}

	address := ep.URL
	if ep.Port > 0 {
		address = fmt.Sprintf("%s:%d", ep.URL, ep.Port)
	}

	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
	elapsed := time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("unreachable: %v", err), elapsed
	}
	conn.Close()
	return true, "", elapsed
}

func (m *Monitor) CheckTCP(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	return m.CheckPort(ctx, ep)
}

func (m *Monitor) CheckWebSocket(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	start := time.Now()

	timeout := m.defaultTimeout
	if ep.Timeout != "" {
		if t, err := time.ParseDuration(ep.Timeout); err == nil {
			timeout = t
		}
	}

	// For websocket, just check if we can establish TCP connection to the host
	url := ep.URL
	url = strings.TrimPrefix(url, "wss://")
	url = strings.TrimPrefix(url, "ws://")

	// Add default port if missing
	if !strings.Contains(url, ":") {
		if strings.HasPrefix(ep.URL, "wss://") {
			url = url + ":443"
		} else {
			url = url + ":80"
		}
	}

	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", url)
	elapsed := time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("unreachable: %v", err), elapsed
	}
	conn.Close()
	return true, "", elapsed
}

func (m *Monitor) CheckEndpoint(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	switch strings.ToLower(ep.Type) {
	case "http", "https", "":
		return m.CheckHTTP(ctx, ep)
	case "jsonrpc", "json-rpc":
		return m.CheckJSONRPC(ctx, ep)
	case "tendermint", "cosmos":
		return m.CheckTendermint(ctx, ep)
	case "grpc":
		return m.CheckGRPC(ctx, ep)
	case "port", "tcp":
		return m.CheckPort(ctx, ep)
	case "websocket", "ws", "wss":
		return m.CheckWebSocket(ctx, ep)
	default:
		// Default to HTTP for any URL
		return m.CheckHTTP(ctx, ep)
	}
}

func (m *Monitor) sendTelegram(chatID interface{}, message string) error {
	if m.config.Telegram.BotToken == "" {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id": chatID, "text": message, "parse_mode": "HTML",
	})
	resp, err := client.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.config.Telegram.BotToken),
		"application/json", bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (m *Monitor) SendAlert(message string) {
	if m.config.Telegram.ChatID == "" {
		log.Printf("Alert: %s", message)
		return
	}
	if err := m.sendTelegram(m.config.Telegram.ChatID, message); err != nil {
		log.Printf("Failed to send alert: %v", err)
	}
}

func (m *Monitor) getDisplayURL(ep Endpoint) string {
	if ep.DisplayURL != "" {
		return ep.DisplayURL
	}
	return ep.URL
}

func (m *Monitor) GenerateStatusMessage() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	upCount := 0
	for _, s := range m.statuses {
		if s.IsUp {
			upCount++
		}
	}

	var msg string
	if upCount == len(m.statuses) {
		msg = fmt.Sprintf("✅ <b>All Systems Operational</b>\n\n%d/%d endpoints UP\n\n", upCount, len(m.statuses))
	} else {
		msg = fmt.Sprintf("⚠️ <b>Service Degradation</b>\n\n%d/%d endpoints UP\n\n", upCount, len(m.statuses))
	}

	for name, status := range m.statuses {
		if status.IsUp {
			msg += fmt.Sprintf("✅ <b>%s</b> (%v)\n   %s\n\n", name, status.ResponseTime.Round(time.Millisecond), m.getDisplayURL(status.Endpoint))
		} else {
			downTime := ""
			if !status.DownSince.IsZero() {
				downTime = fmt.Sprintf("\n   Down: %v", time.Since(status.DownSince).Round(time.Second))
			}
			msg += fmt.Sprintf("🔴 <b>%s</b>\n   %s\n   %s%s\n\n", name, m.getDisplayURL(status.Endpoint), status.LastError, downTime)
		}
	}
	msg += fmt.Sprintf("Last check: %s", time.Now().Format("15:04:05 MST"))
	return msg
}

func (m *Monitor) PollTelegramCommands(ctx context.Context) {
	if m.config.Telegram.BotToken == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var lastUpdateID int64
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := client.Get(fmt.Sprintf(
				"https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=1",
				m.config.Telegram.BotToken, lastUpdateID+1,
			))
			if err != nil {
				continue
			}
			var result struct {
				Result []struct {
					UpdateID int64 `json:"update_id"`
					Message  *struct {
						Text string
						Chat struct{ ID int64 }
					} `json:"message"`
					ChannelPost *struct {
						Text string
						Chat struct{ ID int64 }
					} `json:"channel_post"`
				} `json:"result"`
			}
			json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()

			for _, u := range result.Result {
				lastUpdateID = u.UpdateID
				var text string
				var chatID int64
				if u.Message != nil {
					text, chatID = u.Message.Text, u.Message.Chat.ID
				} else if u.ChannelPost != nil {
					text, chatID = u.ChannelPost.Text, u.ChannelPost.Chat.ID
				}
				if text == "/status" {
					m.sendTelegram(chatID, m.GenerateStatusMessage())
					log.Printf("Responded to /status from %d", chatID)
				}
			}
		}
	}
}

func (m *Monitor) Run(ctx context.Context) {
	for _, ep := range m.config.Endpoints {
		m.statuses[ep.Name] = &EndpointStatus{Endpoint: ep, IsUp: true}
	}

	m.checkAll(ctx)
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

func (m *Monitor) checkAll(ctx context.Context) {
	var wg sync.WaitGroup

	for _, ep := range m.config.Endpoints {
		wg.Add(1)
		go func(ep Endpoint) {
			defer wg.Done()

			timeout := m.defaultTimeout + 5*time.Second
			if ep.Timeout != "" {
				if t, err := time.ParseDuration(ep.Timeout); err == nil {
					timeout = t + 5*time.Second
				}
			}

			checkCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			isUp, errMsg, responseTime := m.CheckEndpoint(checkCtx, ep)
			displayURL := m.getDisplayURL(ep)

			m.mu.Lock()
			status := m.statuses[ep.Name]
			wasUp := status.IsUp
			status.LastCheck = time.Now()
			status.ResponseTime = responseTime

			if isUp {
				status.LastError = ""
				if m.slowThreshold > 0 && responseTime > m.slowThreshold && (status.Consecutive == 0 || wasUp) {
					m.mu.Unlock()
					m.SendAlert(fmt.Sprintf("⚠️ <b>%s</b> is SLOW\n\nEndpoint: <code>%s</code>\nResponse: %v (threshold: %v)",
						ep.Name, displayURL, responseTime.Round(time.Millisecond), m.slowThreshold))
					m.mu.Lock()
				}
				if !wasUp {
					status.IsUp = true
					downDuration := ""
					if !status.DownSince.IsZero() {
						downDuration = fmt.Sprintf("\nDowntime: %v", time.Since(status.DownSince).Round(time.Second))
					}
					status.DownSince = time.Time{}
					status.Consecutive = 1
					m.mu.Unlock()
					m.SendAlert(fmt.Sprintf("✅ <b>%s</b> is UP\n\nEndpoint: <code>%s</code>\nResponse: %v%s",
						ep.Name, displayURL, responseTime.Round(time.Millisecond), downDuration))
				} else {
					status.Consecutive++
					m.mu.Unlock()
				}
				log.Printf("✓ %s UP (%v)", ep.Name, responseTime.Round(time.Millisecond))
			} else {
				status.LastError = errMsg
				if wasUp {
					status.Consecutive = 1
				} else {
					status.Consecutive++
				}
				if status.Consecutive == m.failureThreshold {
					status.IsUp = false
					status.DownSince = time.Now()
					m.mu.Unlock()
					m.SendAlert(fmt.Sprintf("🔴 <b>%s</b> is DOWN\n\nEndpoint: <code>%s</code>\nError: %s",
						ep.Name, displayURL, errMsg))
				} else {
					m.mu.Unlock()
					if status.Consecutive < m.failureThreshold {
						log.Printf("⚠ %s failed (%d/%d): %s", ep.Name, status.Consecutive, m.failureThreshold, errMsg)
						return
					}
				}
				log.Printf("✗ %s DOWN: %s", ep.Name, errMsg)
			}
		}(ep)
	}
	wg.Wait()
}

func (m *Monitor) HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		defer m.mu.RUnlock()

		allUp := true
		response := make(map[string]interface{})
		for name, status := range m.statuses {
			response[name] = map[string]interface{}{
				"up": status.IsUp, "response_time_ms": status.ResponseTime.Milliseconds(),
				"last_check": status.LastCheck, "last_error": status.LastError,
			}
			if !status.IsUp {
				allUp = false
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if !allUp {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(response)
	}
}

func main() {
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	monitor := NewMonitor(*config)

	port := config.Settings.Port
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", monitor.HealthHandler())
	mux.HandleFunc("/status", monitor.HealthHandler())
	server := &http.Server{Addr: ":" + port, Handler: mux}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down...")
		cancel()
		server.Shutdown(context.Background())
	}()

	go monitor.Run(ctx)
	go monitor.PollTelegramCommands(ctx)

	startupMsg := fmt.Sprintf("🚀 <b>Pulse Monitor Started</b>\n\n%d endpoints, interval: %s\n\n", len(config.Endpoints), monitor.checkInterval)
	for _, ep := range config.Endpoints {
		startupMsg += fmt.Sprintf("• %s (%s)\n", ep.Name, monitor.getDisplayURL(ep))
	}
	monitor.SendAlert(startupMsg)

	log.Printf("Server starting on :%s", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

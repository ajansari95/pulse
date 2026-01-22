package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"
	"gopkg.in/yaml.v3"
)

//go:embed static/* templates/*
var dashboardFS embed.FS

type Config struct {
	Telegram struct {
		BotToken string `yaml:"bot_token" json:"bot_token"`
		ChatID   string `yaml:"chat_id" json:"chat_id"`
	} `yaml:"telegram" json:"telegram"`
	Notifications struct {
		Slack struct {
			WebhookURL string `yaml:"webhook_url" json:"webhook_url"`
		} `yaml:"slack" json:"slack"`
		Discord struct {
			WebhookURL string `yaml:"webhook_url" json:"webhook_url"`
		} `yaml:"discord" json:"discord"`
		PagerDuty struct {
			RoutingKey string `yaml:"routing_key" json:"routing_key"`
		} `yaml:"pagerduty" json:"pagerduty"`
	} `yaml:"notifications" json:"notifications"`
	Settings struct {
		CheckInterval    string `yaml:"check_interval" json:"check_interval"`
		FailureThreshold int    `yaml:"failure_threshold" json:"failure_threshold"`
		SlowThreshold    string `yaml:"slow_threshold" json:"slow_threshold"`
		Port             string `yaml:"port" json:"port"`
		MetricsPort      string `yaml:"metrics_port" json:"metrics_port"`
		Timeout          string `yaml:"timeout" json:"timeout"`
		DailySummary     string `yaml:"daily_summary" json:"daily_summary"`
		WeeklySummary    string `yaml:"weekly_summary" json:"weekly_summary"`
		PublicDashboard  *bool  `yaml:"public_dashboard" json:"public_dashboard"`
		Region           string `yaml:"region" json:"region"`
		CoordinatorURL   string `yaml:"coordinator_url" json:"coordinator_url"`
		RegionsRequired  int    `yaml:"regions_required" json:"regions_required"`
		RegionAlerts     bool   `yaml:"region_alerts" json:"region_alerts"`
		ReportStaleAfter string `yaml:"report_stale_after" json:"report_stale_after"`
	} `yaml:"settings" json:"settings"`
	Endpoints   []Endpoint          `yaml:"endpoints" json:"endpoints"`
	Maintenance []MaintenanceWindow `yaml:"maintenance" json:"maintenance"`
}

type MaintenanceWindow struct {
	Name      string   `yaml:"name" json:"name"`
	Schedule  string   `yaml:"schedule" json:"schedule"`
	Endpoints []string `yaml:"endpoints" json:"endpoints"`
}

type Endpoint struct {
	Name       string `yaml:"name" json:"name"`
	URL        string `yaml:"url" json:"url"`
	DisplayURL string `yaml:"display_url,omitempty" json:"display_url,omitempty"`
	Type       string `yaml:"type" json:"type"` // http, jsonrpc, tendermint, grpc, port, tcp, websocket, ssl

	// SSL options
	WarnDays     int    `yaml:"warn_days,omitempty" json:"warn_days,omitempty"`
	CriticalDays int    `yaml:"critical_days,omitempty" json:"critical_days,omitempty"`
	CACertFile   string `yaml:"ca_cert_file,omitempty" json:"ca_cert_file,omitempty"`

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
	Endpoint       Endpoint
	IsUp           bool
	LastCheck      time.Time
	LastError      string
	ResponseTime   time.Duration
	Consecutive    int
	DownSince      time.Time
	ConsecFailures int
}

type EndpointMetrics struct {
	TotalChecks        int
	UpChecks           int
	TotalResponseTime  time.Duration
	ResponseSamples    int
	Incidents          int
	LongestDowntime    time.Duration
	ActiveDowntimeFrom time.Time
}

type HistoryPoint struct {
	Timestamp      time.Time `json:"timestamp"`
	ResponseTimeMs int64     `json:"response_time_ms"`
	Up             bool      `json:"up"`
}

type IncidentEvent struct {
	Endpoint string     `json:"endpoint"`
	Start    time.Time  `json:"start"`
	End      *time.Time `json:"end,omitempty"`
	Error    string     `json:"error"`
}

type RegionStatus struct {
	Up             bool      `json:"up"`
	LastCheck      time.Time `json:"last_check"`
	LastError      string    `json:"last_error"`
	ResponseTimeMs int64     `json:"response_time_ms"`
}

type RegionReport struct {
	Region   string                  `json:"region"`
	Statuses map[string]RegionStatus `json:"statuses"`
}

type Monitor struct {
	config           Config
	configPath       string
	checkInterval    time.Duration
	failureThreshold int
	slowThreshold    time.Duration
	defaultTimeout   time.Duration
	statuses         map[string]*EndpointStatus
	dailyMetrics     map[string]*EndpointMetrics
	weeklyMetrics    map[string]*EndpointMetrics
	history          map[string][]HistoryPoint
	incidents        []IncidentEvent
	activeIncidents  map[string]int
	historyLimit     int
	totalChecks      map[string]int
	totalFailures    map[string]int
	sslExpiry        map[string]time.Time
	sslLastWarn      map[string]time.Time
	mutes            map[string]time.Time
	acknowledged     map[string]time.Time
	maintenanceUntil map[string]time.Time
	region           string
	coordinatorURL   string
	regionsRequired  int
	regionAlerts     bool
	regionReports    map[string]map[string]RegionStatus
	aggregateStatus  map[string]bool
	regionLastReport map[string]time.Time
	reportStaleAfter time.Duration
	configHash       uint64
	alertClient      *http.Client
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

func LoadConfigWithHash(path string) (*Config, uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	expanded := os.ExpandEnv(string(data))
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, 0, err
	}
	cfg.applyEnvOverrides()
	return &cfg, fnv64a([]byte(expanded)), nil
}

func fnv64a(data []byte) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	var hash uint64 = offset64
	for _, value := range data {
		hash ^= uint64(value)
		hash *= prime64
	}
	return hash
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
	if v := os.Getenv("METRICS_PORT"); v != "" {
		c.Settings.MetricsPort = v
	}
	if v := os.Getenv("REGION"); v != "" {
		c.Settings.Region = v
	}
	if v := os.Getenv("COORDINATOR_URL"); v != "" {
		c.Settings.CoordinatorURL = v
	}
	if v := os.Getenv("REGIONS_REQUIRED"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.Settings.RegionsRequired = parsed
		}
	}
	if v := os.Getenv("REGION_ALERTS"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.Settings.RegionAlerts = parsed
		}
	}
	if v := os.Getenv("REPORT_STALE_AFTER"); v != "" {
		c.Settings.ReportStaleAfter = v
	}
	if v := os.Getenv("PUBLIC_DASHBOARD"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.Settings.PublicDashboard = &parsed
		}
	}
}

func NewMonitor(config Config, configPath string, configHash uint64) *Monitor {
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

	region := strings.TrimSpace(config.Settings.Region)
	if region == "" {
		region = "default"
	}
	regionsRequired := config.Settings.RegionsRequired
	if regionsRequired == 0 {
		regionsRequired = 2
	}
	reportStaleAfter := 2 * checkInterval
	if config.Settings.ReportStaleAfter != "" {
		if parsed, err := time.ParseDuration(config.Settings.ReportStaleAfter); err == nil {
			reportStaleAfter = parsed
		}
	}
	if reportStaleAfter <= 0 {
		reportStaleAfter = 2 * checkInterval
	}

	return &Monitor{
		config:           config,
		configPath:       configPath,
		checkInterval:    checkInterval,
		failureThreshold: failureThreshold,
		slowThreshold:    slowThreshold,
		defaultTimeout:   defaultTimeout,
		statuses:         make(map[string]*EndpointStatus),
		dailyMetrics:     make(map[string]*EndpointMetrics),
		weeklyMetrics:    make(map[string]*EndpointMetrics),
		history:          make(map[string][]HistoryPoint),
		incidents:        []IncidentEvent{},
		activeIncidents:  make(map[string]int),
		historyLimit:     500,
		totalChecks:      make(map[string]int),
		totalFailures:    make(map[string]int),
		sslExpiry:        make(map[string]time.Time),
		sslLastWarn:      make(map[string]time.Time),
		mutes:            make(map[string]time.Time),
		acknowledged:     make(map[string]time.Time),
		maintenanceUntil: make(map[string]time.Time),
		region:           region,
		coordinatorURL:   strings.TrimSpace(config.Settings.CoordinatorURL),
		regionsRequired:  regionsRequired,
		regionAlerts:     config.Settings.RegionAlerts,
		regionReports:    make(map[string]map[string]RegionStatus),
		aggregateStatus:  make(map[string]bool),
		regionLastReport: make(map[string]time.Time),
		reportStaleAfter: reportStaleAfter,
		alertClient:      &http.Client{Timeout: 10 * time.Second},
		configHash:       configHash,
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

func daysUntilExpiry(now time.Time, expiry time.Time) int {
	return int(expiry.Sub(now).Hours() / 24)
}

func (m *Monitor) CheckSSL(ctx context.Context, ep Endpoint) (bool, string, time.Duration) {
	start := time.Now()

	urlValue := strings.TrimSpace(ep.URL)
	if urlValue == "" {
		return false, "missing url", 0
	}
	if !strings.Contains(urlValue, "://") {
		urlValue = "https://" + urlValue
	}
	parsed, err := url.Parse(urlValue)
	if err != nil {
		return false, fmt.Sprintf("invalid url: %v", err), 0
	}
	host := parsed.Host
	if host == "" {
		host = parsed.Path
	}
	if host == "" {
		return false, "missing host", 0
	}

	hostname := host
	if strings.Contains(host, ":") {
		if splitHost, _, err := net.SplitHostPort(host); err == nil {
			hostname = splitHost
		}
	} else {
		host = host + ":443"
	}

	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if ep.CACertFile != "" {
		pemData, err := os.ReadFile(ep.CACertFile)
		if err != nil {
			return false, fmt.Sprintf("failed to read CA cert: %v", err), 0
		}
		if !rootCAs.AppendCertsFromPEM(pemData) {
			return false, "invalid CA cert bundle", 0
		}
	}

	tlsConfig := &tls.Config{RootCAs: rootCAs}
	if net.ParseIP(hostname) == nil {
		tlsConfig.ServerName = hostname
	}

	timeout := m.defaultTimeout
	if ep.Timeout != "" {
		if t, err := time.ParseDuration(ep.Timeout); err == nil {
			timeout = t
		}
	}

	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", host)
	if err != nil {
		return false, fmt.Sprintf("dial failed: %v", err), time.Since(start)
	}
	defer conn.Close()

	tlsConn := tls.Client(conn, tlsConfig)
	defer tlsConn.Close()
	_ = tlsConn.SetDeadline(time.Now().Add(timeout))
	if err := tlsConn.Handshake(); err != nil {
		return false, fmt.Sprintf("tls handshake failed: %v", err), time.Since(start)
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return false, "no peer certificates", time.Since(start)
	}

	expiry := state.PeerCertificates[0].NotAfter
	now := time.Now()
	daysLeft := daysUntilExpiry(now, expiry)

	m.mu.Lock()
	m.sslExpiry[ep.Name] = expiry
	m.mu.Unlock()

	criticalDays := ep.CriticalDays
	if criticalDays == 0 {
		criticalDays = 7
	}

	if expiry.Before(now) {
		return false, fmt.Sprintf("certificate expired %d days ago", -daysLeft), time.Since(start)
	}
	if daysLeft <= criticalDays {
		return false, fmt.Sprintf("certificate expires in %d days", daysLeft), time.Since(start)
	}
	return true, "", time.Since(start)
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
	case "ssl":
		return m.CheckSSL(ctx, ep)
	default:
		// Default to HTTP for any URL
		return m.CheckHTTP(ctx, ep)
	}
}

func (m *Monitor) sendTelegram(chatID interface{}, message string) error {
	if m.config.Telegram.BotToken == "" {
		return nil
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id": chatID, "text": message, "parse_mode": "HTML",
	})
	resp, err := m.alertClient.Post(
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
	} else {
		if err := m.sendTelegram(m.config.Telegram.ChatID, message); err != nil {
			log.Printf("Failed to send Telegram alert: %v", err)
		}
	}

	details := parseAlertDetails(message)

	if err := m.sendSlackAlert(details.PlainMessage); err != nil {
		log.Printf("Failed to send Slack alert: %v", err)
	}
	if err := m.sendDiscordAlert(details.PlainMessage); err != nil {
		log.Printf("Failed to send Discord alert: %v", err)
	}
	if err := m.sendPagerDutyAlert(details); err != nil {
		log.Printf("Failed to send PagerDuty alert: %v", err)
	}
}

type AlertDetails struct {
	Kind         string
	EndpointName string
	PlainMessage string
}

var htmlTagPattern = regexp.MustCompile("<[^>]+>")
var boldTagPattern = regexp.MustCompile(`<b>([^<]+)</b>`)

func parseAlertDetails(message string) AlertDetails {
	plain := htmlTagPattern.ReplaceAllString(message, "")
	kind := "info"

	if strings.Contains(message, "Summary") {
		kind = "summary"
	} else if strings.Contains(message, "is DOWN") {
		kind = "down"
	} else if strings.Contains(message, "is UP") {
		kind = "up"
	} else if strings.Contains(message, "is SLOW") {
		kind = "slow"
	}

	endpoint := ""
	if matches := boldTagPattern.FindStringSubmatch(message); len(matches) > 1 {
		endpoint = matches[1]
	}

	return AlertDetails{Kind: kind, EndpointName: endpoint, PlainMessage: plain}
}

func (m *Monitor) sendSlackAlert(message string) error {
	webhookURL := strings.TrimSpace(m.config.Notifications.Slack.WebhookURL)
	if webhookURL == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"text": message})
	if err != nil {
		return err
	}
	resp, err := m.alertClient.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func (m *Monitor) sendDiscordAlert(message string) error {
	webhookURL := strings.TrimSpace(m.config.Notifications.Discord.WebhookURL)
	if webhookURL == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"content": message})
	if err != nil {
		return err
	}
	resp, err := m.alertClient.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func (m *Monitor) sendPagerDutyAlert(details AlertDetails) error {
	routingKey := strings.TrimSpace(m.config.Notifications.PagerDuty.RoutingKey)
	if routingKey == "" {
		return nil
	}
	if details.Kind == "summary" || details.Kind == "info" || details.Kind == "slow" {
		return nil
	}

	eventAction := "trigger"
	severity := "error"
	dedupKind := details.Kind
	if details.Kind == "down" {
		severity = "critical"
	} else if details.Kind == "slow" {
		severity = "warning"
	} else if details.Kind == "up" {
		eventAction = "resolve"
		severity = "info"
		dedupKind = "down"
	}

	endpointKey := strings.TrimSpace(details.EndpointName)
	if endpointKey == "" {
		endpointKey = "unknown"
	}
	endpointKey = strings.ReplaceAll(strings.ToLower(endpointKey), " ", "-")
	dedupKey := fmt.Sprintf("pulse-%s-%s", endpointKey, dedupKind)

	payload := map[string]interface{}{
		"routing_key":  routingKey,
		"event_action": eventAction,
		"dedup_key":    dedupKey,
	}
	if eventAction == "trigger" {
		payload["payload"] = map[string]interface{}{
			"summary":  details.PlainMessage,
			"source":   "pulse",
			"severity": severity,
			"custom_details": map[string]interface{}{
				"endpoint": details.EndpointName,
				"kind":     details.Kind,
			},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := m.alertClient.Post("https://events.pagerduty.com/v2/enqueue", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("pagerduty returned status %d", resp.StatusCode)
	}
	return nil
}

func (m *Monitor) ensureMetrics(ep Endpoint) {
	if _, ok := m.dailyMetrics[ep.Name]; !ok {
		m.dailyMetrics[ep.Name] = &EndpointMetrics{}
	}
	if _, ok := m.weeklyMetrics[ep.Name]; !ok {
		m.weeklyMetrics[ep.Name] = &EndpointMetrics{}
	}
	if _, ok := m.history[ep.Name]; !ok {
		m.history[ep.Name] = []HistoryPoint{}
	}
	if _, ok := m.totalChecks[ep.Name]; !ok {
		m.totalChecks[ep.Name] = 0
	}
	if _, ok := m.totalFailures[ep.Name]; !ok {
		m.totalFailures[ep.Name] = 0
	}
	if _, ok := m.sslExpiry[ep.Name]; !ok {
		m.sslExpiry[ep.Name] = time.Time{}
	}
	if _, ok := m.sslLastWarn[ep.Name]; !ok {
		m.sslLastWarn[ep.Name] = time.Time{}
	}
}

func (m *Monitor) recordCheckMetrics(metrics *EndpointMetrics, isUp bool, responseTime time.Duration) {
	metrics.TotalChecks++
	if isUp {
		metrics.UpChecks++
		metrics.TotalResponseTime += responseTime
		metrics.ResponseSamples++
	}
}

func (m *Monitor) recordIncidentStart(metrics *EndpointMetrics, start time.Time) {
	metrics.Incidents++
	metrics.ActiveDowntimeFrom = start
}

func (m *Monitor) recordIncidentEnd(metrics *EndpointMetrics, end time.Time) {
	if metrics.ActiveDowntimeFrom.IsZero() {
		return
	}
	downtime := end.Sub(metrics.ActiveDowntimeFrom)
	if downtime > metrics.LongestDowntime {
		metrics.LongestDowntime = downtime
	}
	metrics.ActiveDowntimeFrom = time.Time{}
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

func formatDuration(duration time.Duration) string {
	if duration <= 0 {
		return "0s"
	}
	if duration < time.Second {
		return duration.Round(time.Millisecond).String()
	}
	return duration.Round(time.Second).String()
}

func (m *Monitor) buildSummaryMessage(label string, metrics map[string]*EndpointMetrics) string {
	if len(m.statuses) == 0 {
		return ""
	}

	if len(m.statuses) == 0 {
		return ""
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "📊 <b>%s Summary</b>\n\n", label)
	for _, ep := range m.config.Endpoints {
		status := m.statuses[ep.Name]
		metric := metrics[ep.Name]
		if metric == nil {
			metric = &EndpointMetrics{}
		}

		uptime := 0.0
		if metric.TotalChecks > 0 {
			uptime = (float64(metric.UpChecks) / float64(metric.TotalChecks)) * 100
		}

		avgResponse := "n/a"
		if metric.ResponseSamples > 0 {
			avg := metric.TotalResponseTime / time.Duration(metric.ResponseSamples)
			avgResponse = avg.Round(time.Millisecond).String()
		}

		longestDowntime := metric.LongestDowntime
		if !metric.ActiveDowntimeFrom.IsZero() {
			current := time.Since(metric.ActiveDowntimeFrom)
			if current > longestDowntime {
				longestDowntime = current
			}
		}
		if status != nil && !status.DownSince.IsZero() && metric.ActiveDowntimeFrom.IsZero() {
			current := time.Since(status.DownSince)
			if current > longestDowntime {
				longestDowntime = current
			}
		}

		icon := "✅"
		if status != nil && !status.IsUp {
			icon = "🔴"
		}

		fmt.Fprintf(&builder, "%s <b>%s</b>\n", icon, ep.Name)
		fmt.Fprintf(&builder, "Uptime: <code>%.2f%%</code>\n", uptime)
		fmt.Fprintf(&builder, "Avg response: <code>%s</code>\n", avgResponse)
		fmt.Fprintf(&builder, "Incidents: <code>%d</code>\n", metric.Incidents)
		fmt.Fprintf(&builder, "Longest downtime: <code>%s</code>\n\n", formatDuration(longestDowntime))
	}

	fmt.Fprintf(&builder, "Report generated: %s", time.Now().Format("2006-01-02 15:04 MST"))
	return builder.String()
}

func (m *Monitor) resetMetrics(metrics map[string]*EndpointMetrics) {
	for name, metric := range metrics {
		activeDowntime := metric.ActiveDowntimeFrom
		*metric = EndpointMetrics{}
		status := m.statuses[name]
		if status != nil && !status.IsUp {
			metric.ActiveDowntimeFrom = activeDowntime
			if !activeDowntime.IsZero() {
				metric.Incidents = 1
			}
		}
	}
}

func (m *Monitor) recordHistory(ep Endpoint, now time.Time, responseTime time.Duration, isUp bool) {
	points := m.history[ep.Name]
	points = append(points, HistoryPoint{
		Timestamp:      now,
		ResponseTimeMs: responseTime.Milliseconds(),
		Up:             isUp,
	})
	if len(points) > m.historyLimit {
		points = points[len(points)-m.historyLimit:]
	}
	m.history[ep.Name] = points
}

func (m *Monitor) startIncident(ep Endpoint, start time.Time, errMsg string) {
	if _, exists := m.activeIncidents[ep.Name]; exists {
		return
	}
	incident := IncidentEvent{
		Endpoint: ep.Name,
		Start:    start,
		Error:    errMsg,
	}
	m.incidents = append(m.incidents, incident)
	m.activeIncidents[ep.Name] = len(m.incidents) - 1
}

func (m *Monitor) resolveIncident(ep Endpoint, end time.Time) {
	index, ok := m.activeIncidents[ep.Name]
	if !ok || index < 0 || index >= len(m.incidents) {
		return
	}
	m.incidents[index].End = &end
	delete(m.activeIncidents, ep.Name)
}

type DashboardSummary struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Endpoints   []EndpointReport `json:"endpoints"`
}

type EndpointReport struct {
	Name              string    `json:"name"`
	DisplayURL        string    `json:"display_url"`
	Up                bool      `json:"up"`
	ResponseTimeMs    int64     `json:"response_time_ms"`
	LastCheck         time.Time `json:"last_check"`
	LastError         string    `json:"last_error"`
	UptimePercent     float64   `json:"uptime_percent"`
	AvgResponseTimeMs int64     `json:"avg_response_time_ms"`
	Incidents         int       `json:"incidents"`
	LongestDowntimeMs int64     `json:"longest_downtime_ms"`
}

type DashboardHistory struct {
	GeneratedAt time.Time                 `json:"generated_at"`
	History     map[string][]HistoryPoint `json:"history"`
	Incidents   []IncidentEvent           `json:"incidents"`
}

func responseTimeMs(status *EndpointStatus) int64 {
	if status == nil {
		return 0
	}
	return status.ResponseTime.Milliseconds()
}

func (m *Monitor) DashboardSummaryHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		defer m.mu.RUnlock()

		reports := make([]EndpointReport, 0, len(m.config.Endpoints))
		for _, ep := range m.config.Endpoints {
			status := m.statuses[ep.Name]
			metrics := m.dailyMetrics[ep.Name]
			if metrics == nil {
				metrics = &EndpointMetrics{}
			}
			uptime := 0.0
			if metrics.TotalChecks > 0 {
				uptime = (float64(metrics.UpChecks) / float64(metrics.TotalChecks)) * 100
			}
			avgResponse := int64(0)
			if metrics.ResponseSamples > 0 {
				avgResponse = (metrics.TotalResponseTime / time.Duration(metrics.ResponseSamples)).Milliseconds()
			}
			longest := metrics.LongestDowntime.Milliseconds()
			lastCheck := time.Time{}
			lastError := ""
			if status != nil {
				lastCheck = status.LastCheck
				lastError = status.LastError
			}
			reports = append(reports, EndpointReport{
				Name:              ep.Name,
				DisplayURL:        m.getDisplayURL(ep),
				Up:                status != nil && status.IsUp,
				ResponseTimeMs:    responseTimeMs(status),
				LastCheck:         lastCheck,
				LastError:         lastError,
				UptimePercent:     uptime,
				AvgResponseTimeMs: avgResponse,
				Incidents:         metrics.Incidents,
				LongestDowntimeMs: longest,
			})
		}

		payload := DashboardSummary{
			GeneratedAt: time.Now(),
			Endpoints:   reports,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}
}

func (m *Monitor) DashboardHistoryHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		defer m.mu.RUnlock()

		historyCopy := make(map[string][]HistoryPoint, len(m.history))
		for name, points := range m.history {
			copyPoints := make([]HistoryPoint, len(points))
			copy(copyPoints, points)
			historyCopy[name] = copyPoints
		}
		incidentsCopy := make([]IncidentEvent, len(m.incidents))
		copy(incidentsCopy, m.incidents)

		payload := DashboardHistory{
			GeneratedAt: time.Now(),
			History:     historyCopy,
			Incidents:   incidentsCopy,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}
}

func parseClock(value string) (int, int, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, 0, false
	}
	parsed, err := time.Parse("15:04", trimmed)
	if err != nil {
		return 0, 0, false
	}
	return parsed.Hour(), parsed.Minute(), true
}

func parseWeeklySchedule(value string) (time.Weekday, int, int, bool) {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	if len(parts) != 2 {
		return time.Sunday, 0, 0, false
	}
	weekday, ok := map[string]time.Weekday{
		"sunday":    time.Sunday,
		"monday":    time.Monday,
		"tuesday":   time.Tuesday,
		"wednesday": time.Wednesday,
		"thursday":  time.Thursday,
		"friday":    time.Friday,
		"saturday":  time.Saturday,
	}[parts[0]]
	if !ok {
		return time.Sunday, 0, 0, false
	}
	hour, minute, ok := parseClock(parts[1])
	if !ok {
		return time.Sunday, 0, 0, false
	}
	return weekday, hour, minute, true
}

func parseMaintenanceSchedule(value string) (time.Weekday, int, int, int, int, bool) {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	if len(parts) != 2 {
		return time.Sunday, 0, 0, 0, 0, false
	}
	weekday, ok := map[string]time.Weekday{
		"sunday":    time.Sunday,
		"monday":    time.Monday,
		"tuesday":   time.Tuesday,
		"wednesday": time.Wednesday,
		"thursday":  time.Thursday,
		"friday":    time.Friday,
		"saturday":  time.Saturday,
	}[parts[0]]
	if !ok {
		return time.Sunday, 0, 0, 0, 0, false
	}
	rangeParts := strings.Split(parts[1], "-")
	if len(rangeParts) != 2 {
		return time.Sunday, 0, 0, 0, 0, false
	}
	startHour, startMinute, ok := parseClock(rangeParts[0])
	if !ok {
		return time.Sunday, 0, 0, 0, 0, false
	}
	endHour, endMinute, ok := parseClock(rangeParts[1])
	if !ok {
		return time.Sunday, 0, 0, 0, 0, false
	}
	return weekday, startHour, startMinute, endHour, endMinute, true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func nextDailyRun(now time.Time, hour, minute int) time.Time {
	start := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !start.After(now) {
		start = start.Add(24 * time.Hour)
	}
	return start
}

func nextWeeklyRun(now time.Time, weekday time.Weekday, hour, minute int) time.Time {
	start := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	daysUntil := (int(weekday) - int(now.Weekday()) + 7) % 7
	if daysUntil == 0 && !start.After(now) {
		daysUntil = 7
	}
	if daysUntil > 0 {
		start = start.AddDate(0, 0, daysUntil)
	}
	return start
}

func isWithinWeeklyWindow(now time.Time, weekday time.Weekday, startHour, startMinute, endHour, endMinute int) bool {
	start := time.Date(now.Year(), now.Month(), now.Day(), startHour, startMinute, 0, 0, now.Location())
	delta := (int(weekday) - int(now.Weekday()) + 7) % 7
	start = start.AddDate(0, 0, delta)
	if start.After(now) {
		start = start.AddDate(0, 0, -7)
	}
	end := time.Date(start.Year(), start.Month(), start.Day(), endHour, endMinute, 0, 0, start.Location())
	if !end.After(start) {
		end = end.Add(24 * time.Hour)
	}
	return (now.Equal(start) || now.After(start)) && now.Before(end)
}

func (m *Monitor) isMutedLocked(endpoint string, now time.Time) bool {
	until, ok := m.mutes[endpoint]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(m.mutes, endpoint)
		return false
	}
	return true
}

func (m *Monitor) isInMaintenanceLocked(endpoint string, now time.Time) bool {
	until, ok := m.maintenanceUntil[endpoint]
	if ok {
		if now.After(until) {
			delete(m.maintenanceUntil, endpoint)
		} else {
			return true
		}
	}
	for _, window := range m.config.Maintenance {
		if len(window.Endpoints) > 0 && !containsString(window.Endpoints, endpoint) {
			continue
		}
		weekday, startHour, startMinute, endHour, endMinute, ok := parseMaintenanceSchedule(window.Schedule)
		if !ok {
			continue
		}
		if isWithinWeeklyWindow(now, weekday, startHour, startMinute, endHour, endMinute) {
			return true
		}
	}
	return false
}

func (m *Monitor) isAcknowledgedLocked(endpoint string) bool {
	_, ok := m.acknowledged[endpoint]
	return ok
}

func (m *Monitor) setMute(endpoint string, until time.Time) {
	m.mu.Lock()
	m.mutes[endpoint] = until
	m.mu.Unlock()
}

func (m *Monitor) setAcknowledged(endpoint string, at time.Time) {
	m.mu.Lock()
	m.acknowledged[endpoint] = at
	m.mu.Unlock()
}

func (m *Monitor) setMaintenance(endpoint string, until time.Time) {
	m.mu.Lock()
	m.maintenanceUntil[endpoint] = until
	m.mu.Unlock()
}

func (m *Monitor) endpointExists(name string) bool {
	for _, ep := range m.config.Endpoints {
		if ep.Name == name {
			return true
		}
	}
	return false
}

func (m *Monitor) runDailySummary(ctx context.Context, hour, minute int) {
	for {
		nextRun := nextDailyRun(time.Now(), hour, minute)
		timer := time.NewTimer(time.Until(nextRun))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m.SendDailySummary()
		}
	}
}

func (m *Monitor) runWeeklySummary(ctx context.Context, weekday time.Weekday, hour, minute int) {
	for {
		nextRun := nextWeeklyRun(time.Now(), weekday, hour, minute)
		timer := time.NewTimer(time.Until(nextRun))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m.SendWeeklySummary()
		}
	}
}

func (m *Monitor) ScheduleSummaryReports(ctx context.Context) {
	dailySetting := strings.TrimSpace(m.config.Settings.DailySummary)
	if dailySetting != "" {
		if hour, minute, ok := parseClock(dailySetting); ok {
			go m.runDailySummary(ctx, hour, minute)
		} else {
			log.Printf("Invalid daily_summary format: %s (expected HH:MM)", dailySetting)
		}
	}

	weeklySetting := strings.TrimSpace(m.config.Settings.WeeklySummary)
	if weeklySetting != "" {
		if weekday, hour, minute, ok := parseWeeklySchedule(weeklySetting); ok {
			go m.runWeeklySummary(ctx, weekday, hour, minute)
		} else {
			log.Printf("Invalid weekly_summary format: %s (expected 'monday HH:MM')", weeklySetting)
		}
	}
}

func (m *Monitor) SendDailySummary() {
	m.mu.Lock()
	message := m.buildSummaryMessage("Daily", m.dailyMetrics)
	if message != "" {
		m.resetMetrics(m.dailyMetrics)
	}
	m.mu.Unlock()
	if message != "" {
		m.SendAlert(message)
	}
}

func (m *Monitor) SendWeeklySummary() {
	m.mu.Lock()
	message := m.buildSummaryMessage("Weekly", m.weeklyMetrics)
	if message != "" {
		m.resetMetrics(m.weeklyMetrics)
	}
	m.mu.Unlock()
	if message != "" {
		m.SendAlert(message)
	}
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
				text = strings.TrimSpace(text)
				switch {
				case text == "/status":
					m.sendTelegram(chatID, m.GenerateStatusMessage())
					log.Printf("Responded to /status from %d", chatID)
				case text == "/reload":
					if err := m.ReloadConfig("telegram"); err != nil {
						m.sendTelegram(chatID, fmt.Sprintf("Reload failed: %v", err))
						continue
					}
					m.sendTelegram(chatID, "Config reloaded")
				case strings.HasPrefix(text, "/mute "):
					parts := strings.Fields(text)
					if len(parts) < 3 {
						m.sendTelegram(chatID, "Usage: /mute <endpoint> <duration>")
						continue
					}
					if !m.endpointExists(parts[1]) {
						m.sendTelegram(chatID, fmt.Sprintf("Unknown endpoint: %s", parts[1]))
						continue
					}
					duration, err := time.ParseDuration(parts[2])
					if err != nil {
						m.sendTelegram(chatID, "Invalid duration. Example: /mute API 30m")
						continue
					}
					until := time.Now().Add(duration)
					m.setMute(parts[1], until)
					m.sendTelegram(chatID, fmt.Sprintf("Muted %s until %s", parts[1], until.Format("15:04 MST")))
				case strings.HasPrefix(text, "/ack "):
					parts := strings.Fields(text)
					if len(parts) < 2 {
						m.sendTelegram(chatID, "Usage: /ack <endpoint>")
						continue
					}
					if !m.endpointExists(parts[1]) {
						m.sendTelegram(chatID, fmt.Sprintf("Unknown endpoint: %s", parts[1]))
						continue
					}
					m.setAcknowledged(parts[1], time.Now())
					m.sendTelegram(chatID, fmt.Sprintf("Acknowledged %s", parts[1]))
				case strings.HasPrefix(text, "/maintenance start "):
					parts := strings.Fields(text)
					if len(parts) < 4 {
						m.sendTelegram(chatID, "Usage: /maintenance start <endpoint> <duration>")
						continue
					}
					if !m.endpointExists(parts[2]) {
						m.sendTelegram(chatID, fmt.Sprintf("Unknown endpoint: %s", parts[2]))
						continue
					}
					duration, err := time.ParseDuration(parts[3])
					if err != nil {
						m.sendTelegram(chatID, "Invalid duration. Example: /maintenance start API 2h")
						continue
					}
					until := time.Now().Add(duration)
					m.setMaintenance(parts[2], until)
					m.sendTelegram(chatID, fmt.Sprintf("Maintenance started for %s until %s", parts[2], until.Format("15:04 MST")))
				}
			}
		}
	}
}

func (m *Monitor) Run(ctx context.Context) {
	for _, ep := range m.config.Endpoints {
		m.statuses[ep.Name] = &EndpointStatus{Endpoint: ep, IsUp: true}
		m.ensureMetrics(ep)
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
			now := time.Now()
			sendLocalAlerts := m.coordinatorURL == ""

			m.mu.Lock()
			status := m.statuses[ep.Name]
			wasUp := status.IsUp
			status.LastCheck = now
			status.ResponseTime = responseTime

			dailyMetrics := m.dailyMetrics[ep.Name]
			weeklyMetrics := m.weeklyMetrics[ep.Name]
			if dailyMetrics == nil || weeklyMetrics == nil {
				m.ensureMetrics(ep)
				dailyMetrics = m.dailyMetrics[ep.Name]
				weeklyMetrics = m.weeklyMetrics[ep.Name]
			}
			m.recordCheckMetrics(dailyMetrics, isUp, responseTime)
			m.recordCheckMetrics(weeklyMetrics, isUp, responseTime)
			m.recordHistory(ep, now, responseTime, isUp)
			m.totalChecks[ep.Name]++
			if !isUp {
				m.totalFailures[ep.Name]++
			}
			sslWarnMsg := ""
			if strings.EqualFold(ep.Type, "ssl") && isUp {
				expiry := m.sslExpiry[ep.Name]
				if !expiry.IsZero() {
					warnDays := ep.WarnDays
					if warnDays == 0 {
						warnDays = 30
					}
					criticalDays := ep.CriticalDays
					if criticalDays == 0 {
						criticalDays = 7
					}
					daysLeft := daysUntilExpiry(now, expiry)
					if daysLeft > criticalDays && daysLeft <= warnDays {
						lastWarn := m.sslLastWarn[ep.Name]
						if lastWarn.IsZero() || now.Sub(lastWarn) >= 24*time.Hour {
							m.sslLastWarn[ep.Name] = now
							sslWarnMsg = fmt.Sprintf("⚠️ <b>%s</b> SSL certificate expires in %d days\n\nEndpoint: <code>%s</code>\nExpiry: %s",
								ep.Name, daysLeft, displayURL, expiry.Format("2006-01-02"))
						}
					}
				}
			}
			shouldAlert := !m.isMutedLocked(ep.Name, now) && !m.isInMaintenanceLocked(ep.Name, now)
			acknowledged := m.isAcknowledgedLocked(ep.Name)
			if isUp {
				status.LastError = ""
				status.ConsecFailures = 0
				sendSlowAlert := m.slowThreshold > 0 && responseTime > m.slowThreshold && (status.Consecutive == 0 || wasUp)
				if !wasUp {
					status.IsUp = true
					downDuration := time.Duration(0)
					if !status.DownSince.IsZero() {
						downDuration = now.Sub(status.DownSince)
					}
					if downDuration > 0 {
						m.recordIncidentEnd(dailyMetrics, now)
						m.recordIncidentEnd(weeklyMetrics, now)
					}
					m.resolveIncident(ep, now)
					status.DownSince = time.Time{}
					status.Consecutive = 1
					m.mu.Unlock()
					if sendLocalAlerts && shouldAlert && sendSlowAlert {
						m.SendAlert(fmt.Sprintf("⚠️ <b>%s</b> is SLOW\n\nEndpoint: <code>%s</code>\nResponse: %v (threshold: %v)",
							ep.Name, displayURL, responseTime.Round(time.Millisecond), m.slowThreshold))
					}
					downMsg := ""
					if downDuration > 0 {
						downMsg = fmt.Sprintf("\nDowntime: %v", downDuration.Round(time.Second))
					}
					if sendLocalAlerts && (shouldAlert || acknowledged) {
						m.SendAlert(fmt.Sprintf("✅ <b>%s</b> is UP\n\nEndpoint: <code>%s</code>\nResponse: %v%s",
							ep.Name, displayURL, responseTime.Round(time.Millisecond), downMsg))
						if sslWarnMsg != "" {
							m.SendAlert(sslWarnMsg)
						}
					}
					delete(m.acknowledged, ep.Name)
				} else {
					status.Consecutive++
					m.mu.Unlock()
					if sendLocalAlerts && shouldAlert && sendSlowAlert {
						m.SendAlert(fmt.Sprintf("⚠️ <b>%s</b> is SLOW\n\nEndpoint: <code>%s</code>\nResponse: %v (threshold: %v)",
							ep.Name, displayURL, responseTime.Round(time.Millisecond), m.slowThreshold))
					}
					if sslWarnMsg != "" {
						m.SendAlert(sslWarnMsg)
					}
				}
				log.Printf("✓ %s UP (%v)", ep.Name, responseTime.Round(time.Millisecond))
			} else {
				status.LastError = errMsg
				status.ConsecFailures++
				if status.ConsecFailures == m.failureThreshold && status.IsUp {
					status.IsUp = false
					status.DownSince = now
					delete(m.acknowledged, ep.Name)
					m.recordIncidentStart(dailyMetrics, now)
					m.recordIncidentStart(weeklyMetrics, now)
					m.startIncident(ep, now, errMsg)
					m.mu.Unlock()
					if sendLocalAlerts && shouldAlert && !acknowledged {
						m.SendAlert(fmt.Sprintf("🔴 <b>%s</b> is DOWN\n\nEndpoint: <code>%s</code>\nError: %s",
							ep.Name, displayURL, errMsg))
					}
				} else {
					m.mu.Unlock()
					if status.ConsecFailures < m.failureThreshold {
						log.Printf("⚠ %s failed (%d/%d): %s", ep.Name, status.ConsecFailures, m.failureThreshold, errMsg)
						return
					}
				}
				log.Printf("✗ %s DOWN: %s", ep.Name, errMsg)
			}
		}(ep)
	}
	wg.Wait()
	if m.coordinatorURL != "" {
		go m.SendRegionReport(ctx)
	}
}

var prometheusLabelReplacer = strings.NewReplacer(
	"\\", "\\\\",
	"\n", "\\n",
	"\"", "\\\"",
)

func prometheusLabelValue(value string) string {
	return prometheusLabelReplacer.Replace(value)
}

func (m *Monitor) MetricsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		defer m.mu.RUnlock()

		var builder strings.Builder
		builder.WriteString("# TYPE pulse_endpoint_up gauge\n")
		builder.WriteString("# TYPE pulse_endpoint_response_time_seconds gauge\n")
		builder.WriteString("# TYPE pulse_endpoint_total_checks counter\n")
		builder.WriteString("# TYPE pulse_endpoint_total_failures counter\n")

		for _, ep := range m.config.Endpoints {
			label := prometheusLabelValue(ep.Name)
			status := m.statuses[ep.Name]
			up := 0
			responseSeconds := 0.0
			if status != nil {
				if status.IsUp {
					up = 1
				}
				responseSeconds = status.ResponseTime.Seconds()
			}
			checks := m.totalChecks[ep.Name]
			failures := m.totalFailures[ep.Name]

			fmt.Fprintf(&builder, "pulse_endpoint_up{name=\"%s\"} %d\n", label, up)
			fmt.Fprintf(&builder, "pulse_endpoint_response_time_seconds{name=\"%s\"} %.6f\n", label, responseSeconds)
			fmt.Fprintf(&builder, "pulse_endpoint_total_checks{name=\"%s\"} %d\n", label, checks)
			fmt.Fprintf(&builder, "pulse_endpoint_total_failures{name=\"%s\"} %d\n", label, failures)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write([]byte(builder.String()))
	}
}

func (m *Monitor) BuildRegionReport() RegionReport {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]RegionStatus, len(m.statuses))
	for _, ep := range m.config.Endpoints {
		status := m.statuses[ep.Name]
		if status == nil {
			statuses[ep.Name] = RegionStatus{Up: false}
			continue
		}
		statuses[ep.Name] = RegionStatus{
			Up:             status.IsUp,
			LastCheck:      status.LastCheck,
			LastError:      status.LastError,
			ResponseTimeMs: status.ResponseTime.Milliseconds(),
		}
	}

	return RegionReport{Region: m.region, Statuses: statuses}
}

func (m *Monitor) ReloadConfig(trigger string) error {
	config, hash, err := LoadConfigWithHash(m.configPath)
	if err != nil {
		return err
	}
	if hash == m.configHash {
		return nil
	}

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

	region := strings.TrimSpace(config.Settings.Region)
	if region == "" {
		region = "default"
	}
	regionsRequired := config.Settings.RegionsRequired
	if regionsRequired == 0 {
		regionsRequired = 2
	}
	reportStaleAfter := 2 * checkInterval
	if config.Settings.ReportStaleAfter != "" {
		if parsed, err := time.ParseDuration(config.Settings.ReportStaleAfter); err == nil {
			reportStaleAfter = parsed
		}
	}
	if reportStaleAfter <= 0 {
		reportStaleAfter = 2 * checkInterval
	}

	added := []string{}
	removed := []string{}

	m.mu.Lock()
	newStatuses := make(map[string]*EndpointStatus)
	for _, ep := range config.Endpoints {
		if existing, ok := m.statuses[ep.Name]; ok {
			copyStatus := *existing
			copyStatus.Endpoint = ep
			newStatuses[ep.Name] = &copyStatus
		} else {
			newStatuses[ep.Name] = &EndpointStatus{Endpoint: ep, IsUp: true}
			added = append(added, ep.Name)
		}
	}
	for name := range m.statuses {
		if _, ok := newStatuses[name]; !ok {
			removed = append(removed, name)
		}
	}

	newDaily := make(map[string]*EndpointMetrics)
	newWeekly := make(map[string]*EndpointMetrics)
	newHistory := make(map[string][]HistoryPoint)
	newChecks := make(map[string]int)
	newFailures := make(map[string]int)
	newExpiry := make(map[string]time.Time)
	newWarn := make(map[string]time.Time)
	newRegions := make(map[string]map[string]RegionStatus)
	newAggregate := make(map[string]bool)
	newLastReport := make(map[string]time.Time)

	for _, ep := range config.Endpoints {
		if metric, ok := m.dailyMetrics[ep.Name]; ok {
			newDaily[ep.Name] = metric
		} else {
			newDaily[ep.Name] = &EndpointMetrics{}
		}
		if metric, ok := m.weeklyMetrics[ep.Name]; ok {
			newWeekly[ep.Name] = metric
		} else {
			newWeekly[ep.Name] = &EndpointMetrics{}
		}
		if history, ok := m.history[ep.Name]; ok {
			newHistory[ep.Name] = history
		} else {
			newHistory[ep.Name] = []HistoryPoint{}
		}
		newChecks[ep.Name] = m.totalChecks[ep.Name]
		newFailures[ep.Name] = m.totalFailures[ep.Name]
		newExpiry[ep.Name] = m.sslExpiry[ep.Name]
		newWarn[ep.Name] = m.sslLastWarn[ep.Name]
		if regionStatus, ok := m.regionReports[ep.Name]; ok {
			newRegions[ep.Name] = regionStatus
		}
		newAggregate[ep.Name] = m.aggregateStatus[ep.Name]
	}
	for regionName, lastSeen := range m.regionLastReport {
		newLastReport[regionName] = lastSeen
	}

	m.config = *config
	m.checkInterval = checkInterval
	m.slowThreshold = slowThreshold
	m.failureThreshold = failureThreshold
	m.defaultTimeout = defaultTimeout
	m.region = region
	m.coordinatorURL = strings.TrimSpace(config.Settings.CoordinatorURL)
	m.regionsRequired = regionsRequired
	m.regionAlerts = config.Settings.RegionAlerts
	m.reportStaleAfter = reportStaleAfter
	m.statuses = newStatuses
	m.dailyMetrics = newDaily
	m.weeklyMetrics = newWeekly
	m.history = newHistory
	m.totalChecks = newChecks
	m.totalFailures = newFailures
	m.sslExpiry = newExpiry
	m.sslLastWarn = newWarn
	m.regionReports = newRegions
	m.aggregateStatus = newAggregate
	m.regionLastReport = newLastReport
	m.configHash = hash
	m.mu.Unlock()

	log.Printf("Config reloaded via %s. Added endpoints: %v, removed endpoints: %v", trigger, added, removed)
	return nil
}

func (m *Monitor) reloadHandler(trigger string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := m.ReloadConfig(trigger); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(err.Error()))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (m *Monitor) watchConfig(ctx context.Context) {
	if m.configPath == "" {
		return
	}
	info, err := os.Stat(m.configPath)
	if err != nil {
		log.Printf("Config watch disabled: %v", err)
		return
	}
	lastMod := info.ModTime()
	lastSize := info.Size()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(m.configPath)
			if err != nil {
				continue
			}
			if info.ModTime().Equal(lastMod) && info.Size() == lastSize {
				continue
			}
			lastMod = info.ModTime()
			lastSize = info.Size()
			if err := m.ReloadConfig("watcher"); err != nil {
				log.Printf("Config reload failed: %v", err)
			}
		}
	}
}

func (m *Monitor) SendRegionReport(ctx context.Context) {
	if m.coordinatorURL == "" {
		return
	}
	report := m.BuildRegionReport()
	body, err := json.Marshal(report)
	if err != nil {
		log.Printf("Failed to encode region report: %v", err)
		return
	}

	endpoint := strings.TrimRight(m.coordinatorURL, "/") + "/report"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		log.Printf("Failed to create region report request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.alertClient.Do(req)
	if err != nil {
		log.Printf("Failed to send region report: %v", err)
		return
	}
	resp.Body.Close()
}

func (m *Monitor) RegionReportHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var report RegionReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if report.Region == "" {
			report.Region = "unknown"
		}

		now := time.Now()
		alerts := []string{}
		m.mu.Lock()
		m.regionLastReport[report.Region] = now
		for endpoint, status := range report.Statuses {
			regions := m.regionReports[endpoint]
			if regions == nil {
				regions = make(map[string]RegionStatus)
				m.regionReports[endpoint] = regions
			}
			previous, hadPrevious := regions[report.Region]
			regions[report.Region] = status
			if m.regionAlerts && hadPrevious && previous.Up != status.Up {
				state := "DOWN"
				if status.Up {
					state = "UP"
				}
				message := fmt.Sprintf("🌍 <b>%s</b> %s in %s\n\nEndpoint: <code>%s</code>",
					endpoint, state, report.Region, endpoint)
				alerts = append(alerts, message)
			}
		}
		alerts = append(alerts, m.evaluateAggregatesLocked(now)...)
		m.mu.Unlock()
		for _, message := range alerts {
			m.SendAlert(message)
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func (m *Monitor) evaluateAggregatesLocked(now time.Time) []string {
	alerts := []string{}
	if m.regionsRequired <= 1 {
		return alerts
	}
	for endpoint, regions := range m.regionReports {
		downRegions := []string{}
		activeRegions := 0
		for regionName, status := range regions {
			lastReport := m.regionLastReport[regionName]
			if lastReport.IsZero() || now.Sub(lastReport) > m.reportStaleAfter {
				delete(regions, regionName)
				continue
			}
			activeRegions++
			if !status.Up {
				downRegions = append(downRegions, regionName)
			}
		}
		if activeRegions == 0 {
			continue
		}
		if activeRegions < m.regionsRequired {
			continue
		}
		aggregatedDown := len(downRegions) >= m.regionsRequired
		previous, hadPrevious := m.aggregateStatus[endpoint]
		if hadPrevious && previous == aggregatedDown {
			continue
		}
		m.aggregateStatus[endpoint] = aggregatedDown
		state := "UP"
		icon := "✅"
		if aggregatedDown {
			state = "DOWN"
			icon = "🔴"
		}
		message := fmt.Sprintf("%s <b>%s</b> is %s in %d/%d regions\n\nEndpoint: <code>%s</code>\nRegions: %s",
			icon, endpoint, state, len(downRegions), activeRegions, endpoint, strings.Join(downRegions, ", "))
		alerts = append(alerts, message)
	}
	return alerts
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

	config, configHash, err := LoadConfigWithHash(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	monitor := NewMonitor(*config, configPath, configHash)

	port := config.Settings.Port
	if port == "" {
		port = "8080"
	}
	metricsPort := strings.TrimSpace(config.Settings.MetricsPort)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", monitor.HealthHandler())
	mux.HandleFunc("/status", monitor.HealthHandler())
	mux.HandleFunc("/reload", monitor.reloadHandler("api"))
	mux.HandleFunc("/report", monitor.RegionReportHandler())
	if metricsPort == "" || metricsPort == port {
		mux.HandleFunc("/metrics", monitor.MetricsHandler())
	}

	publicDashboard := true
	if config.Settings.PublicDashboard != nil {
		publicDashboard = *config.Settings.PublicDashboard
	}
	if publicDashboard {
		tmpl := template.Must(template.ParseFS(dashboardFS, "templates/index.html"))
		staticFS, err := fs.Sub(dashboardFS, "static")
		if err != nil {
			log.Fatalf("Failed to load dashboard assets: %v", err)
		}
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
		mux.HandleFunc("/api/summary", monitor.DashboardSummaryHandler())
		mux.HandleFunc("/api/history", monitor.DashboardHistoryHandler())
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
				http.NotFound(w, r)
				return
			}
			if err := tmpl.Execute(w, nil); err != nil {
				log.Printf("Failed to render dashboard: %v", err)
			}
		})
	}
	server := &http.Server{Addr: ":" + port, Handler: mux}

	var metricsServer *http.Server
	if metricsPort != "" && metricsPort != port {
		metricsMux := http.NewServeMux()
		metricsMux.HandleFunc("/metrics", monitor.MetricsHandler())
		metricsServer = &http.Server{Addr: ":" + metricsPort, Handler: metricsMux}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		for {
			sig := <-sigChan
			switch sig {
			case syscall.SIGHUP:
				if err := monitor.ReloadConfig("sighup"); err != nil {
					log.Printf("Config reload failed: %v", err)
				}
			case syscall.SIGINT, syscall.SIGTERM:
				log.Println("Shutting down...")
				cancel()
				server.Shutdown(context.Background())
				if metricsServer != nil {
					metricsServer.Shutdown(context.Background())
				}
				return
			}
		}
	}()

	go monitor.Run(ctx)
	go monitor.PollTelegramCommands(ctx)
	go monitor.ScheduleSummaryReports(ctx)
	go monitor.watchConfig(ctx)

	startupMsg := fmt.Sprintf("🚀 <b>Pulse Monitor Started</b>\n\n%d endpoints, interval: %s\n\n", len(config.Endpoints), monitor.checkInterval)
	for _, ep := range config.Endpoints {
		startupMsg += fmt.Sprintf("• %s (%s)\n", ep.Name, monitor.getDisplayURL(ep))
	}
	monitor.SendAlert(startupMsg)

	log.Printf("Server starting on :%s", port)
	if metricsServer != nil {
		go func() {
			log.Printf("Metrics server starting on :%s", metricsPort)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Metrics server error: %v", err)
			}
		}()
	}
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

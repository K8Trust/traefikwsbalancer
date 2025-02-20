package traefikwsbalancer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/K8Trust/traefikwsbalancer/ws"
)

// Version information
var (
	Version   = "dev"
	BuildTime = "unknown"
)

// Metrics
var (
	activeConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wsbalancer_active_connections",
			Help: "Number of active WebSocket connections per service",
		},
		[]string{"service"},
	)

	connectionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wsbalancer_connection_duration_seconds",
			Help:    "Duration of WebSocket connections",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		},
		[]string{"service"},
	)

	connectionErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wsbalancer_connection_errors_total",
			Help: "Total number of WebSocket connection errors",
		},
		[]string{"service", "error_type"},
	)
)

var registerMetricsOnce sync.Once

// CircuitBreaker handles service health monitoring
type CircuitBreaker struct {
	failures    atomic.Int64
	lastFailure atomic.Value // time.Time
	threshold   int64
	resetAfter  time.Duration
	mu          sync.RWMutex
}

// ConnectionPool manages WebSocket connections
type ConnectionPool struct {
	maxIdle     int
	idleTimeout time.Duration
	conns       chan *ws.Conn
	mu          sync.Mutex
}

type CircuitBreakerConfig struct {
	Threshold  int64         `json:"threshold" yaml:"threshold"`
	ResetAfter time.Duration `json:"resetAfter" yaml:"resetAfter"`
}

type ConnectionPoolConfig struct {
	MaxIdle     int           `json:"maxIdle" yaml:"maxIdle"`
	IdleTimeout time.Duration `json:"idleTimeout" yaml:"idleTimeout"`
}

type Config struct {
	MetricPath     string              `json:"metricPath,omitempty" yaml:"metricPath"`
	Services       []string            `json:"services,omitempty" yaml:"services"`
	CacheTTL       int                 `json:"cacheTTL" yaml:"cacheTTL"`
	MaxRetries     int                 `json:"maxRetries" yaml:"maxRetries"`
	RetryBackoff   time.Duration       `json:"retryBackoff" yaml:"retryBackoff"`
	CircuitBreaker CircuitBreakerConfig `json:"circuitBreaker" yaml:"circuitBreaker"`
	ConnectionPool ConnectionPoolConfig  `json:"connectionPool" yaml:"connectionPool"`
}

// Balancer core implementation
type Balancer struct {
	Next         http.Handler
	Name         string
	Services     []string
	Client       *http.Client
	MetricPath   string
	DialTimeout  time.Duration
	WriteTimeout time.Duration
	ReadTimeout  time.Duration

	activeConns    sync.Map // map[string]*atomic.Int64
	connCache      sync.Map
	cacheTTL       time.Duration
	circuitBreaker sync.Map // map[string]*CircuitBreaker
	connPools      sync.Map // map[string]*ConnectionPool
	logger         *slog.Logger
}

func NewCircuitBreaker(threshold int64, resetAfter time.Duration) *CircuitBreaker {
	cb := &CircuitBreaker{
		threshold:  threshold,
		resetAfter: resetAfter,
	}
	cb.lastFailure.Store(time.Now())
	return cb
}

func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures.Add(1)
	cb.lastFailure.Store(time.Now())
}

func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures.Store(0)
}

func (cb *CircuitBreaker) IsOpen() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	if cb.failures.Load() > cb.threshold {
		lastFailure := cb.lastFailure.Load().(time.Time)
		if time.Since(lastFailure) < cb.resetAfter {
			return true
		}
		cb.failures.Store(0)
	}
	return false
}

func NewConnectionPool(maxIdle int, idleTimeout time.Duration) *ConnectionPool {
	return &ConnectionPool{
		maxIdle:     maxIdle,
		idleTimeout: idleTimeout,
		conns:       make(chan *ws.Conn, maxIdle),
	}
}

func CreateConfig() *Config {
	return &Config{
		MetricPath:   "/metric",
		CacheTTL:     30,
		MaxRetries:   3,
		RetryBackoff: 1 * time.Second,
		CircuitBreaker: CircuitBreakerConfig{
			Threshold:  5,
			ResetAfter: 30 * time.Second,
		},
		ConnectionPool: ConnectionPoolConfig{
			MaxIdle:     10,
			IdleTimeout: 60 * time.Second,
		},
	}
}

func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: true,
	}))

	logger.Info("Initializing TraefikWSBalancer Plugin",
		"version", Version,
		"buildTime", BuildTime,
		"config", config,
	)

	if len(config.Services) == 0 {
		return nil, fmt.Errorf("no services configured")
	}

	b := &Balancer{
		Next:         next,
		Name:         name,
		Services:     config.Services,
		Client:       &http.Client{Timeout: 5 * time.Second},
		MetricPath:   config.MetricPath,
		cacheTTL:     time.Duration(config.CacheTTL) * time.Second,
		DialTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  30 * time.Second,
		logger:       logger,
	}

	for _, service := range config.Services {
		b.activeConns.Store(service, &atomic.Int64{})
		b.circuitBreaker.Store(service, NewCircuitBreaker(
			config.CircuitBreaker.Threshold,
			config.CircuitBreaker.ResetAfter,
		))
		b.connPools.Store(service, NewConnectionPool(
			config.ConnectionPool.MaxIdle,
			config.ConnectionPool.IdleTimeout,
		))
	}

	registerMetricsOnce.Do(func() {
		prometheus.MustRegister(activeConnections, connectionDuration, connectionErrors)
	})

	return b, nil
}

// The remainder of the code remains unchanged

func (b *Balancer) selectService() (string, error) {
	var selectedService string
	minConnections := int64(^uint64(0) >> 1)
	allServiceConnections := make(map[string]int64)

	for _, service := range b.Services {
		if cb, ok := b.circuitBreaker.Load(service); ok {
			if cb.(*CircuitBreaker).IsOpen() {
				continue
			}
		}

		counter, ok := b.activeConns.Load(service)
		if !ok {
			continue
		}
		connections := counter.(*atomic.Int64).Load()
		allServiceConnections[service] = connections

		if connections < minConnections {
			minConnections = connections
			selectedService = service
		}
	}

	if selectedService == "" {
		return "", fmt.Errorf("no available services")
	}

	return selectedService, nil
}

func (b *Balancer) trackConnection(service string, delta int64) {
	if counter, ok := b.activeConns.Load(service); ok {
		newCount := counter.(*atomic.Int64).Add(delta)
		activeConnections.WithLabelValues(service).Set(float64(newCount))
		b.logger.Debug("Connection count changed",
			"service", service,
			"delta", delta,
			"newCount", newCount,
		)
	}
}

func (b *Balancer) getServiceConnections(service string) int64 {
	if counter, ok := b.activeConns.Load(service); ok {
		return counter.(*atomic.Int64).Load()
	}
	return 0
}

func (b *Balancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	logger := b.logger.With(
		"method", req.Method,
		"path", req.URL.Path,
		"remote_addr", req.RemoteAddr,
	)

	selectedService, err := b.selectService()
	if err != nil {
		logger.Error("Service selection failed", "error", err)
		http.Error(rw, "No available service", http.StatusServiceUnavailable)
		return
	}

	logger.Info("Selected service for request",
		"service", selectedService,
		"active_connections", b.getServiceConnections(selectedService),
	)

	if ws.IsWebSocketUpgrade(req) {
		b.handleWebSocket(rw, req, selectedService)
		return
	}

	b.handleHTTP(rw, req, selectedService)
}

func (b *Balancer) handleWebSocket(rw http.ResponseWriter, req *http.Request, targetService string) {
	ctx := req.Context()
	logger := b.logger.With(
		"service", targetService,
		"remoteAddr", req.RemoteAddr,
		"path", req.URL.Path,
	)

	if cb, ok := b.circuitBreaker.Load(targetService); ok {
		if cb.(*CircuitBreaker).IsOpen() {
			logger.Error("Circuit breaker open for service", "service", targetService)
			connectionErrors.WithLabelValues(targetService, "circuit_breaker").Inc()
			http.Error(rw, "Service temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	}

	b.trackConnection(targetService, 1)
	connectionStart := time.Now()
	defer func() {
		b.trackConnection(targetService, -1)
		connectionDuration.WithLabelValues(targetService).Observe(time.Since(connectionStart).Seconds())
	}()

	headers := b.prepareHeaders(req)
	targetURL := b.createTargetURL(targetService, req)
	logger.Debug("Dialing backend WebSocket", "targetURL", targetURL)

	dialer := ws.Dialer{
		HandshakeTimeout: b.DialTimeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}

	backendConn, resp, err := b.connectWithRetry(ctx, dialer, targetURL, headers)
	if err != nil {
		b.handleConnectionError(rw, targetService, err, resp, logger)
		return
	}
	defer backendConn.Close()

	clientConn, err := b.upgradeClientConnection(rw, req)
	if err != nil {
		logger.Error("Failed to upgrade client connection", "error", err)
		return
	}
	defer clientConn.Close()

	b.handleBidirectionalCommunication(ctx, clientConn, backendConn, targetService, logger)
}

func (b *Balancer) handleConnectionError(rw http.ResponseWriter, targetService string, err error, resp *http.Response, logger *slog.Logger) {
	if cb, ok := b.circuitBreaker.Load(targetService); ok {
		cb.(*CircuitBreaker).RecordFailure()
	}

	connectionErrors.WithLabelValues(targetService, "connection_failed").Inc()
	logger.Error("Backend connection failed", "error", err)

	if resp != nil {
		for k, v := range resp.Header {
			rw.Header()[k] = v
		}
		rw.WriteHeader(resp.StatusCode)
		io.Copy(rw, resp.Body)
		resp.Body.Close()
	} else {
		http.Error(rw, "Service Unavailable", http.StatusServiceUnavailable)
	}
}

func (b *Balancer) connectWithRetry(ctx context.Context, dialer ws.Dialer, targetURL string, headers http.Header) (*ws.Conn, *http.Response, error) {
	var lastErr error
	for retries := 0; retries < 3; retries++ {
		conn, resp, err := dialer.Dial(targetURL, headers)
		if err == nil {
			return conn, resp, nil
		}

		lastErr = err
		if resp != nil && resp.StatusCode == http.StatusForbidden {
			return nil, resp, err
		}

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(time.Second * time.Duration(retries+1)):
			continue
		}
	}
	return nil, nil, fmt.Errorf("failed after retries: %v", lastErr)
}

func (b *Balancer) handleBidirectionalCommunication(ctx context.Context, clientConn, backendConn *ws.Conn, targetService string, logger *slog.Logger) {
	errChan := make(chan error, 2)
	done := make(chan struct{})
	defer close(done)

	// Client to backend
	go b.forwardMessages(ctx, clientConn, backendConn, "client", "backend", b.WriteTimeout, errChan, done, logger)
	// Backend to client
	go b.forwardMessages(ctx, backendConn, clientConn, "backend", "client", b.WriteTimeout, errChan, done, logger)

	select {
	case err := <-errChan:
		logger.Info("Connection closed",
			"error", err,
			"activeConnections", b.getServiceConnections(targetService),
		)
	case <-ctx.Done():
		logger.Info("Context cancelled", "reason", ctx.Err())
	}
}

func (b *Balancer) forwardMessages(ctx context.Context, src, dst *ws.Conn, srcName, dstName string, writeTimeout time.Duration, errChan chan error, done chan struct{}, logger *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Panic in message forwarder",
				"source", srcName,
				"destination", dstName,
				"error", r,
			)
			errChan <- fmt.Errorf("panic in %s->%s forwarder: %v", srcName, dstName, r)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		default:
			messageType, message, err := src.ReadMessage()
			if err != nil {
				errChan <- fmt.Errorf("%s read error: %v", srcName, err)
				return
			}

			logger.Debug("Message forwarded",
				"source", srcName,
				"destination", dstName,
				"messageType", messageType,
				"messageSize", len(message),
			)

			dst.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := dst.WriteMessage(messageType, message); err != nil {
				errChan <- fmt.Errorf("%s write error: %v", dstName, err)
				return
			}
		}
	}
}

func (b *Balancer) createTargetURL(targetService string, req *http.Request) string {
	targetURL := "ws" + strings.TrimPrefix(targetService, "http") + req.URL.Path
	if req.URL.RawQuery != "" {
		targetURL += "?" + req.URL.RawQuery
	}
	return targetURL
}

func (b *Balancer) prepareHeaders(req *http.Request) http.Header {
	headers := make(http.Header)

	// Copy essential headers
	essentialHeaders := []string{"Authorization", "Agent-Id", "X-Account", "X-Forwarded-For", "X-Real-IP"}
	for _, header := range essentialHeaders {
		if value := req.Header.Get(header); value != "" {
			headers.Set(header, value)
		}
	}

	// WebSocket specific headers
	headers.Set("Upgrade", "websocket")
	headers.Set("Connection", "Upgrade")
	headers.Set("Sec-WebSocket-Version", "13")

	if wsKey := req.Header.Get("Sec-WebSocket-Key"); wsKey != "" {
		headers.Set("Sec-WebSocket-Key", wsKey)
	}

	if wsProtocol := req.Header.Get("Sec-WebSocket-Protocol"); wsProtocol != "" {
		headers.Set("Sec-WebSocket-Protocol", wsProtocol)
	}

	return headers
}

func (b *Balancer) upgradeClientConnection(rw http.ResponseWriter, req *http.Request) (*ws.Conn, error) {
	upgrader := ws.Upgrader{
		HandshakeTimeout: b.DialTimeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	return upgrader.Upgrade(rw, req, nil)
}

func (b *Balancer) handleHTTP(rw http.ResponseWriter, req *http.Request, targetService string) {
	targetURL := targetService + req.URL.Path
	proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, req.Body)
	if err != nil {
		b.logger.Error("Failed to create proxy request", "error", err)
		http.Error(rw, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Copy headers
	for key, values := range req.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}
	proxyReq.Host = req.Host
	proxyReq.URL.RawQuery = req.URL.RawQuery

	resp, err := b.Client.Do(proxyReq)
	if err != nil {
		b.logger.Error("Proxy request failed", "error", err)
		http.Error(rw, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			rw.Header().Add(key, value)
		}
	}
	rw.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(rw, resp.Body); err != nil {
		b.logger.Error("Failed to copy response body", "error", err)
	}
}

// Package traefik_queue_manager implements a Traefik middleware plugin that manages
// access to services by implementing a queue when capacity is reached.
// It functions similar to a virtual waiting room, allowing a controlled number
// of users to access the service while placing others in a structured queue.
package traefik_queue_manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net" // Added net import
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Constants for configuration defaults and internal settings.
const (
	defaultLogLevel              = "info" // Default logging level
	logPrefix                    = "[QueueManager] "
	defaultInactivityTimeoutSecs = 60   // Default seconds for a session to be considered inactive
	defaultCleanupIntervalSecs   = 30   // Default seconds for how often cleanup logic runs (reduced for responsiveness)
	defaultMaxEntries            = 100  // Default maximum concurrent users
	defaultHTTPResponseCode      = http.StatusTooManyRequests
	defaultHTTPContentType       = "text/html; charset=utf-8"
	defaultUseCookies            = true
	defaultCookieName            = "queue-manager-id"
	defaultCookieMaxAgeSecs      = 3600 // 1 hour
	defaultQueueStrategy         = "fifo" // Currently only "fifo" is implemented
	defaultRefreshIntervalSecs   = 20   // Default seconds for queue page refresh (reduced for better UX)
	defaultMinWaitTimeMinutes    = 1    // Default minimum wait time displayed to users
	defaultQueuePageFile         = "queue-page.html"
	secureIDLengthBytes          = 16   // Number of random bytes for secure cookie ID generation
)

// Config holds the plugin configuration.
type Config struct {
	Enabled                   bool   `json:"enabled"`                   // Enable/disable the queue manager
	QueuePageFile             string `json:"queuePageFile"`             // Path to queue page HTML template
	InactivityTimeoutSeconds  int    `json:"inactivityTimeoutSeconds"`  // How long an inactive session is valid for (in seconds)
	HardSessionLimitSeconds   int    `json:"hardSessionLimitSeconds"`   // Optional: Absolute max time for an active session (seconds), 0 to disable
	CleanupIntervalSeconds    int    `json:"cleanupIntervalSeconds"`    // How often to run cleanup logic (in seconds)
	MaxEntries                int    `json:"maxEntries"`                // Maximum concurrent users
	HTTPResponseCode          int    `json:"httpResponseCode"`          // HTTP response code for queue page
	HTTPContentType           string `json:"httpContentType"`           // Content type of queue page
	UseCookies                bool   `json:"useCookies"`                // Use cookies or IP+UserAgent hash for client identification
	CookieName                string `json:"cookieName"`                // Name of the cookie
	CookieMaxAgeSeconds       int    `json:"cookieMaxAgeSeconds"`       // Max age of the cookie in seconds
	QueueStrategy             string `json:"queueStrategy"`             // Queue strategy: "fifo" (currently only supported)
	RefreshIntervalSeconds    int    `json:"refreshIntervalSeconds"`    // Refresh interval for queue page (in seconds)
	Debug                     bool   `json:"debug"`                     // Enable verbose debug logging (overrides LogLevel to debug)
	MinWaitTimeMinutes        int    `json:"minWaitTimeMinutes"`        // Minimum wait time to show users (in minutes)
	LogFile                   string `json:"logFile"`                   // Optional: Path to a log file. Default is stderr.
	LogLevel                  string `json:"logLevel"`                  // Logging level: "debug", "info", "warn", "error"
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		Enabled:                   true,
		QueuePageFile:             defaultQueuePageFile,
		InactivityTimeoutSeconds:  defaultInactivityTimeoutSecs,
		HardSessionLimitSeconds:   0, // Disabled by default
		CleanupIntervalSeconds:    defaultCleanupIntervalSecs,
		MaxEntries:                defaultMaxEntries,
		HTTPResponseCode:          defaultHTTPResponseCode,
		HTTPContentType:           defaultHTTPContentType,
		UseCookies:                defaultUseCookies,
		CookieName:                defaultCookieName,
		CookieMaxAgeSeconds:       defaultCookieMaxAgeSecs,
		QueueStrategy:             defaultQueueStrategy,
		RefreshIntervalSeconds:    defaultRefreshIntervalSecs,
		Debug:                     false,
		MinWaitTimeMinutes:        defaultMinWaitTimeMinutes,
		LogFile:                   "",
		LogLevel:                  defaultLogLevel,
	}
}

// Session represents a visitor session in the queue or active service.
type Session struct {
	ID           string    `json:"id"`           // Unique client identifier
	CreatedAt    time.Time `json:"createdAt"`    // Timestamp when the session was first created (for hard limit)
	LastSeenAt   time.Time `json:"lastSeenAt"`   // Timestamp when the client was last seen (for inactivity)
	HardExpiryAt time.Time `json:"hardExpiryAt"` // Absolute expiry time, if hard session limit is enabled
	Position     int       `json:"position"`     // 0-based position in the queue (-1 if not in queue/active)
}

// QueuePageData contains data to be passed to the HTML template.
type QueuePageData struct {
	Position           int    `json:"position"`           // 1-based position in queue for display
	QueueSize          int    `json:"queueSize"`          // Total current queue size
	EstimatedWaitTime  int    `json:"estimatedWaitTime"`  // Estimated wait time in minutes
	RefreshInterval    int    `json:"refreshInterval"`    // Refresh interval in seconds for the page
	ProgressPercentage int    `json:"progressPercentage"` // Visual progress percentage
	Message            string `json:"message"`            // Custom message (currently static)
	DebugInfo          string `json:"debugInfo"`          // Debug information (only shown if debug mode enabled)
}

// QueueManager is the main middleware handler struct.
type QueueManager struct {
	next    http.Handler
	name    string
	config  *Config
	logger  *log.Logger
	cache   *SimpleCache // In-memory cache for session data
	tpl     *template.Template
	tplLock sync.RWMutex // For thread-safe template parsing/reloading

	// Queue and active session management
	queue            []Session         // FIFO queue of waiting sessions
	activeSessionIDs map[string]bool   // Set of currently active session IDs (value is always true)
	mu               sync.RWMutex      // Mutex for thread-safe access to queue and activeSessionIDs

	// Durations derived from config for convenience
	inactivityTimeoutDur time.Duration
	hardSessionLimitDur  time.Duration // Will be 0 if not configured
	cleanupIntervalDur   time.Duration

	// Cleanup routine
	cleanupTicker *time.Ticker
	stopCleanup   chan bool // Channel to signal the cleanup goroutine to stop

	// Log file handle if a log file is used
	logFileHandle *os.File
}

// logf provides leveled logging for the plugin.
func (qm *QueueManager) logf(level string, format string, v ...interface{}) {
	if qm.logger == nil {
		log.Printf(logPrefix+"[NO LOGGER] "+format, v...) // Fallback if logger somehow not initialized
		return
	}

	// Determine effective log level (debug flag overrides configured level)
	effectiveLogLevel := qm.config.LogLevel
	if qm.config.Debug {
		effectiveLogLevel = "debug"
	}

	// Check if the message should be logged based on its level and the effective log level
	shouldLog := false
	switch strings.ToLower(effectiveLogLevel) {
	case "debug":
		shouldLog = true // Debug logs everything
	case "info":
		shouldLog = (level == "info" || level == "warn" || level == "error" || level == "debug") // Info also logs debug if debug is true
	case "warn":
		shouldLog = (level == "warn" || level == "error")
	case "error":
		shouldLog = (level == "error")
	default: // Unknown log level, default to info behavior
		shouldLog = (level == "info" || level == "warn" || level == "error")
	}
	if qm.config.Debug && level != "debug" { // If debug is true, log everything above error as debug too
		if level == "info" || level == "warn" {
			// log as debug
		}
	}

	if shouldLog {
		qm.logger.Printf(logPrefix+strings.ToUpper(level)+": "+format, v...)
	}
}

// New creates a new queue manager middleware instance.
func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	// Validate configuration
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid QueueManager configuration: %w", err)
	}

	// Setup logger
	logOutput, logFileHandle := setupLogOutput(config)
	logger := log.New(logOutput, "", log.LstdFlags|log.Lmicroseconds) // Added microseconds for finer log timing

	qm := &QueueManager{
		next:                 next,
		name:                 name,
		config:               config,
		logger:               logger,
		queue:                make([]Session, 0),
		activeSessionIDs:     make(map[string]bool),
		stopCleanup:          make(chan bool),
		inactivityTimeoutDur: time.Duration(config.InactivityTimeoutSeconds) * time.Second,
		cleanupIntervalDur:   time.Duration(config.CleanupIntervalSeconds) * time.Second,
		logFileHandle:        logFileHandle, // Store the file handle
	}

	if config.HardSessionLimitSeconds > 0 {
		qm.hardSessionLimitDur = time.Duration(config.HardSessionLimitSeconds) * time.Second
	}

	// Initialize cache
	// The cache's internal cleanup can be more frequent than the plugin's main cleanup logic.
	cacheInternalCleanupInterval := qm.inactivityTimeoutDur / 2
	if cacheInternalCleanupInterval < 5*time.Second { // Minimum 5s for cache cleanup
		cacheInternalCleanupInterval = 5 * time.Second
	}
	qm.cache = NewSimpleCache(qm.inactivityTimeoutDur, cacheInternalCleanupInterval)

	// Attempt to load template during initialization
	if err := qm.loadTemplate(); err != nil {
		qm.logf("warn", "Could not load queue page template '%s' during init: %v. Will attempt on first request. Ensure file is accessible.", config.QueuePageFile, err)
	}

	// Start periodic cleanup goroutine
	qm.startCleanupRoutine()

	qm.logf("info", "QueueManager plugin '%s' initialized. MaxEntries: %d, InactivityTimeout: %v, HardSessionLimit: %v, CleanupInterval: %v",
		name, config.MaxEntries, qm.inactivityTimeoutDur, qm.hardSessionLimitDur, qm.cleanupIntervalDur)
	if config.Debug {
		qm.logf("debug", "Full configuration: %+v", config)
	}

	return qm, nil
}

// validateConfig checks for essential configuration parameters.
func validateConfig(config *Config) error {
	if config.MaxEntries <= 0 {
		return fmt.Errorf("maxEntries must be greater than 0 (got %d)", config.MaxEntries)
	}
	if config.InactivityTimeoutSeconds <= 0 {
		return fmt.Errorf("inactivityTimeoutSeconds must be greater than 0 (got %d)", config.InactivityTimeoutSeconds)
	}
	if config.CleanupIntervalSeconds <= 0 {
		return fmt.Errorf("cleanupIntervalSeconds must be greater than 0 (got %d)", config.CleanupIntervalSeconds)
	}
	if config.HardSessionLimitSeconds < 0 { // 0 is valid (disabled)
		return fmt.Errorf("hardSessionLimitSeconds cannot be negative (got %d)", config.HardSessionLimitSeconds)
	}
	if config.RefreshIntervalSeconds <= 0 {
		return fmt.Errorf("refreshIntervalSeconds must be greater than 0 (got %d)", config.RefreshIntervalSeconds)
	}
	if config.QueuePageFile == "" {
		return fmt.Errorf("queuePageFile cannot be empty")
	}
	return nil
}

// setupLogOutput configures the log writer (stderr or file).
func setupLogOutput(config *Config) (io.Writer, *os.File) {
	var logOutput io.Writer = os.Stderr
	var fileHandle *os.File = nil // Keep track of file handle to close it later if possible
	if config.LogFile != "" {
		file, err := os.OpenFile(config.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf(logPrefix+"WARN: Could not open log file %s: %v. Defaulting to stderr.", config.LogFile, err)
		} else {
			logOutput = file
			fileHandle = file
		}
	}
	return logOutput, fileHandle
}

// loadTemplate reads and parses the HTML template file.
func (qm *QueueManager) loadTemplate() error {
	qm.tplLock.Lock()
	defer qm.tplLock.Unlock()

	if !fileExists(qm.config.QueuePageFile) {
		qm.tpl = nil // Ensure template is nil if file doesn't exist
		return fmt.Errorf("template file not found: %s", qm.config.QueuePageFile)
	}

	content, err := os.ReadFile(qm.config.QueuePageFile)
	if err != nil {
		qm.tpl = nil
		return fmt.Errorf("error reading template file '%s': %w", qm.config.QueuePageFile, err)
	}

	newTpl, parseErr := template.New("QueuePage").Delims("[[", "]]").Parse(string(content))
	if parseErr != nil {
		qm.tpl = nil
		return fmt.Errorf("error parsing template '%s': %w", qm.config.QueuePageFile, parseErr)
	}

	qm.tpl = newTpl
	qm.logf("info", "Successfully loaded/reloaded queue page template: %s", qm.config.QueuePageFile)
	return nil
}

// getTemplate safely retrieves the current parsed template.
func (qm *QueueManager) getTemplate() *template.Template {
	qm.tplLock.RLock()
	defer qm.tplLock.RUnlock()
	return qm.tpl
}

// startCleanupRoutine initializes and starts the periodic cleanup task.
func (qm *QueueManager) startCleanupRoutine() {
	qm.cleanupTicker = time.NewTicker(qm.cleanupIntervalDur)
	go func() {
		for {
			select {
			case <-qm.cleanupTicker.C:
				qm.logf("debug", "Cleanup routine triggered.")
				qm.CleanupExpiredSessions()
			case <-qm.stopCleanup:
				qm.cleanupTicker.Stop()
				qm.logf("info", "Cleanup routine has been stopped.")
				return
			}
		}
	}()
	qm.logf("info", "Cleanup routine started. Interval: %v", qm.cleanupIntervalDur)
}

// ServeHTTP is the main entry point for incoming HTTP requests.
func (qm *QueueManager) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if !qm.config.Enabled {
		qm.next.ServeHTTP(rw, req)
		return
	}

	clientID, _ := qm.getClientID(rw, req) // isNewClient info not strictly needed here
	qm.logf("debug", "Request from ClientID: %s, Path: %s", clientID, req.URL.Path)

	// Attempt to let the client proceed if conditions are met
	if qm.tryProceedClient(clientID, rw, req) {
		qm.logf("debug", "Client %s allowed to proceed to service.", clientID)
		qm.next.ServeHTTP(rw, req)
		return
	}

	// Client cannot proceed, must be queued or is already in queue
	positionInQueue := qm.enqueueOrUpdateClient(clientID)
	qm.logf("debug", "Client %s is being served queue page. Position (0-based): %d", clientID, positionInQueue)
	qm.serveQueuePage(rw, req, positionInQueue)
}

// tryProceedClient checks if the client can access the service.
// Handles active sessions, hard limits, and capacity checks.
// Returns true if client proceeds, false otherwise.
// This function acquires the main lock (qm.mu).
func (qm *QueueManager) tryProceedClient(clientID string, rw http.ResponseWriter, req *http.Request) bool {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	// 1. Check if client is already active
	if qm.activeSessionIDs[clientID] {
		sessionData, foundInCache := qm.getSessionFromCache(clientID)
		if !foundInCache {
			// Was marked active, but its session expired from cache (due to inactivity).
			qm.logf("info", "Client %s was active, but session expired from cache (inactivity). Removing from active list.", clientID)
			delete(qm.activeSessionIDs, clientID)
			// Client will be re-evaluated below for capacity or requeue.
		} else {
			// Active and session found in cache. Check for hard limit.
			if qm.hardSessionLimitDur > 0 && time.Now().After(sessionData.HardExpiryAt) {
				qm.logf("info", "Client %s active session reached hard limit (Expiry: %v). Removing from active list.", clientID, sessionData.HardExpiryAt)
				delete(qm.activeSessionIDs, clientID)
				qm.cache.Delete(clientID) // Explicitly remove from cache as well
				// Client will be re-evaluated below for capacity or requeue.
			} else {
				// Active, in cache, and within hard limit. Update last seen and allow to proceed.
				qm.logf("debug", "Client %s is active and within limits. Updating activity and allowing proceed.", clientID)
				qm.updateClientActivityInCache(clientID, sessionData)
				return true
			}
		}
	}

	// 2. Client is not currently active (or was just removed). Check for capacity.
	if len(qm.activeSessionIDs) < qm.config.MaxEntries {
		qm.logf("info", "Capacity available (%d/%d). Promoting client %s to active.", len(qm.activeSessionIDs), qm.config.MaxEntries, clientID)

		// If client was in the queue, remove them first.
		qm.dequeueClientUnderLock(clientID) // This helper assumes qm.mu is already locked.

		now := time.Now()
		newSession := Session{
			ID:         clientID,
			CreatedAt:  now,
			LastSeenAt: now,
			Position:   -1, // Mark as not in queue
		}
		if qm.hardSessionLimitDur > 0 {
			newSession.HardExpiryAt = now.Add(qm.hardSessionLimitDur)
		}

		qm.activeSessionIDs[clientID] = true
		qm.cache.Set(clientID, newSession, qm.inactivityTimeoutDur) // Add/update cache with inactivity timeout

		return true
	}

	// 3. No capacity, and client is not currently active and allowed.
	qm.logf("debug", "Client %s cannot proceed. No capacity (%d/%d) or session expired.", clientID, len(qm.activeSessionIDs), qm.config.MaxEntries)
	return false
}

// enqueueOrUpdateClient places a client in the queue or updates their LastSeenAt if already queued.
// Returns the client's 0-based position in the queue.
// This function acquires the main lock (qm.mu).
func (qm *QueueManager) enqueueOrUpdateClient(clientID string) int {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	// Check if client is already in the queue
	for i := range qm.queue {
		if qm.queue[i].ID == clientID {
			qm.logf("debug", "Client %s already in queue at position %d. Updating activity.", clientID, i)
			// Update LastSeenAt for the existing queue entry (via cache)
			if sessionData, found := qm.getSessionFromCache(clientID); found {
				qm.updateClientActivityInCache(clientID, sessionData) // Updates cache
				qm.queue[i].LastSeenAt = time.Now()                  // Also update in-memory queue representation
			} else {
				// Session not in cache, but in queue? This is inconsistent. Re-add to cache.
				qm.logf("warn", "Client %s in queue but not in cache. Re-caching.", clientID)
				qm.queue[i].LastSeenAt = time.Now()
				qm.cache.Set(clientID, qm.queue[i], qm.inactivityTimeoutDur)
			}
			return i // Return current position
		}
	}

	// Client is not in queue, add them
	now := time.Now()
	newQueueSession := Session{
		ID:         clientID,
		CreatedAt:  now, // Time they entered the queue system
		LastSeenAt: now,
		Position:   len(qm.queue), // 0-based position for the new entry
		// HardExpiryAt is typically not set for queued items, only for active ones.
	}
	qm.queue = append(qm.queue, newQueueSession)
	// Add to cache with inactivity timeout; queued users also need to show activity.
	qm.cache.Set(clientID, newQueueSession, qm.inactivityTimeoutDur)

	qm.logf("info", "Client %s added to queue at position %d. Current queue size: %d", clientID, newQueueSession.Position, len(qm.queue))
	return newQueueSession.Position
}

// getSessionFromCache retrieves and type-asserts a session from the cache.
// This is a read-only operation on the cache itself.
func (qm *QueueManager) getSessionFromCache(clientID string) (Session, bool) {
	obj, found := qm.cache.Get(clientID) // This cache.Get is thread-safe
	if !found {
		return Session{}, false
	}
	sessionData, ok := obj.(Session)
	if !ok {
		qm.logf("warn", "Failed to assert session data type from cache for client %s. Cache data: %+v. Treating as not found.", clientID, obj)
		// Optionally, delete the problematic cache entry: qm.cache.Delete(clientID)
		return Session{}, false
	}
	return sessionData, true
}

// updateClientActivityInCache updates the LastSeenAt timestamp for a client's session in the cache.
func (qm *QueueManager) updateClientActivityInCache(clientID string, sessionData Session) {
	sessionData.LastSeenAt = time.Now()
	// Important: HardExpiryAt is NOT updated by activity. It's a fixed point from CreatedAt.
	qm.cache.Set(clientID, sessionData, qm.inactivityTimeoutDur) // Reset inactivity timer by re-setting in cache
	qm.logf("debug", "Updated LastSeenAt for client %s in cache.", clientID)
}

// getClientID generates or retrieves a unique client identifier using cookies or IP/UserAgent hash.
// Returns (clientID, isTrulyNewCookie).
func (qm *QueueManager) getClientID(rw http.ResponseWriter, req *http.Request) (string, bool) {
	if qm.config.UseCookies {
		cookie, err := req.Cookie(qm.config.CookieName)
		if err == nil && cookie.Value != "" {
			// Cookie exists, return its value.
			// Optionally, refresh cookie's MaxAge if desired, but not strictly necessary for ID retrieval.
			// http.SetCookie(rw, &http.Cookie{... Same params but updated MaxAge ...})
			return cookie.Value, false // Not a new cookie
		}

		// No valid cookie found, create a new one.
		randomPart := generateSecureRandomID(secureIDLengthBytes)
		// Optionally, add a short, non-sensitive hash part for easier server-side log correlation if IPs change.
		// clientHashPart := generateClientHash(req)[:8]
		// newID := fmt.Sprintf("%s-%s", randomPart, clientHashPart)
		newID := randomPart

		http.SetCookie(rw, &http.Cookie{
			Name:     qm.config.CookieName,
			Value:    newID,
			Path:     "/", // Cookie accessible for all paths
			MaxAge:   qm.config.CookieMaxAgeSeconds,
			HttpOnly: true,   // Prevent client-side script access
			Secure:   req.TLS != nil, // Set Secure flag only if connection is HTTPS
			SameSite: http.SameSiteLaxMode,
		})
		qm.logf("debug", "New cookie issued for client. ID: %s (Secure: %t)", newID, req.TLS != nil)
		return newID, true // This is a new cookie
	}

	// Not using cookies, use IP + UserAgent hash.
	hashID := generateClientHash(req)
	qm.logf("debug", "Using IP+UserAgent hash for client. Hash: %s", hashID)
	// isTrulyNewCookie is false as hashes are deterministic for the same IP/UA.
	return hashID, false
}

// generateSecureRandomID creates a cryptographically secure random hex string.
func generateSecureRandomID(lengthBytes int) string {
	bytes := make([]byte, lengthBytes)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to a less secure method if crypto/rand fails (should be extremely rare).
		log.Printf(logPrefix+"ERROR: crypto/rand.Read failed: %v. Using timestamp-based fallback for ID generation. THIS IS NOT SECURE.", err)
		// This fallback is NOT cryptographically secure. Production systems must have a working crypto/rand.
		timestamp := time.Now().UnixNano()
		fallbackStr := fmt.Sprintf("%x", timestamp) // Hex representation of timestamp
		// Ensure it has some length, pad if necessary
		for i := 0; i < lengthBytes && i < len(fallbackStr); i++ {
			bytes[i] = fallbackStr[i]
		}
		// If still too short, fill with pseudo-random based on timestamp
		for i := len(fallbackStr); i < lengthBytes; i++ {
			bytes[i] = byte((timestamp + int64(i)*101) % 256) // 101 is a prime
		}
	}
	return hex.EncodeToString(bytes)
}

// generateClientHash creates a SHA256 hash from client IP and User-Agent.
func generateClientHash(req *http.Request) string {
	clientIP := getClientIP(req)
	userAgent := req.UserAgent() // Can be empty

	hasher := sha256.New()
	hasher.Write([]byte(clientIP))
	hasher.Write([]byte("|")) // Separator
	hasher.Write([]byte(userAgent))
	return hex.EncodeToString(hasher.Sum(nil))
}

// getClientIP extracts the client's real IP address, respecting common proxy headers.
func getClientIP(req *http.Request) string {
	// 1. X-Forwarded-For: Can be a comma-separated list. The first one is often the original client.
	xff := req.Header.Get("X-Forwarded-For")
	if xff != "" {
		ips := strings.Split(xff, ",")
		firstIP := strings.TrimSpace(ips[0])
		if firstIP != "" {
			return firstIP
		}
	}

	// 2. X-Real-IP: Typically a single IP.
	xrip := req.Header.Get("X-Real-IP")
	if xrip != "" {
		return strings.TrimSpace(xrip)
	}

	// 3. Fly-Client-IP: Header used by Fly.io
	flyIP := req.Header.Get("Fly-Client-IP")
	if flyIP != "" {
		return strings.TrimSpace(flyIP)
	}

	// 4. CF-Connecting-IP: Header used by Cloudflare
	cfIP := req.Header.Get("CF-Connecting-IP")
	if cfIP != "" {
		return strings.TrimSpace(cfIP)
	}

	// 5. Fallback to RemoteAddr: This might be the IP of the immediate upstream proxy.
	// RemoteAddr is typically in "ip:port" format.
	remoteAddr := req.RemoteAddr
	host, _, err := net.SplitHostPort(remoteAddr) // net is not imported, need to import "net"
	if err == nil && host != "" {
		return host
	}

	// If SplitHostPort fails (e.g. RemoteAddr is just an IP), return RemoteAddr as is.
	return remoteAddr
}

// serveQueuePage renders and serves the queue page HTML to the client.
func (qm *QueueManager) serveQueuePage(rw http.ResponseWriter, req *http.Request, positionInQueue int) {
	pageData := qm.prepareQueuePageData(positionInQueue)

	// Set headers to prevent caching of the queue page
	rw.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0, post-check=0, pre-check=0")
	rw.Header().Set("Pragma", "no-cache")
	rw.Header().Set("Expires", "Thu, 01 Jan 1970 00:00:00 GMT") // Classic "don't cache"

	currentTpl := qm.getTemplate()
	if currentTpl == nil {
		// Attempt to reload template if it was nil (e.g., initial load failed or file changed)
		qm.logf("debug", "Queue page template is nil, attempting to reload from: %s", qm.config.QueuePageFile)
		if err := qm.loadTemplate(); err != nil {
			qm.logf("warn", "Failed to load template '%s' on demand: %v. Serving fallback.", qm.config.QueuePageFile, err)
		}
		currentTpl = qm.getTemplate() // Get again after attempting load
	}

	if currentTpl != nil {
		rw.Header().Set("Content-Type", qm.config.HTTPContentType)
		rw.WriteHeader(qm.config.HTTPResponseCode)
		if err := currentTpl.Execute(rw, pageData); err != nil {
			qm.logf("error", "Error executing custom queue page template: %v. Serving fallback.", err)
			// Fall through to serve fallback template if execution fails
		} else {
			qm.logf("debug", "Served custom queue page to client. Position (1-based): %d", pageData.Position)
			return // Successfully served custom template
		}
	}

	// Fallback to default internal template if custom template is not available or failed
	qm.logf("debug", "Serving fallback queue page. Position (1-based): %d", pageData.Position)
	qm.serveFallbackTemplate(rw, pageData)
}

// prepareQueuePageData calculates and prepares the data for rendering the queue page.
func (qm *QueueManager) prepareQueuePageData(positionInQueue int) QueuePageData {
	qm.mu.RLock() // Read lock for accessing queue size and active count
	queueSize := len(qm.queue)
	activeCount := len(qm.activeSessionIDs)
	qm.mu.RUnlock()

	// Estimated wait time logic (simplified: 0.3 to 0.7 minutes per person ahead)
	// This can be made more sophisticated if average service time is known.
	// Using a slightly variable factor to make it seem less static.
	waitFactor := 0.3 + (float64(positionInQueue%5) * 0.08) // Varies between 0.3 and 0.62
	rawEstimatedTime := float64(positionInQueue) * waitFactor // positionInQueue is 0-based here

	estimatedWaitTime := int(math.Max(float64(qm.config.MinWaitTimeMinutes), math.Ceil(rawEstimatedTime)))
	// If they are at position 0 and served queue page, it means capacity was full at that instant.
	// So, even at pos 0, show MinWaitTimeMinutes unless it's genuinely 0.
	if positionInQueue == 0 && estimatedWaitTime == 0 && qm.config.MinWaitTimeMinutes > 0 {
		estimatedWaitTime = qm.config.MinWaitTimeMinutes
	}

	// Calculate progress percentage
	progressPercentage := 0
	if queueSize > 0 {
		if positionInQueue >= queueSize { // Should not happen if pos is 0-based from len(queue)
			progressPercentage = 1 // At least 1% if considered in queue but pos is off
		} else {
			// Progress: (queueSize - (positionInQueue+1)) / queueSize * 100
			// Example: 5 in queue, you are pos 0 (1st). Progress: (5-1)/5 = 80%
			// Example: 5 in queue, you are pos 4 (5th). Progress: (5-5)/5 = 0% -> adjust to show some progress
			progress := float64(queueSize-(positionInQueue+1)) / float64(queueSize)
			progressPercentage = int(math.Round(progress * 100))
			if progressPercentage <= 0 && positionInQueue < queueSize { // If calculated to 0 or less but still in queue
				progressPercentage = 1 // Ensure at least 1%
			}
		}
	} else if positionInQueue == 0 { // No one in queue, and you are pos 0 (means you are about to get in or just missed)
		progressPercentage = 99 // Almost there
	}
	// Clamp percentage
	if progressPercentage > 100 {
		progressPercentage = 100
	}
	if progressPercentage < 0 {
		progressPercentage = 0
	}

	var debugInfo string
	if qm.config.Debug {
		debugInfo = fmt.Sprintf("Pos(0-based): %d, QSize: %d, Active: %d, Max: %d, RawWait: %.2f min, Factor: %.2f",
			positionInQueue, queueSize, activeCount, qm.config.MaxEntries, rawEstimatedTime, waitFactor)
	}

	return QueuePageData{
		Position:           positionInQueue + 1, // Display 1-based position
		QueueSize:          queueSize,
		EstimatedWaitTime:  estimatedWaitTime,
		RefreshInterval:    qm.config.RefreshIntervalSeconds,
		ProgressPercentage: progressPercentage,
		Message:            "Your request is important to us. Please wait, and you will be redirected automatically.",
		DebugInfo:          debugInfo,
	}
}

// serveFallbackTemplate provides a basic, hardcoded HTML queue page.
func (qm *QueueManager) serveFallbackTemplate(rw http.ResponseWriter, data QueuePageData) {
	// Minified and slightly improved fallback HTML
	fallbackHTML := `<!DOCTYPE html><html><head><title>Service Queue</title><meta http-equiv="refresh" content="[[.RefreshInterval]]"><meta name="viewport" content="width=device-width, initial-scale=1.0"><style>body{font-family:Arial,sans-serif;text-align:center;margin:20px;padding:0;background-color:#f4f4f4;color:#333;} .container{max-width:600px;margin:40px auto;padding:20px;background-color:white;border-radius:8px;box-shadow:0 2px 10px rgba(0,0,0,0.1);} h1{color:#2c3e50;margin-bottom:15px;} p{line-height:1.6;} .progress-container{width:100%;background-color:#e9ecef;border-radius:5px;margin:25px 0;overflow:hidden;} .progress-bar{height:24px;width:[[.ProgressPercentage]]%;background-color:#3498db;text-align:center;line-height:24px;color:white;font-weight:bold;transition:width .3s ease;} .info-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:15px;margin:20px 0;} .info-box{background-color:#f8f9fa;padding:15px;border-radius:5px;border-left:4px solid #3498db;} .info-box strong{display:block;margin-bottom:5px;color:#2c3e50;} .debug{font-size:0.85em;color:#7f8c8d;margin-top:20px;padding:10px;background-color:#ecf0f1;border-radius:4px;text-align:left;display:[[if .DebugInfo]]block[[else]]none[[end]];}</style></head><body><div class="container"><h1>You're in the Queue</h1><p>Our service is currently experiencing high demand. Please wait, and this page will refresh automatically.</p><div class="progress-container"><div class="progress-bar">[[.ProgressPercentage]]%</div></div><div class="info-grid"><div class="info-box"><strong>Your Position</strong>[[.Position]] / [[.QueueSize]]</div><div class="info-box"><strong>Est. Wait Time</strong>~[[.EstimatedWaitTime]] min(s)</div></div><p>[[.Message]]</p><p>This page will refresh in <span id="countdown">[[.RefreshInterval]]</span> seconds.</p><div class="debug"><strong>Debug Info:</strong> <pre>[[.DebugInfo]]</pre></div></div><script>let s=[[.RefreshInterval]];const e=document.getElementById("countdown");function n(){s--,e.textContent=s,s<=0&&window.location.reload(!0)}e&&setInterval(n,1e3);</script></body></html>`

	tmpl, err := template.New("FallbackQueuePage").Delims("[[", "]]").Parse(fallbackHTML)
	if err != nil {
		qm.logf("error", "FATAL: Could not parse internal fallback template: %v", err)
		http.Error(rw, "Service temporarily unavailable. Error Code: FBTPLP.", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", qm.config.HTTPContentType)
	rw.WriteHeader(qm.config.HTTPResponseCode)

	if execErr := tmpl.Execute(rw, data); execErr != nil {
		qm.logf("error", "Error executing fallback queue page template: %v", execErr)
		// Ultimate fallback to plain text if template execution fails
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8") // Ensure plain text
		fmt.Fprintf(rw, "You are %d of %d in queue. Estimated wait: ~%d min(s). Page will refresh in %d seconds.",
			data.Position, data.QueueSize, data.EstimatedWaitTime, data.RefreshInterval)
	}
}

// CleanupExpiredSessions handles eviction of expired sessions (inactive or past hard limit)
// and promotes clients from the queue if capacity allows.
// This function acquires the main lock (qm.mu).
func (qm *QueueManager) CleanupExpiredSessions() {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	now := time.Now()
	removedActiveCount := 0
	removedQueueCount := 0
	promotedCount := 0

	// 1. Check active sessions for expiry (inactivity via cache or hard limit)
	// Iterate over a copy of keys to allow modification of qm.activeSessionIDs
	currentActiveClientIDs := make([]string, 0, len(qm.activeSessionIDs))
	for id := range qm.activeSessionIDs {
		currentActiveClientIDs = append(currentActiveClientIDs, id)
	}

	for _, clientID := range currentActiveClientIDs {
		sessionData, foundInCache := qm.getSessionFromCache(clientID)
		shouldRemoveActive := false
		reason := ""

		if !foundInCache {
			// Session not in cache means it expired due to inactivity (cache's default TTL)
			shouldRemoveActive = true
			reason = "inactivity (expired from cache)"
		} else {
			// Session is in cache, check hard limit
			if qm.hardSessionLimitDur > 0 && now.After(sessionData.HardExpiryAt) {
				shouldRemoveActive = true
				reason = fmt.Sprintf("hard session limit (expiry: %v)", sessionData.HardExpiryAt)
				qm.cache.Delete(clientID) // Explicitly remove from cache if hard limit exceeded
			}
		}

		if shouldRemoveActive {
			qm.logf("info", "Removing client %s from active sessions due to %s.", clientID, reason)
			delete(qm.activeSessionIDs, clientID)
			removedActiveCount++
		}
	}

	// 2. Filter queue for inactive sessions (expired from cache)
	newQueue := make([]Session, 0, len(qm.queue))
	for _, sessionInQueue := range qm.queue {
		// Check if the queued session is still valid in the cache (not timed out by inactivity)
		cachedSessionData, foundInCache := qm.getSessionFromCache(sessionInQueue.ID)
		if foundInCache {
			// Update LastSeenAt to now for active check during this cleanup, and re-set in cache to refresh its timer
			cachedSessionData.LastSeenAt = now
			cachedSessionData.Position = len(newQueue) // Update position before appending
			qm.cache.Set(sessionInQueue.ID, cachedSessionData, qm.inactivityTimeoutDur)
			newQueue = append(newQueue, cachedSessionData)
		} else {
			qm.logf("info", "Removing client %s from queue due to inactivity (expired from cache).", sessionInQueue.ID)
			removedQueueCount++
		}
	}
	qm.queue = newQueue // Replace old queue with the filtered one

	// 3. Promote clients from queue if capacity is available
	for len(qm.activeSessionIDs) < qm.config.MaxEntries && len(qm.queue) > 0 {
		// Get the client from the front of the queue (FIFO)
		promotedClientSession := qm.queue[0]
		qm.queue = qm.queue[1:] // Dequeue

		qm.logf("info", "Promoting client %s from queue to active. Queue size now %d.", promotedClientSession.ID, len(qm.queue))

		// Update session data for activation
		promotedClientSession.LastSeenAt = now
		promotedClientSession.Position = -1 // Mark as not in queue
		if qm.hardSessionLimitDur > 0 {
			// Set/reset HardExpiryAt based on promotion time for fairness
			promotedClientSession.HardExpiryAt = now.Add(qm.hardSessionLimitDur)
			promotedClientSession.CreatedAt = now // Consider promotion time as new "creation" for hard limit
		}

		qm.activeSessionIDs[promotedClientSession.ID] = true
		qm.cache.Set(promotedClientSession.ID, promotedClientSession, qm.inactivityTimeoutDur)
		promotedCount++
	}

	// 4. After promotions, re-index positions for the remaining items in the queue
	for i := range qm.queue {
		qm.queue[i].Position = i
		// Optionally update cache for these items if position is critical there,
		// but it's mainly for display from this function's perspective.
		if s, found := qm.getSessionFromCache(qm.queue[i].ID); found {
			s.Position = i
			qm.cache.Set(s.ID, s, qm.inactivityTimeoutDur) // Keep it fresh in cache
		}
	}

	if removedActiveCount > 0 || removedQueueCount > 0 || promotedCount > 0 {
		qm.logf("info", "Cleanup results: Removed Active: %d, Removed Queued: %d, Promoted: %d. Current State -> Active: %d, Queue: %d",
			removedActiveCount, removedQueueCount, promotedCount, len(qm.activeSessionIDs), len(qm.queue))
	} else {
		qm.logf("debug", "Cleanup run: No significant changes. Active: %d, Queue: %d", len(qm.activeSessionIDs), len(qm.queue))
	}
}

// dequeueClientUnderLock removes a client from the queue by ID.
// IMPORTANT: This helper assumes qm.mu is already write-locked by the caller.
func (qm *QueueManager) dequeueClientUnderLock(clientID string) {
	foundIndex := -1
	for i, session := range qm.queue {
		if session.ID == clientID {
			foundIndex = i
			break
		}
	}

	if foundIndex != -1 {
		// Remove the element by slicing: qm.queue = append(qm.queue[:foundIndex], qm.queue[foundIndex+1:]...)
		// More explicit copy for clarity if needed, but above is idiomatic.
		copy(qm.queue[foundIndex:], qm.queue[foundIndex+1:]) // Shift elements left
		qm.queue = qm.queue[:len(qm.queue)-1]             // Truncate slice

		qm.logf("debug", "Client %s dequeued (under lock).", clientID)

		// Re-index positions for remaining items in the queue
		for i := range qm.queue {
			qm.queue[i].Position = i
			// Optionally update cache for these items if their position needs to be reflected there.
			// For now, position in cache is mostly for debug or if session is re-fetched.
			if s, found := qm.getSessionFromCache(qm.queue[i].ID); found {
				s.Position = i
				qm.cache.Set(s.ID, s, qm.inactivityTimeoutDur)
			}
		}
	}
}

// fileExists checks if a file exists at the given path.
func fileExists(filePath string) bool {
	info, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		return false
	}
	return err == nil && !info.IsDir() // Ensure it's a file, not a directory
}

// Stop signals the cleanup goroutine to terminate and cleans up resources.
// Note: Traefik plugins don't have a formal "Stop" or "Shutdown" lifecycle method
// that's guaranteed to be called. This is for potential use in other contexts or future Traefik enhancements.
func (qm *QueueManager) Stop() {
	qm.logf("info", "QueueManager plugin stopping...")
	if qm.stopCleanup != nil {
		// Non-blocking send in case channel is already closed or receiver is gone
		select {
		case qm.stopCleanup <- true:
			qm.logf("debug", "Stop signal sent to cleanup routine.")
		default:
			qm.logf("warn", "Could not send stop signal to cleanup routine (already stopped or not running?).")
		}
		close(qm.stopCleanup) // Close channel to prevent further sends
	}

	if qm.cache != nil {
		qm.cache.Stop() // Stop the cache's internal cleanup timer
		qm.logf("debug", "Cache cleanup routine signaled to stop.")
	}

	// Close log file if it was opened and stored
	if qm.logFileHandle != nil {
		qm.logf("info", "Closing log file: %s", qm.config.LogFile)
		err := qm.logFileHandle.Close()
		if err != nil {
			// Log to stderr if log file closing fails, as qm.logger might be using the file.
			log.Printf(logPrefix+"ERROR: Failed to close log file %s: %v", qm.config.LogFile, err)
		}
		qm.logFileHandle = nil // Avoid double closing
	}
	qm.logf("info", "QueueManager plugin has been signaled to stop.")
}
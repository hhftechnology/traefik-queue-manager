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
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds the plugin configuration.
type Config struct {
	Enabled          bool   `json:"enabled"`           // Enable/disable the queue manager
	QueuePageFile    string `json:"queuePageFile"`     // Path to queue page HTML template
	SessionTime      int    `json:"sessionTime"`       // How long a session is valid for (in seconds)
	PurgeTime        int    `json:"purgeTime"`         // How often to purge expired sessions (in seconds)
	MaxEntries       int    `json:"maxEntries"`        // Maximum concurrent users
	HTTPResponseCode int    `json:"httpResponseCode"`  // HTTP response code for queue page
	HTTPContentType  string `json:"httpContentType"`   // Content type of queue page
	UseCookies       bool   `json:"useCookies"`        // Use cookies or IP+UserAgent hash
	CookieName       string `json:"cookieName"`        // Name of the cookie
	CookieMaxAge     int    `json:"cookieMaxAge"`      // Max age of the cookie in seconds
	QueueStrategy    string `json:"queueStrategy"`     // Queue strategy: "fifo" or "random"
	RefreshInterval  int    `json:"refreshInterval"`   // Refresh interval in seconds
	Debug            bool   `json:"debug"`             // Enable debug logging
	MinWaitTime      int    `json:"minWaitTime"`       // Minimum wait time to show users (in minutes)
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		Enabled:          true,
		QueuePageFile:    "queue-page.html",
		SessionTime:      60,    // 1 minute in seconds
		PurgeTime:        300,   // 5 minutes in seconds
		MaxEntries:       100,
		HTTPResponseCode: http.StatusTooManyRequests,
		HTTPContentType:  "text/html; charset=utf-8",
		UseCookies:       true,
		CookieName:       "queue-manager-id",
		CookieMaxAge:     3600,
		QueueStrategy:    "fifo",
		RefreshInterval:  30,
		Debug:            false,
		MinWaitTime:      1,     // Minimum wait time to show in minutes
	}
}

// Session represents a visitor session.
type Session struct {
	ID        string    `json:"id"`        // Unique client identifier
	CreatedAt time.Time `json:"createdAt"` // When the session was created
	LastSeen  time.Time `json:"lastSeen"`  // When the client was last seen
	Position  int       `json:"position"`  // Position in the queue
}

// QueuePageData contains data to be passed to the HTML template.
type QueuePageData struct {
	Position           int    `json:"position"`           // Position in queue
	QueueSize          int    `json:"queueSize"`          // Total queue size
	EstimatedWaitTime  int    `json:"estimatedWaitTime"`  // Estimated wait time in minutes
	RefreshInterval    int    `json:"refreshInterval"`    // Refresh interval in seconds
	ProgressPercentage int    `json:"progressPercentage"` // Progress percentage
	Message            string `json:"message"`            // Custom message
	Debug              string `json:"debug"`              // Debug information (only shown if debug mode enabled)
}

// QueueManager is the middleware handler.
type QueueManager struct {
	next             http.Handler
	name             string
	config           *Config
	cache            *SimpleCache
	template         *template.Template
	queue            []Session
	activeSessionIDs map[string]bool
	mu               sync.RWMutex            // Mutex for thread safety
	cleanupTicker    *time.Ticker            // Ticker for cleanup routine
	stopCleaner      chan bool               // Channel to stop the cleanup routine
	sessionTimeDur   time.Duration           // Session time as duration
	purgeTimeDur     time.Duration           // Purge time as duration
}

// New creates a new queue manager middleware.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	// Validate configuration
	if config.MaxEntries <= 0 {
		return nil, fmt.Errorf("maxEntries must be greater than 0")
	}

	if len(config.QueuePageFile) == 0 {
		return nil, fmt.Errorf("queuePageFile cannot be empty")
	}
	
	// Convert second-based configuration to durations
	sessionTimeDur := time.Duration(config.SessionTime) * time.Second
	purgeTimeDur := time.Duration(config.PurgeTime) * time.Second
	
	if config.Debug {
		log.Printf("[Queue Manager] Using sessionTime: %v, purgeTime: %v", sessionTimeDur, purgeTimeDur)
	}

	// Create queue manager
	qm := &QueueManager{
		next:             next,
		name:             name,
		config:           config,
		cache:            NewSimpleCache(sessionTimeDur, purgeTimeDur),
		queue:            make([]Session, 0),
		activeSessionIDs: make(map[string]bool),
		mu:               sync.RWMutex{},
		stopCleaner:      make(chan bool),
		sessionTimeDur:   sessionTimeDur,
		purgeTimeDur:     purgeTimeDur,
	}
	
	// Start a goroutine for periodic cleanup if purge time is positive
	if purgeTimeDur > 0 {
		qm.startCleanupRoutine(purgeTimeDur)
	}
	
	// Try to load and parse the template (but don't fail if it can't be found yet)
	// This allows the plugin to start even if the template will be mounted later
	if fileExists(config.QueuePageFile) {
		tmplContent, err := os.ReadFile(config.QueuePageFile)
		if err == nil {
			qm.template, _ = template.New("QueuePage").Delims("[[", "]]").Parse(string(tmplContent))
		}
	}

	// Log configuration in debug mode
	if config.Debug {
		log.Printf("[Queue Manager] Configuration: %+v", config)
		log.Printf("[Queue Manager] Template file exists: %v", fileExists(config.QueuePageFile))
	}

	return qm, nil
}

// startCleanupRoutine starts a background goroutine to clean up expired sessions
func (qm *QueueManager) startCleanupRoutine(interval time.Duration) {
	qm.cleanupTicker = time.NewTicker(interval)
	
	go func() {
		for {
			select {
			case <-qm.cleanupTicker.C:
				qm.CleanupExpiredSessions()
			case <-qm.stopCleaner:
				qm.cleanupTicker.Stop()
				return
			}
		}
	}()
	
	if qm.config.Debug {
		log.Printf("[Queue Manager] Started cleanup routine with interval: %v", interval)
	}
}

// ServeHTTP implements the http.Handler interface.
func (qm *QueueManager) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Skip if disabled
	if !qm.config.Enabled {
		qm.next.ServeHTTP(rw, req)
		return
	}

	// Get or create client ID
	clientID, isNewClient := qm.getClientID(rw, req)
	
	// Debug logging
	if qm.config.Debug {
		log.Printf("[Queue Manager] Client ID: %s, New client: %t", clientID, isNewClient)
		
		qm.mu.RLock()
		activeCount := len(qm.activeSessionIDs)
		queueSize := len(qm.queue)
		qm.mu.RUnlock()
		
		log.Printf("[Queue Manager] Active sessions: %d, Queue size: %d, Max entries: %d", 
			activeCount, queueSize, qm.config.MaxEntries)
	}

	// Check if the client can proceed directly
	if qm.canClientProceed(clientID) {
		// Client is allowed to access the service
		if qm.config.Debug {
			log.Printf("[Queue Manager] Client %s allowed to proceed", clientID)
		}
		qm.next.ServeHTTP(rw, req)
		return
	}

	// Handle client that needs to wait
	position := qm.placeClientInQueue(clientID)

	// Serve queue page
	qm.serveQueuePage(rw, req, position)
}

// canClientProceed checks if a client can proceed without waiting.
func (qm *QueueManager) canClientProceed(clientID string) bool {
	qm.mu.RLock()
	// Fast path: check if already active
	if qm.activeSessionIDs[clientID] {
		qm.mu.RUnlock()
		qm.updateClientTimestamp(clientID)
		return true
	}
	
	// Check if there's room for more sessions
	hasCapacity := len(qm.activeSessionIDs) < qm.config.MaxEntries
	qm.mu.RUnlock()
	
	if hasCapacity {
		// There's room, add the client to active sessions
		session := Session{
			ID:        clientID,
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
			Position:  0,
		}
		
		qm.mu.Lock()
		// Double-check capacity (might have changed since we checked)
		if len(qm.activeSessionIDs) < qm.config.MaxEntries {
			qm.activeSessionIDs[clientID] = true
			qm.mu.Unlock()
			qm.cache.Set(clientID, session, DefaultExpiration)
			return true
		}
		qm.mu.Unlock()
	}

	// No capacity available
	return false
}

// updateClientTimestamp updates the last seen timestamp for a client.
func (qm *QueueManager) updateClientTimestamp(clientID string) {
	if sessionObj, found := qm.cache.Get(clientID); found {
		sessionData, ok := sessionObj.(Session)
		if !ok {
			if qm.config.Debug {
				log.Printf("[Queue Manager] Error: Failed to convert session to Session type")
			}
			return
		}
		
		sessionData.LastSeen = time.Now()
		qm.cache.Set(clientID, sessionData, DefaultExpiration)
	}
}

// placeClientInQueue places a client in the waiting queue and returns their position.
func (qm *QueueManager) placeClientInQueue(clientID string) int {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	
	// Check if they're already in the queue
	position := -1
	for i, session := range qm.queue {
		if session.ID == clientID {
			position = i
			break
		}
	}

	// If not in queue, add them
	if position == -1 {
		session := Session{
			ID:        clientID,
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
			Position:  len(qm.queue),
		}
		qm.queue = append(qm.queue, session)
		qm.cache.Set(clientID, session, DefaultExpiration)
		position = len(qm.queue) - 1
		
		if qm.config.Debug {
			log.Printf("[Queue Manager] Added client %s to queue at position %d", clientID, position)
		}
	} else {
		// Update last seen timestamp for existing queue entry
		qm.updateClientTimestamp(clientID)
		
		if qm.config.Debug {
			log.Printf("[Queue Manager] Client %s already in queue at position %d", clientID, position)
		}
	}
	
	return position
}

// getClientID generates or retrieves a unique client identifier.
func (qm *QueueManager) getClientID(rw http.ResponseWriter, req *http.Request) (string, bool) {
	if qm.config.UseCookies {
		// Try to get existing cookie
		cookie, err := req.Cookie(qm.config.CookieName)
		if err == nil && cookie.Value != "" {
			return cookie.Value, false
		}
		
		// Create new cookie
		newID := generateUniqueID(req)
		http.SetCookie(rw, &http.Cookie{
			Name:     qm.config.CookieName,
			Value:    newID,
			Path:     "/",
			MaxAge:   qm.config.CookieMaxAge,
			HttpOnly: true,
			Secure:   req.TLS != nil,
			SameSite: http.SameSiteLaxMode,
		})
		return newID, true
	}
	
	// Use IP + UserAgent hash
	return generateClientHash(req), false
}

// generateUniqueID creates a unique identifier for a client.
func generateUniqueID(req *http.Request) string {
	// Create a buffer for true randomness
	randBytes := make([]byte, 16)
	_, err := rand.Read(randBytes)
	if err != nil {
		// If crypto/rand fails, use a fallback method
		timestamp := time.Now().UnixNano()
		for i := range randBytes {
			randBytes[i] = byte((timestamp + int64(i)) % 256)
		}
	}
	
	// Add client IP to the randomness
	clientIP := getClientIP(req)
	
	// Create a hash of the random bytes + IP
	hasher := sha256.New()
	hasher.Write(randBytes)
	hasher.Write([]byte(clientIP))
	hasher.Write([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	
	randHash := hex.EncodeToString(hasher.Sum(nil))[:16]
	
	// Format: timestamp-ip-randomhash
	return fmt.Sprintf("%d-%s-%s", time.Now().UnixNano(), clientIP, randHash)
}

// generateClientHash creates a hash from client attributes.
func generateClientHash(req *http.Request) string {
	// Get client IP
	clientIP := getClientIP(req)
	
	// Get user agent
	userAgent := req.UserAgent()
	
	// Create hash
	hasher := sha256.New()
	hasher.Write([]byte(clientIP + "|" + userAgent))
	return hex.EncodeToString(hasher.Sum(nil))[:32]
}

// getClientIP extracts the client's real IP address.
func getClientIP(req *http.Request) string {
	// Check for X-Forwarded-For header
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	
	// Check for X-Real-IP header
	if xrip := req.Header.Get("X-Real-IP"); xrip != "" {
		return xrip
	}
	
	// Fall back to RemoteAddr
	remoteAddr := req.RemoteAddr
	ipPort := strings.Split(remoteAddr, ":")
	if len(ipPort) > 0 {
		return ipPort[0]
	}
	
	// In case RemoteAddr is in an unexpected format
	return remoteAddr
}

// serveQueuePage serves the queue page HTML.
func (qm *QueueManager) serveQueuePage(rw http.ResponseWriter, req *http.Request, position int) {
	// Prepare template data
	data := qm.prepareQueuePageData(position)
	
	// Set cache control headers to prevent caching
	rw.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0, post-check=0, pre-check=0")
	rw.Header().Set("Pragma", "no-cache")
	rw.Header().Set("Expires", "Thu, 01 Jan 1970 00:00:00 GMT")
	
	// Try to use the template file
	if qm.serveCustomTemplate(rw, data) {
		return
	}
	
	// Fall back to the default template
	qm.serveFallbackTemplate(rw, data)
}

// prepareQueuePageData creates the data structure for the queue page template.
func (qm *QueueManager) prepareQueuePageData(position int) QueuePageData {
	qm.mu.RLock()
	queueSize := len(qm.queue)
	activeCount := len(qm.activeSessionIDs)
	qm.mu.RUnlock()
	
	// Calculate estimated wait time - with a more realistic model
	// Base calculation: position * 0.5 minutes per person ahead in queue
	rawEstimatedTime := float64(position) * 0.5
	
	// Apply minimum wait time (unless position is 0 and there's capacity)
	var estimatedWaitTime int
	if position == 0 && activeCount < qm.config.MaxEntries {
		estimatedWaitTime = 0 // Immediate access available
	} else {
		// Use maximum of calculated wait time and minimum configured wait time
		estimatedWaitTime = int(math.Max(float64(qm.config.MinWaitTime), math.Ceil(rawEstimatedTime)))
	}
	
	// Calculate progress percentage - protect against division by zero
	progressPercentage := 0
	if queueSize > 0 {
		// Calculate progress - inverted from position/queueSize
		// The lower the position, the higher the progress
		if position == 0 {
			progressPercentage = 100 // First in queue = 100% progress
		} else {
			// Calculate progress: 1 - (position / queueSize) then convert to percentage
			progressPercentage = int(math.Round((1 - float64(position)/float64(queueSize)) * 100))
			
			// Ensure progress is at least 1% for anyone in the queue
			if progressPercentage < 1 {
				progressPercentage = 1
			}
		}
	}
	
	// Prepare debug info if debug mode is enabled
	var debugInfo string
	if qm.config.Debug {
		debugInfo = fmt.Sprintf("Position: %d, Queue Size: %d, Active Sessions: %d, Raw Wait Time: %.2f min",
			position, queueSize, activeCount, rawEstimatedTime)
		log.Printf("[Queue Manager] %s", debugInfo)
	}
	
	return QueuePageData{
		Position:           position + 1, // 1-based position for users
		QueueSize:          queueSize,
		EstimatedWaitTime:  estimatedWaitTime,
		RefreshInterval:    qm.config.RefreshInterval,
		ProgressPercentage: progressPercentage,
		Message:            "Please wait while we process your request.",
		Debug:              debugInfo,
	}
}

// serveCustomTemplate attempts to serve the custom queue page template.
// Returns true if successful, false if the template could not be served.
func (qm *QueueManager) serveCustomTemplate(rw http.ResponseWriter, data QueuePageData) bool {
	if !fileExists(qm.config.QueuePageFile) {
		if qm.config.Debug {
			log.Printf("[Queue Manager] Template file not found: %s", qm.config.QueuePageFile)
		}
		return false
	}
	
	// Try to load the template if we haven't successfully loaded it yet
	if qm.template == nil {
		content, err := os.ReadFile(qm.config.QueuePageFile)
		if err != nil {
			if qm.config.Debug {
				log.Printf("[Queue Manager] Error reading template file: %v", err)
			}
			return false
		}
		
		queueTemplate, parseErr := template.New("QueuePage").Delims("[[", "]]").Parse(string(content))
		if parseErr != nil {
			if qm.config.Debug {
				log.Printf("[Queue Manager] Error parsing template: %v", parseErr)
			}
			return false
		}
		
		qm.template = queueTemplate
	}
	
	// Set content type and status code
	rw.Header().Set("Content-Type", qm.config.HTTPContentType)
	rw.WriteHeader(qm.config.HTTPResponseCode)
	
	// Execute template
	if execErr := qm.template.Execute(rw, data); execErr != nil {
		if qm.config.Debug {
			log.Printf("[Queue Manager] Error executing template: %v", execErr)
		}
		return false
	}
	
	return true
}

// serveFallbackTemplate serves the built-in default queue page template.
func (qm *QueueManager) serveFallbackTemplate(rw http.ResponseWriter, data QueuePageData) {
	fallbackTemplate := `<!DOCTYPE html>
<html>
<head>
    <title>Service Queue</title>
    <meta http-equiv="refresh" content="[[.RefreshInterval]]">
    <style>
        body {
            font-family: Arial, sans-serif;
            text-align: center;
            margin-top: 50px;
            background-color: #f8f9fa;
        }
        .container {
            max-width: 600px;
            margin: 0 auto;
            padding: 20px;
            border: 1px solid #ddd;
            border-radius: 5px;
            background-color: white;
            box-shadow: 0 2px 10px rgba(0,0,0,0.1);
        }
        .progress {
            margin: 20px 0;
            height: 20px;
            background-color: #f5f5f5;
            border-radius: 4px;
            overflow: hidden;
        }
        .progress-bar {
            height: 100%;
            background-color: #4CAF50;
            text-align: center;
            line-height: 20px;
            color: white;
        }
        .info-grid {
            display: grid;
            grid-template-columns: 1fr 1fr;
            gap: 15px;
            margin: 20px 0;
            text-align: left;
        }
        .info-box {
            background-color: #f0f4f8;
            padding: 15px;
            border-radius: 8px;
        }
        .debug-info {
            margin-top: 20px;
            font-size: 0.8em;
            color: #999;
            display: [[if .Debug]]block[[else]]none[[end]];
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>You're in the queue</h1>
        <p>Our service is currently at capacity. Please wait and you'll be automatically redirected when space becomes available.</p>
        
        <div class="progress">
            <div class="progress-bar" style="width: [[.ProgressPercentage]]%">
                [[.ProgressPercentage]]%
            </div>
        </div>
        
        <div class="info-grid">
            <div class="info-box">
                <h3>Your position in queue</h3>
                <p><strong>[[.Position]]</strong> of [[.QueueSize]]</p>
            </div>
            <div class="info-box">
                <h3>Estimated wait time</h3>
                <p><strong>[[.EstimatedWaitTime]]</strong> minutes</p>
            </div>
        </div>
        
        <p>[[.Message]]</p>
        <p>This page will refresh automatically in [[.RefreshInterval]] seconds.</p>
        
        <div class="debug-info">
            Debug: [[.Debug]]
        </div>
    </div>
    
    <!-- JavaScript fallback for refresh -->
    <script>
        // Ensure page refreshes even if meta tag fails
        setTimeout(function() {
            window.location.reload();
        }, [[.RefreshInterval]] * 1000);
    </script>
</body>
</html>`
	
	// Create and execute the fallback template
	tmpl, _ := template.New("FallbackQueuePage").Delims("[[", "]]").Parse(fallbackTemplate)
	rw.Header().Set("Content-Type", qm.config.HTTPContentType)
	rw.WriteHeader(qm.config.HTTPResponseCode)
	if err := tmpl.Execute(rw, data); err != nil {
		if qm.config.Debug {
			log.Printf("[Queue Manager] Error executing fallback template: %v", err)
		}
		// If template execution fails, return a basic response
		fmt.Fprintf(rw, "You are in position %d of %d in the queue. Estimated wait: %d minutes. This page will refresh in %d seconds.",
			data.Position, data.QueueSize, data.EstimatedWaitTime, data.RefreshInterval)
	}
}

// CleanupExpiredSessions periodically checks and removes expired sessions.
func (qm *QueueManager) CleanupExpiredSessions() {
	if qm.config.Debug {
		log.Printf("[Queue Manager] Running cleanup routine")
	}
	
	qm.mu.Lock()
	defer qm.mu.Unlock()
	
	// Track removed sessions for logging
	var removedActive, removedQueue int
	
	// Check if any active sessions have expired
	for id := range qm.activeSessionIDs {
		if _, found := qm.cache.Get(id); !found {
			delete(qm.activeSessionIDs, id)
			removedActive++
		}
	}
	
	// Filter out expired sessions from queue and update positions
	newQueue := make([]Session, 0, len(qm.queue))
	for _, session := range qm.queue {
		if _, found := qm.cache.Get(session.ID); found {
			newQueue = append(newQueue, session)
		} else {
			removedQueue++
		}
	}
	qm.queue = newQueue
	
	// Promote clients from queue to active sessions if there's room
	var promoted int
	for len(qm.activeSessionIDs) < qm.config.MaxEntries && len(qm.queue) > 0 {
		// Get next client in queue
		nextClient := qm.queue[0]
		
		// Promote to active session
		qm.activeSessionIDs[nextClient.ID] = true
		
		// Remove from queue
		qm.queue = qm.queue[1:]
		promoted++
	}
	
	// Update positions in queue
	for i := range qm.queue {
		if sessionObj, found := qm.cache.Get(qm.queue[i].ID); found {
			sessionData, ok := sessionObj.(Session)
			if ok {
				sessionData.Position = i
				qm.cache.Set(qm.queue[i].ID, sessionData, DefaultExpiration)
			}
		}
	}
	
	if qm.config.Debug && (removedActive > 0 || removedQueue > 0 || promoted > 0) {
		log.Printf("[Queue Manager] Cleanup results: Removed %d active sessions, %d queued sessions. Promoted %d clients.",
			removedActive, removedQueue, promoted)
		log.Printf("[Queue Manager] Current state: %d active sessions, %d in queue", 
			len(qm.activeSessionIDs), len(qm.queue))
	}
}

// fileExists checks if a file exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
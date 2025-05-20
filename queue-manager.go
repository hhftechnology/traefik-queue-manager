package traefik_queue_manager

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
)

// Config holds the plugin configuration
type Config struct {
	Enabled          bool          `json:"enabled"`           // Enable/disable the queue manager
	QueuePageFile    string        `json:"queuePageFile"`     // Path to queue page HTML template
	SessionTime      time.Duration `json:"sessionTime"`       // How long a session is valid for
	PurgeTime        time.Duration `json:"purgeTime"`         // How often to purge expired sessions
	MaxEntries       int           `json:"maxEntries"`        // Maximum concurrent users
	HttpResponseCode int           `json:"httpResponseCode"`  // HTTP response code for queue page
	HttpContentType  string        `json:"httpContentType"`   // Content type of queue page
	UseCookies       bool          `json:"useCookies"`        // Use cookies or IP+UserAgent hash
	CookieName       string        `json:"cookieName"`        // Name of the cookie
	CookieMaxAge     int           `json:"cookieMaxAge"`      // Max age of the cookie in seconds
	QueueStrategy    string        `json:"queueStrategy"`     // Queue strategy: "fifo" or "random"
	RefreshInterval  int           `json:"refreshInterval"`   // Refresh interval in seconds
	Debug            bool          `json:"debug"`             // Enable debug logging
}

// CreateConfig creates the default plugin configuration
func CreateConfig() *Config {
	return &Config{
		Enabled:          true,
		QueuePageFile:    "queue-page.html",
		SessionTime:      1 * time.Minute,
		PurgeTime:        5 * time.Minute,
		MaxEntries:       100,
		HttpResponseCode: http.StatusTooManyRequests,
		HttpContentType:  "text/html; charset=utf-8",
		UseCookies:       true,
		CookieName:       "queue-manager-id",
		CookieMaxAge:     3600,
		QueueStrategy:    "fifo",
		RefreshInterval:  30,
		Debug:            false,
	}
}

// Session represents a visitor session
type Session struct {
	ID        string    `json:"id"`        // Unique client identifier
	CreatedAt time.Time `json:"createdAt"` // When the session was created
	LastSeen  time.Time `json:"lastSeen"`  // When the client was last seen
	Position  int       `json:"position"`  // Position in the queue
}

// QueuePageData contains data to be passed to the HTML template
type QueuePageData struct {
	Position           int    `json:"position"`           // Position in queue
	QueueSize          int    `json:"queueSize"`          // Total queue size
	EstimatedWaitTime  int    `json:"estimatedWaitTime"`  // Estimated wait time in minutes
	RefreshInterval    int    `json:"refreshInterval"`    // Refresh interval in seconds
	ProgressPercentage int    `json:"progressPercentage"` // Progress percentage
	Message            string `json:"message"`            // Custom message
}

// QueueManager is the middleware handler
type QueueManager struct {
	next             http.Handler
	name             string
	config           *Config
	cache            *cache.Cache
	template         *template.Template
	queue            []Session
	activeSessionIDs map[string]bool
}

// New creates a new queue manager middleware
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	// Validate configuration
	if config.MaxEntries <= 0 {
		return nil, fmt.Errorf("maxEntries must be greater than 0")
	}

	if len(config.QueuePageFile) == 0 {
		return nil, fmt.Errorf("queuePageFile cannot be empty")
	}

	// Parse HTML template
	tmpl, err := template.New("QueuePage").Delims("[[", "]]").ParseFiles(config.QueuePageFile)
	if err != nil {
		// Try to use the default template if the file doesn't exist or can't be parsed
		tmpl = template.New("QueuePage").Delims("[[", "]]")
	}

	// Log configuration in debug mode
	if config.Debug {
		log.Printf("[Queue Manager] Configuration: %+v", config)
	}

	return &QueueManager{
		next:             next,
		name:             name,
		config:           config,
		cache:            cache.New(config.SessionTime, config.PurgeTime),
		template:         tmpl,
		queue:            make([]Session, 0),
		activeSessionIDs: make(map[string]bool),
	}, nil
}

// ServeHTTP implements the http.Handler interface
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
		log.Printf("[Queue Manager] Active sessions: %d, Max entries: %d", len(qm.activeSessionIDs), qm.config.MaxEntries)
	}

	// Check if client is already in active sessions
	if qm.activeSessionIDs[clientID] {
		// Update last seen timestamp
		if session, found := qm.cache.Get(clientID); found {
			sessionData := session.(Session)
			sessionData.LastSeen = time.Now()
			qm.cache.Set(clientID, sessionData, cache.DefaultExpiration)
		}
		
		// Allow access
		qm.next.ServeHTTP(rw, req)
		return
	}

	// If there's room for more active sessions, add this client
	if len(qm.activeSessionIDs) < qm.config.MaxEntries {
		// Create new session
		session := Session{
			ID:        clientID,
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
			Position:  0,
		}
		
		// Add to active sessions
		qm.activeSessionIDs[clientID] = true
		qm.cache.Set(clientID, session, cache.DefaultExpiration)
		
		// Allow access
		qm.next.ServeHTTP(rw, req)
		return
	}

	// At this point, we know the client needs to wait
	// Let's check if they're already in the queue
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
		qm.cache.Set(clientID, session, cache.DefaultExpiration)
		position = len(qm.queue) - 1
	}

	// Update last seen timestamp
	if session, found := qm.cache.Get(clientID); found {
		sessionData := session.(Session)
		sessionData.LastSeen = time.Now()
		qm.cache.Set(clientID, sessionData, cache.DefaultExpiration)
	}

	// Serve queue page
	qm.serveQueuePage(rw, req, position)
}

// getClientID generates or retrieves a unique client identifier
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
	} else {
		// Use IP + UserAgent hash
		return generateClientHash(req), false
	}
}

// generateUniqueID creates a unique identifier for a client
func generateUniqueID(req *http.Request) string {
	// Create unique ID based on current time, remote IP, and some randomness
	timestamp := time.Now().UnixNano()
	clientIP := getClientIP(req)
	return fmt.Sprintf("%d-%s-%d", timestamp, clientIP, timestamp%1000)
}

// generateClientHash creates a hash from client attributes
func generateClientHash(req *http.Request) string {
	// Get client IP
	clientIP := getClientIP(req)
	
	// Get user agent
	userAgent := req.UserAgent()
	
	// Create hash
	hasher := md5.New()
	hasher.Write([]byte(clientIP + "|" + userAgent))
	return hex.EncodeToString(hasher.Sum(nil))
}

// getClientIP extracts the client's real IP address
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

// serveQueuePage serves the queue page HTML
func (qm *QueueManager) serveQueuePage(rw http.ResponseWriter, req *http.Request, position int) {
	// Calculate estimated wait time (rough estimate: 30 seconds per position)
	estimatedWaitTime := int(math.Ceil(float64(position) * 0.5)) // in minutes
	
	// Calculate progress percentage
	progressPercentage := 0
	if position > 0 && len(qm.queue) > 0 {
		progressPercentage = int(100 - (float64(position) / float64(len(qm.queue)) * 100))
	}
	
	// Prepare template data
	data := QueuePageData{
		Position:           position + 1, // 1-based position for users
		QueueSize:          len(qm.queue),
		EstimatedWaitTime:  estimatedWaitTime,
		RefreshInterval:    qm.config.RefreshInterval,
		ProgressPercentage: progressPercentage,
		Message:            "Please wait while we process your request.",
	}
	
	// Check if we have a template file
	if fileExists(qm.config.QueuePageFile) {
		// Read the file content
		content, err := os.ReadFile(qm.config.QueuePageFile)
		if err == nil {
			// Create a new template from the file content
			tmpl, err := template.New("QueuePage").Delims("[[", "]]").Parse(string(content))
			if err == nil {
				// Set content type
				rw.Header().Set("Content-Type", qm.config.HttpContentType)
				rw.WriteHeader(qm.config.HttpResponseCode)
				
				// Execute template
				err = tmpl.Execute(rw, data)
				if err != nil && qm.config.Debug {
					log.Printf("[Queue Manager] Error executing template: %v", err)
				}
				return
			} else if qm.config.Debug {
				log.Printf("[Queue Manager] Error parsing template: %v", err)
			}
		} else if qm.config.Debug {
			log.Printf("[Queue Manager] Error reading template file: %v", err)
		}
	}
	
	// Fall back to a simple built-in template if there's an issue with the file
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
        }
        .container {
            max-width: 600px;
            margin: 0 auto;
            padding: 20px;
            border: 1px solid #ddd;
            border-radius: 5px;
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
        
        <p>Your position in queue: [[.Position]] of [[.QueueSize]]</p>
        <p>Estimated wait time: [[.EstimatedWaitTime]] minutes</p>
        <p>This page will refresh automatically in [[.RefreshInterval]] seconds.</p>
    </div>
</body>
</html>`
	
	// Create and execute the fallback template
	tmpl, _ := template.New("FallbackQueuePage").Delims("[[", "]]").Parse(fallbackTemplate)
	rw.Header().Set("Content-Type", qm.config.HttpContentType)
	rw.WriteHeader(qm.config.HttpResponseCode)
	err := tmpl.Execute(rw, data)
	if err != nil && qm.config.Debug {
		log.Printf("[Queue Manager] Error executing fallback template: %v", err)
	}
}

// Periodically check and clean up expired sessions
func (qm *QueueManager) cleanupExpiredSessions() {
	// Check if any active sessions have expired
	for id := range qm.activeSessionIDs {
		if _, found := qm.cache.Get(id); !found {
			delete(qm.activeSessionIDs, id)
		}
	}
	
	// Promote clients from queue to active sessions if there's room
	for i := 0; i < len(qm.queue) && len(qm.activeSessionIDs) < qm.config.MaxEntries; i++ {
		session := qm.queue[i]
		
		// Check if session is still valid
		if _, found := qm.cache.Get(session.ID); found {
			// Promote to active session
			qm.activeSessionIDs[session.ID] = true
			
			// Remove from queue
			qm.queue = append(qm.queue[:i], qm.queue[i+1:]...)
			i-- // Adjust index after removal
		} else {
			// Session expired, remove from queue
			qm.queue = append(qm.queue[:i], qm.queue[i+1:]...)
			i-- // Adjust index after removal
		}
	}
	
	// Update positions in queue
	for i := range qm.queue {
		if session, found := qm.cache.Get(qm.queue[i].ID); found {
			sessionData := session.(Session)
			sessionData.Position = i
			qm.cache.Set(qm.queue[i].ID, sessionData, cache.DefaultExpiration)
		}
	}
}

// Helper function to check if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
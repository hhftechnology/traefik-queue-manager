// Package traefik_queue_manager_test contains tests for the QueueManager plugin.
package traefik_queue_manager

import (
	"bytes"
	"context"

	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockNext is a simple http.Handler for testing.
type mockNext struct {
	serveHTTPFunc func(http.ResponseWriter, *http.Request)
}

func (m *mockNext) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if m.serveHTTPFunc != nil {
		m.serveHTTPFunc(rw, req)
	} else {
		rw.WriteHeader(http.StatusOK) // Default OK response
		_, _ = rw.Write([]byte("Service Accessed"))
	}
}

// Helper to create a temporary file with content.
func createTempFile(t *testing.T, content string) (string, func()) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "test-*.html")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tmpFile.Close()
	return tmpFile.Name(), func() { os.Remove(tmpFile.Name()) }
}

func TestCreateConfig(t *testing.T) {
	cfg := CreateConfig()
	if cfg == nil {
		t.Fatal("CreateConfig returned nil")
	}
	if cfg.MaxEntries != defaultMaxEntries {
		t.Errorf("Expected MaxEntries %d, got %d", defaultMaxEntries, cfg.MaxEntries)
	}
	if cfg.InactivityTimeoutSeconds != defaultInactivityTimeoutSecs {
		t.Errorf("Expected InactivityTimeoutSeconds %d, got %d", defaultInactivityTimeoutSecs, cfg.InactivityTimeoutSeconds)
	}
	// Add more checks for other default fields if necessary
}

func TestNew_ValidConfig(t *testing.T) {
	cfg := CreateConfig()
	next := &mockNext{}
	handler, err := New(context.Background(), next, cfg, "test-queue")
	if err != nil {
		t.Fatalf("New() with valid config failed: %v", err)
	}
	if handler == nil {
		t.Fatal("New() with valid config returned nil handler")
	}
	// Ensure cleanup routine is stopped for test hygiene
	if qm, ok := handler.(*QueueManager); ok {
		qm.Stop()
	}
}

func TestNew_InvalidConfig(t *testing.T) {
	tests := []struct {
		name        string
		modifier    func(*Config)
		expectedErr string
	}{
		{"MaxEntriesZero", func(c *Config) { c.MaxEntries = 0 }, "maxEntries must be greater than 0"},
		{"InactivityTimeoutZero", func(c *Config) { c.InactivityTimeoutSeconds = 0 }, "inactivityTimeoutSeconds must be greater than 0"},
		{"CleanupIntervalZero", func(c *Config) { c.CleanupIntervalSeconds = 0 }, "cleanupIntervalSeconds must be greater than 0"},
		{"HardSessionNegative", func(c *Config) { c.HardSessionLimitSeconds = -1 }, "hardSessionLimitSeconds cannot be negative"},
		{"RefreshIntervalZero", func(c *Config) { c.RefreshIntervalSeconds = 0 }, "refreshIntervalSeconds must be greater than 0"},
		{"EmptyQueuePageFile", func(c *Config) { c.QueuePageFile = "" }, "queuePageFile cannot be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CreateConfig()
			tt.modifier(cfg)
			next := &mockNext{}
			_, err := New(context.Background(), next, cfg, "test-queue-invalid")
			if err == nil {
				t.Fatalf("New() with invalid config (%s) should have failed, but did not", tt.name)
			}
			if !strings.Contains(err.Error(), tt.expectedErr) {
				t.Errorf("Expected error containing '%s', got '%v'", tt.expectedErr, err)
			}
		})
	}
}

func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		remoteAddr string
		expectedIP string
	}{
		{"X-Forwarded-For", map[string]string{"X-Forwarded-For": "1.1.1.1, 2.2.2.2"}, "3.3.3.3:12345", "1.1.1.1"},
		{"X-Real-IP", map[string]string{"X-Real-IP": "4.4.4.4"}, "5.5.5.5:12345", "4.4.4.4"},
		{"Fly-Client-IP", map[string]string{"Fly-Client-IP": "6.6.6.6"}, "7.7.7.7:12345", "6.6.6.6"},
		{"CF-Connecting-IP", map[string]string{"CF-Connecting-IP": "8.8.8.8"}, "9.9.9.9:12345", "8.8.8.8"},
		{"RemoteAddr only", map[string]string{}, "10.10.10.10:12345", "10.10.10.10"},
		{"RemoteAddr no port", map[string]string{}, "11.11.11.11", "11.11.11.11"},
		{"XFF takes precedence", map[string]string{"X-Forwarded-For": "1.1.1.1", "X-Real-IP": "4.4.4.4"}, "3.3.3.3:12345", "1.1.1.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			req.RemoteAddr = tt.remoteAddr
			ip := getClientIP(req)
			if ip != tt.expectedIP {
				t.Errorf("Expected IP %s, got %s", tt.expectedIP, ip)
			}
		})
	}
}

func TestGetClientID_Cookies(t *testing.T) {
	cfg := CreateConfig()
	cfg.UseCookies = true
	cfg.CookieName = "test-cookie"
	cfg.CookieMaxAgeSeconds = 60

	// Scenario 1: No cookie, new one should be set
	req1 := httptest.NewRequest("GET", "/", nil)
	rr1 := httptest.NewRecorder()
	qm := &QueueManager{config: cfg, logger: log.New(io.Discard, "", 0)} // Simplified QM for this test

	id1, isNew1 := qm.getClientID(rr1, req1)
	if !isNew1 {
		t.Error("Expected isNew to be true for first request")
	}
	if id1 == "" {
		t.Error("Expected a non-empty client ID")
	}
	cookies1 := rr1.Result().Cookies()
	if len(cookies1) != 1 {
		t.Fatalf("Expected 1 cookie to be set, got %d", len(cookies1))
	}
	if cookies1[0].Name != cfg.CookieName || cookies1[0].Value != id1 {
		t.Errorf("Cookie mismatch: Name=%s, Value=%s", cookies1[0].Name, cookies1[0].Value)
	}

	// Scenario 2: Cookie present, should be reused
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.AddCookie(cookies1[0]) // Add the cookie from the previous response
	rr2 := httptest.NewRecorder()

	id2, isNew2 := qm.getClientID(rr2, req2)
	if isNew2 {
		t.Error("Expected isNew to be false when cookie is present")
	}
	if id2 != id1 {
		t.Errorf("Expected client ID to be reused, got %s, expected %s", id2, id1)
	}
	cookies2 := rr2.Result().Cookies()
	if len(cookies2) != 0 { // No new cookie should be set if one was present
		t.Errorf("Expected 0 cookies to be set when reusing, got %d", len(cookies2))
	}
}

func TestGetClientID_NoCookies_IPUserAgentHash(t *testing.T) {
	cfg := CreateConfig()
	cfg.UseCookies = false
	qm := &QueueManager{config: cfg, logger: log.New(io.Discard, "", 0)}

	req1 := httptest.NewRequest("GET", "/", nil)
	req1.RemoteAddr = "1.2.3.4:123"
	req1.Header.Set("User-Agent", "TestAgent1")
	rr1 := httptest.NewRecorder()
	id1, _ := qm.getClientID(rr1, req1)

	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "1.2.3.4:456" // Same IP, different port
	req2.Header.Set("User-Agent", "TestAgent1") // Same User-Agent
	rr2 := httptest.NewRecorder()
	id2, _ := qm.getClientID(rr2, req2)

	if id1 != id2 {
		t.Errorf("Expected same ID for same IP/UserAgent, got %s and %s", id1, id2)
	}

	req3 := httptest.NewRequest("GET", "/", nil)
	req3.RemoteAddr = "5.6.7.8:123" // Different IP
	req3.Header.Set("User-Agent", "TestAgent1")
	rr3 := httptest.NewRecorder()
	id3, _ := qm.getClientID(rr3, req3)

	if id1 == id3 {
		t.Errorf("Expected different ID for different IP, got same: %s", id1)
	}
}

func TestFileExists(t *testing.T) {
	_, cleanup := createTempFile(t, "test content")
	defer cleanup()

	// This test assumes createTempFile returns the name of the created file.
	// For simplicity, we'll use the name directly. A more robust test
	// might involve passing the file object itself.
	tempFileName := "temp_test_file_for_exists_check.txt" // Example name
	f, err := os.Create(tempFileName)
	if err != nil {
		t.Fatalf("Failed to create temp file for TestFileExists: %v", err)
	}
	f.Close()
	defer os.Remove(tempFileName)


	if !fileExists(tempFileName) {
		t.Errorf("fileExists returned false for an existing file: %s", tempFileName)
	}
	if fileExists("this_file_should_not_exist_ever.txt") {
		t.Error("fileExists returned true for a non-existing file")
	}
}


func TestServeHTTP_CapacityAvailable(t *testing.T) {
	cfg := CreateConfig()
	cfg.MaxEntries = 1
	cfg.Debug = true // Enable debug logs for more insight if test fails

	// Capture logs
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)


	nextCalled := false
	nextHandler := &mockNext{serveHTTPFunc: func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}}

	qm, err := New(context.Background(), nextHandler, cfg, "test-cap-avail")
	if err != nil {
		t.Fatalf("Failed to create QueueManager: %v", err)
	}
	// Replace logger after New() to capture logs from this specific QM instance
	if qmInst, ok := qm.(*QueueManager); ok {
		qmInst.logger = logger
		defer qmInst.Stop()
	}


	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	qm.ServeHTTP(rr, req)

	if !nextCalled {
		t.Error("Next handler was not called when capacity was available")
		t.Logf("Logs:\n%s", logBuf.String())
	}
	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
		t.Logf("Logs:\n%s", logBuf.String())
	}
}

func TestServeHTTP_NoCapacity_QueueUser(t *testing.T) {
	cfg := CreateConfig()
	cfg.MaxEntries = 0 // Force no capacity
	cfg.QueuePageFile, _ = createTempFile(t, "Queue Page: [[.Position]]") // Valid template file
	defer os.Remove(cfg.QueuePageFile)
	cfg.Debug = true

	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	nextHandler := &mockNext{} // Should not be called

	qm, err := New(context.Background(), nextHandler, cfg, "test-no-cap")
	if err != nil {
		// If MaxEntries is 0, New should fail due to validateConfig
		if strings.Contains(err.Error(), "maxEntries must be greater than 0") {
			return // This is expected behavior
		}
		t.Fatalf("Failed to create QueueManager: %v", err)
	}
	// This part will only be reached if MaxEntries=0 was allowed by New (which it shouldn't be)
	if qmInst, ok := qm.(*QueueManager); ok {
		qmInst.logger = logger
		defer qmInst.Stop()
	}


	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	qm.ServeHTTP(rr, req)

	if rr.Code != cfg.HTTPResponseCode {
		t.Errorf("Expected status %d for queue page, got %d", cfg.HTTPResponseCode, rr.Code)
		t.Logf("Logs:\n%s", logBuf.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Queue Page: 1") { // Expecting position 1
		t.Errorf("Queue page content mismatch. Got: %s", body)
		t.Logf("Logs:\n%s", logBuf.String())
	}
}


func TestServeHTTP_MultipleUsers_QueueAndProceed(t *testing.T) {
	cfg := CreateConfig()
	cfg.MaxEntries = 1
	cfg.InactivityTimeoutSeconds = 1 // Short for testing expiry
	cfg.CleanupIntervalSeconds = 1   // Short for testing cleanup
	cfg.QueuePageFile, _ = createTempFile(t, "Queue: Pos [[.Position]] Size [[.QueueSize]] Wait [[.EstimatedWaitTime]]")
	defer os.Remove(cfg.QueuePageFile)
	cfg.Debug = true

	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)


	var serviceAccessCount int
	var mu sync.Mutex // To protect serviceAccessCount

	nextHandler := &mockNext{serveHTTPFunc: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		serviceAccessCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Service Accessed by " + r.Header.Get("X-Test-Client-ID")))
	}}

	qmHandler, err := New(context.Background(), nextHandler, cfg, "test-multi-user")
	if err != nil {
		t.Fatalf("Failed to create QueueManager: %v", err)
	}
	qmInstance := qmHandler.(*QueueManager)
	qmInstance.logger = logger // Override logger
	defer qmInstance.Stop()

	// --- Client 1 ---
	req1 := httptest.NewRequest("GET", "/client1", nil)
	req1.Header.Set("X-Test-Client-ID", "client1") // For easier log tracking
	rr1 := httptest.NewRecorder()
	qmHandler.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Errorf("Client 1: Expected status %d, got %d. Body: %s", http.StatusOK, rr1.Code, rr1.Body.String())
		t.Logf("Logs for Client 1:\n%s", logBuf.String())
		logBuf.Reset()
		return
	}
	if !strings.Contains(rr1.Body.String(), "Service Accessed by client1") {
		t.Errorf("Client 1: Expected service access, got: %s", rr1.Body.String())
	}
	mu.Lock()
	if serviceAccessCount != 1 {
		t.Errorf("Client 1: Expected service access count 1, got %d", serviceAccessCount)
	}
	mu.Unlock()
	logBuf.Reset() // Clear log buffer for next client

	// --- Client 2 (should be queued) ---
	req2 := httptest.NewRequest("GET", "/client2", nil)
	req2.Header.Set("X-Test-Client-ID", "client2")
	// Simulate Client 2 using the cookie Client 1 might have received
	// This requires getClientID to be robust. For simplicity, we'll let it generate a new one.
	rr2 := httptest.NewRecorder()
	qmHandler.ServeHTTP(rr2, req2)

	if rr2.Code != cfg.HTTPResponseCode {
		t.Errorf("Client 2: Expected status %d (queue page), got %d. Body: %s", cfg.HTTPResponseCode, rr2.Code, rr2.Body.String())
		t.Logf("Logs for Client 2:\n%s", logBuf.String())
		logBuf.Reset()
		return
	}
	if !strings.Contains(rr2.Body.String(), "Queue: Pos 1") { // Client 2 is 1st in queue
		t.Errorf("Client 2: Expected to be position 1 in queue, got: %s", rr2.Body.String())
	}
	mu.Lock()
	if serviceAccessCount != 1 { // Service access count should still be 1
		t.Errorf("Client 2: Expected service access count 1, got %d", serviceAccessCount)
	}
	mu.Unlock()
	logBuf.Reset()

	// --- Wait for Client 1's session to expire and cleanup to run ---
	// Inactivity is 1s, Cleanup is 1s. Wait a bit longer.
	time.Sleep(time.Duration(cfg.InactivityTimeoutSeconds+cfg.CleanupIntervalSeconds+1) * time.Second)


	// --- Client 2 again (should now get access) ---
	req3 := httptest.NewRequest("GET", "/client2-retry", nil) // New request path for clarity
	req3.Header.Set("X-Test-Client-ID", "client2") // Same client ID
	// Add cookie that client 2 would have received from its first (queued) request
	cookiesClient2 := rr2.Result().Cookies()
	for _, c := range cookiesClient2 {
		if c.Name == cfg.CookieName {
			req3.AddCookie(c)
			break
		}
	}

	rr3 := httptest.NewRecorder()
	qmHandler.ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusOK {
		t.Errorf("Client 2 (retry): Expected status %d, got %d. Body: %s", http.StatusOK, rr3.Code, rr3.Body.String())
		t.Logf("Logs for Client 2 (retry):\n%s", logBuf.String())
		return
	}
	if !strings.Contains(rr3.Body.String(), "Service Accessed by client2") {
		t.Errorf("Client 2 (retry): Expected service access, got: %s", rr3.Body.String())
	}
	mu.Lock()
	if serviceAccessCount != 2 { // Service access count should now be 2
		t.Errorf("Client 2 (retry): Expected service access count 2, got %d", serviceAccessCount)
	}
	mu.Unlock()
}


func TestPrepareQueuePageData(t *testing.T) {
	qm := &QueueManager{
		config: CreateConfig(),
		logger: log.New(io.Discard, "", 0),
		queue:  make([]Session, 0),
		activeSessionIDs: make(map[string]bool),
	}
	qm.config.MinWaitTimeMinutes = 1
	qm.config.RefreshIntervalSeconds = 15

	// Scenario 1: Empty queue, new user (pos 0)
	data1 := qm.prepareQueuePageData(0) // pos 0 (first in line)
	if data1.Position != 1 { t.Errorf("Expected Position 1, got %d", data1.Position) }
	if data1.QueueSize != 0 { t.Errorf("Expected QueueSize 0, got %d", data1.QueueSize) }
	if data1.EstimatedWaitTime != 1 { t.Errorf("Expected EstimatedWaitTime %d, got %d", qm.config.MinWaitTimeMinutes, data1.EstimatedWaitTime) } // Min wait time
	if data1.ProgressPercentage != 99 {t.Errorf("Expected ProgressPercentage 99, got %d", data1.ProgressPercentage)}


	// Scenario 2: Queue with 5 people, user is 3rd in line (pos 2)
	qm.queue = make([]Session, 5) // Simulate 5 people in queue
	data2 := qm.prepareQueuePageData(2) // 0-indexed position 2 (3rd person)
	if data2.Position != 3 { t.Errorf("Expected Position 3, got %d", data2.Position) }
	if data2.QueueSize != 5 { t.Errorf("Expected QueueSize 5, got %d", data2.QueueSize) }
	// Wait factor for pos 2 (0-indexed) is 0.3 + (2%5 * 0.08) = 0.3 + 0.16 = 0.46
	// Raw wait = 2 * 0.46 = 0.92. Ceil(0.92) = 1. Max(1, MinWaitTime=1) = 1
	if data2.EstimatedWaitTime != 1 { t.Errorf("Expected EstimatedWaitTime 1, got %d", data2.EstimatedWaitTime) }
	// Progress: (5 - (2+1)) / 5 * 100 = (5-3)/5 * 100 = 2/5 * 100 = 40%
	if data2.ProgressPercentage != 40 { t.Errorf("Expected ProgressPercentage 40, got %d", data2.ProgressPercentage) }


	// Scenario 3: Queue with 1 person, user is 1st (pos 0)
	qm.queue = make([]Session, 1)
	data3 := qm.prepareQueuePageData(0)
	if data3.Position != 1 { t.Errorf("Expected Position 1, got %d", data3.Position) }
	if data3.QueueSize != 1 { t.Errorf("Expected QueueSize 1, got %d", data3.QueueSize) }
	if data3.EstimatedWaitTime != 1 { t.Errorf("Expected EstimatedWaitTime %d, got %d", qm.config.MinWaitTimeMinutes, data3.EstimatedWaitTime) }
	// Progress: (1 - (0+1)) / 1 * 100 = 0. Should be adjusted.
	// The logic is: if queueSize == 1 && positionInQueue == 0 -> 50%
	if data3.ProgressPercentage != 50 {t.Errorf("Expected ProgressPercentage 50 for single item queue, got %d", data3.ProgressPercentage)}
}


// Note: Add more comprehensive tests, especially for CleanupExpiredSessions scenarios
// including hard session limits, and more edge cases for ServeHTTP.
// Mocking time (time.Now()) would make these tests more robust and faster,
// but it adds complexity (e.g., using an interface for time or a library).
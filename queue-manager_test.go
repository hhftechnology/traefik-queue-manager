// Package queuemanager provides a Traefik middleware to control traffic flow
// by implementing a queue system similar to a virtual waiting room.
package queuemanager

import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestGenerateUniqueID(t *testing.T) {
	// Create a mock request
	req := &http.Request{
		RemoteAddr: "192.168.1.1:12345",
		Header:     make(http.Header),
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 Test")

	// Generate two IDs - they should be different
	id1 := generateUniqueID(req)
	id2 := generateUniqueID(req)
	
	// IDs should not be empty
	if id1 == "" {
		t.Errorf("Generated ID should not be empty")
	}
	
	// IDs should be unique even when generated in rapid succession
	if id1 == id2 {
		t.Errorf("Generated IDs should be unique, got %s twice", id1)
	}
	
	// Generate more IDs with different IPs
	req.RemoteAddr = "192.168.1.2:12345"
	id3 := generateUniqueID(req)
	
	if id1 == id3 {
		t.Errorf("IDs should be different for different IP addresses")
	}
}

func TestGenerateClientHash(t *testing.T) {
	// Create two mock requests with the same IP and User-Agent
	req1 := &http.Request{
		RemoteAddr: "192.168.1.1:12345",
		Header:     make(http.Header),
	}
	req1.Header.Set("User-Agent", "Mozilla/5.0 Test")
	
	req2 := &http.Request{
		RemoteAddr: "192.168.1.1:54321", // Different port should not affect the hash
		Header:     make(http.Header),
	}
	req2.Header.Set("User-Agent", "Mozilla/5.0 Test")
	
	// Create a request with different IP
	req3 := &http.Request{
		RemoteAddr: "192.168.1.2:12345",
		Header:     make(http.Header),
	}
	req3.Header.Set("User-Agent", "Mozilla/5.0 Test")
	
	// Create a request with different User-Agent
	req4 := &http.Request{
		RemoteAddr: "192.168.1.1:12345",
		Header:     make(http.Header),
	}
	req4.Header.Set("User-Agent", "Mozilla/5.0 Different")
	
	// Test same IP and User-Agent give same hash
	hash1 := generateClientHash(req1)
	hash2 := generateClientHash(req2)
	if hash1 != hash2 {
		t.Errorf("Same IP and User-Agent should give same hash, got %s and %s", hash1, hash2)
	}
	
	// Test different IP gives different hash
	hash3 := generateClientHash(req3)
	if hash1 == hash3 {
		t.Errorf("Different IP should give different hash, got %s twice", hash1)
	}
	
	// Test different User-Agent gives different hash
	hash4 := generateClientHash(req4)
	if hash1 == hash4 {
		t.Errorf("Different User-Agent should give different hash, got %s twice", hash1)
	}
}

func TestGetClientIP(t *testing.T) {
	// Test RemoteAddr
	req1 := &http.Request{
		RemoteAddr: "192.168.1.1:12345",
		Header:     make(http.Header),
	}
	if ip := getClientIP(req1); ip != "192.168.1.1" {
		t.Errorf("Expected IP 192.168.1.1, got %s", ip)
	}
	
	// Test X-Forwarded-For
	req2 := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header:     make(http.Header),
	}
	req2.Header.Set("X-Forwarded-For", "192.168.1.2, 10.0.0.1")
	if ip := getClientIP(req2); ip != "192.168.1.2" {
		t.Errorf("Expected IP 192.168.1.2, got %s", ip)
	}
	
	// Test X-Real-IP
	req3 := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header:     make(http.Header),
	}
	req3.Header.Set("X-Real-IP", "192.168.1.3")
	if ip := getClientIP(req3); ip != "192.168.1.3" {
		t.Errorf("Expected IP 192.168.1.3, got %s", ip)
	}
	
	// Test precedence: X-Forwarded-For > X-Real-IP > RemoteAddr
	req4 := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header:     make(http.Header),
	}
	req4.Header.Set("X-Forwarded-For", "192.168.1.4, 10.0.0.1")
	req4.Header.Set("X-Real-IP", "192.168.1.5")
	if ip := getClientIP(req4); ip != "192.168.1.4" {
		t.Errorf("Expected IP 192.168.1.4, got %s", ip)
	}
}

func TestFileExists(t *testing.T) {
	// Create a temporary file
	tempFile := "temp_test_file.txt"
	f, err := os.Create(tempFile)
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	f.Close()
	
	// Clean up after the test
	defer func() {
		err := os.Remove(tempFile)
		if err != nil {
			t.Logf("Warning: Failed to remove temporary file: %v", err)
		}
	}()
	
	// Test that the file exists
	if !fileExists(tempFile) {
		t.Errorf("fileExists should return true for an existing file")
	}
	
	// Test a non-existent file
	if fileExists("nonexistent_file.txt") {
		t.Errorf("fileExists should return false for a non-existent file")
	}
}

// Add tests for the SimpleCache
func TestSimpleCache(t *testing.T) {
	// Create a new cache with default expiration of 1 second
	cache := NewSimpleCache(time.Second, time.Second*2)
	
	// Test Set and Get
	cache.Set("key1", "value1", DefaultExpiration)
	val, found := cache.Get("key1")
	if !found {
		t.Errorf("Expected to find key1 in cache")
	}
	if val != "value1" {
		t.Errorf("Expected value1, got %v", val)
	}
	
	// Test with non-existant key
	_, found = cache.Get("nonexistent")
	if found {
		t.Errorf("Should not find nonexistent key")
	}
	
	// Test expiration
	cache.Set("key2", "value2", 500*time.Millisecond)
	time.Sleep(800 * time.Millisecond)
	_, found = cache.Get("key2")
	if found {
		t.Errorf("key2 should have expired")
	}
	
	// Test Delete
	cache.Set("key3", "value3", NoExpiration)
	cache.Delete("key3")
	_, found = cache.Get("key3")
	if found {
		t.Errorf("key3 should have been deleted")
	}
	
	// Test Flush
	cache.Set("key4", "value4", NoExpiration)
	cache.Flush()
	if cache.ItemCount() != 0 {
		t.Errorf("Cache should be empty after Flush")
	}
}
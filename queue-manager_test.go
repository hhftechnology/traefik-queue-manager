package traefik_queue_manager

import (
	"net/http"
	"testing"
)

func TestGenerateUniqueID(t *testing.T) {
	// Create a mock request
	req := &http.Request{
		RemoteAddr: "192.168.1.1:12345",
		Header: http.Header{
			"User-Agent": []string{"Mozilla/5.0 Test"},
		},
	}

	// Generate an ID
	id1 := generateUniqueID(req)
	
	// IDs should not be empty
	if id1 == "" {
		t.Errorf("Generated ID should not be empty")
	}
	
	// Generate another ID - should be different
	id2 := generateUniqueID(req)
	if id1 == id2 {
		t.Errorf("Generated IDs should be unique, got %s twice", id1)
	}
}

func TestGenerateClientHash(t *testing.T) {
	// Create two mock requests with the same IP and User-Agent
	req1 := &http.Request{
		RemoteAddr: "192.168.1.1:12345",
		Header: http.Header{
			"User-Agent": []string{"Mozilla/5.0 Test"},
		},
	}
	
	req2 := &http.Request{
		RemoteAddr: "192.168.1.1:54321", // Different port should not affect the hash
		Header: http.Header{
			"User-Agent": []string{"Mozilla/5.0 Test"},
		},
	}
	
	// Create a request with different IP
	req3 := &http.Request{
		RemoteAddr: "192.168.1.2:12345",
		Header: http.Header{
			"User-Agent": []string{"Mozilla/5.0 Test"},
		},
	}
	
	// Create a request with different User-Agent
	req4 := &http.Request{
		RemoteAddr: "192.168.1.1:12345",
		Header: http.Header{
			"User-Agent": []string{"Mozilla/5.0 Different"},
		},
	}
	
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
		Header: make(http.Header),
	}
	if ip := getClientIP(req1); ip != "192.168.1.1" {
		t.Errorf("Expected IP 192.168.1.1, got %s", ip)
	}
	
	// Test X-Forwarded-For
	req2 := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header: make(http.Header),
	}
	req2.Header.Set("X-Forwarded-For", "192.168.1.2, 10.0.0.1")
	if ip := getClientIP(req2); ip != "192.168.1.2" {
		t.Errorf("Expected IP 192.168.1.2, got %s", ip)
	}
	
	// Test X-Real-IP
	req3 := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header: make(http.Header),
	}
	req3.Header.Set("X-Real-IP", "192.168.1.3")
	if ip := getClientIP(req3); ip != "192.168.1.3" {
		t.Errorf("Expected IP 192.168.1.3, got %s", ip)
	}
	
	// Test precedence: X-Forwarded-For > X-Real-IP > RemoteAddr
	req4 := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header: make(http.Header),
	}
	req4.Header.Set("X-Forwarded-For", "192.168.1.4, 10.0.0.1")
	req4.Header.Set("X-Real-IP", "192.168.1.5")
	if ip := getClientIP(req4); ip != "192.168.1.4" {
		t.Errorf("Expected IP 192.168.1.4, got %s", ip)
	}
}
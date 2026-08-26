package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubTokenOnlyAuthenticatesAPIRequests(t *testing.T) {
	t.Setenv("GH_TOKEN", "api-token")
	t.Setenv("GITHUB_TOKEN", "")

	var apiAuthorization, downloadAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			apiAuthorization = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(release{TagName: "v1.2.3"})
		case "/download":
			downloadAuthorization = r.Header.Get("Authorization")
			_, _ = w.Write([]byte("archive"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var got release
	if err := getJSON(context.Background(), server.URL+"/api", &got); err != nil {
		t.Fatalf("API request: %v", err)
	}
	if got.TagName != "v1.2.3" || apiAuthorization != "Bearer api-token" {
		t.Fatalf("API request authorization = %q, release = %+v", apiAuthorization, got)
	}

	path := filepath.Join(t.TempDir(), "archive")
	if err := download(context.Background(), server.URL+"/download", path); err != nil {
		t.Fatalf("download: %v", err)
	}
	if downloadAuthorization != "" {
		t.Fatalf("download received API authorization %q", downloadAuthorization)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "archive" {
		t.Fatalf("downloaded data = %q, error = %v", data, err)
	}
}

func TestGitHubTokenFallsBackToGITHUB_TOKEN(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "fallback-token")

	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(release{TagName: "v1.2.3"})
	}))
	defer server.Close()

	var got release
	if err := getJSON(context.Background(), server.URL, &got); err != nil {
		t.Fatalf("API request: %v", err)
	}
	if got.TagName != "v1.2.3" || authorization != "Bearer fallback-token" {
		t.Fatalf("API request authorization = %q, release = %+v", authorization, got)
	}
}

func TestGitHubRateLimitErrorExplainsTokenRecovery(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	var got release
	err := getJSON(context.Background(), server.URL, &got)
	if err == nil {
		t.Fatal("rate-limited request unexpectedly succeeded")
	}
	for _, want := range []string{"GitHub API rate limit exceeded", "GH_TOKEN", "GITHUB_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("rate-limit error %q does not contain %q", err, want)
		}
	}
}

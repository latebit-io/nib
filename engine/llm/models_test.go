package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newModelServer(t *testing.T, status int, body, wantAuth string) *AgentAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("Authorization = %q, want %q", got, wantAuth)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // test server, error irrelevant
	}))
	t.Cleanup(srv.Close)
	key := ""
	if wantAuth != "" {
		// Extract key from "Bearer <key>".
		key = wantAuth[len("Bearer "):]
	}
	return NewAgentAPI(srv.URL, "test-model", key)
}

func TestListModels(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantCount int
		wantFirst string
		wantErr   bool
	}{
		{"standard response", http.StatusOK, `{"data":[{"id":"gpt-4o","name":"GPT-4o"},{"id":"gpt-3.5-turbo"}]}`, 2, "gpt-4o", false},
		{"name falls back to id", http.StatusOK, `{"data":[{"id":"my-model"}]}`, 1, "my-model", false},
		{"auth error", http.StatusUnauthorized, `{"error":"invalid key"}`, 0, "", true},
		{"empty list", http.StatusOK, `{"data":[]}`, 0, "", false},
		{"invalid json", http.StatusOK, `not json`, 0, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newModelServer(t, tt.status, tt.body, "Bearer test-key")
			models, err := api.ListModels(context.Background())

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(models) != tt.wantCount {
				t.Fatalf("got %d models, want %d", len(models), tt.wantCount)
			}
			if tt.wantCount > 0 && models[0].ID != tt.wantFirst {
				t.Errorf("first model ID = %q, want %q", models[0].ID, tt.wantFirst)
			}
			if tt.wantCount > 0 && models[0].Name == "" {
				t.Error("Name should not be empty (falls back to ID)")
			}
		})
	}
}

func TestListModels_NoAuth(t *testing.T) {
	api := newModelServer(t, http.StatusOK, `{"data":[{"id":"public-model"}]}`, "")
	models, err := api.ListModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "public-model" {
		t.Errorf("got %v, want [{public-model public-model}]", models)
	}
}

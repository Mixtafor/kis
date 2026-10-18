package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testServer struct {
	URL     string
	handler http.Handler
}

func newTestServer(handler http.Handler) *testServer {
	return &testServer{URL: "http://edition.test", handler: handler}
}

func (s *testServer) Close() {}

func (s *testServer) run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	return run(ctx, args, stdin, stdout, stderr, getenv, handlerTransport{s.handler})
}

type handlerTransport struct{ http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, r)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	response := recorder.Result()
	response.Request = r
	return response, nil
}

func environment(endpoint string) func(string) string {
	return func(key string) string {
		return map[string]string{
			"EDITION_URL":       endpoint,
			"EDITION_CLIENT_ID": "reporter",
			"EDITION_API_KEY":   "private-api-key",
		}[key]
	}
}

func TestHandshakePrecedesAuthenticatedCommand(t *testing.T) {
	var sequence []string
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sequence = append(sequence, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/handshake" {
			if r.Header.Get("Authorization") != "" {
				t.Error("handshake must not contain a bearer token")
			}
			var credentials map[string]string
			if err := json.NewDecoder(r.Body).Decode(&credentials); err != nil {
				t.Errorf("decode credentials: %v", err)
			}
			if credentials["client_id"] != "reporter" || credentials["api_key"] != "private-api-key" {
				t.Error("wrong handshake credentials")
			}
			_, _ = io.WriteString(w, `{"token":"private-session-token","expires_at":"2026-10-18T10:00:00Z"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer private-session-token" {
			t.Error("command must carry the handshake token")
		}
		_, _ = io.WriteString(w, `{"client_id":"reporter","accepted":3}`)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := server.run(context.Background(), []string{"stats"}, strings.NewReader(""), &stdout, &stderr, environment(server.URL))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
	if !reflect.DeepEqual(sequence, []string{"POST /v1/handshake", "GET /v1/stats/me"}) {
		t.Fatalf("unexpected request sequence: %v", sequence)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"accepted": 3`) {
		t.Fatalf("unexpected JSON output: %s", &stdout)
	}
	for _, secret := range []string{"private-api-key", "private-session-token"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("CLI printed a credential")
		}
	}
}

func TestHandshakeFailureStopsCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"private-api-key"}}`},
		{"missing_token", http.StatusOK, `{"client":{"id":"reporter"}}`},
		{"malformed_json", http.StatusOK, `not JSON`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/handshake" {
					t.Errorf("unexpected command after failed handshake: %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			code := server.run(context.Background(), []string{"stats"}, strings.NewReader(""), &stdout, &stderr, environment(server.URL))
			if code != 1 || requests.Load() != 1 {
				t.Fatalf("exit=%d requests=%d stderr=%s", code, requests.Load(), &stderr)
			}
			if strings.Contains(stdout.String()+stderr.String(), "private-api-key") || stdout.Len() > 0 {
				t.Fatal("handshake response was exposed")
			}
		})
	}
}

func TestHTTPFailurePreservesStructuredReceipt(t *testing.T) {
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/handshake" {
			_, _ = io.WriteString(w, `{"token":"session"}`)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"rejected","message":"already published","request_id":"trace-id"},"receipt":{"status":"rejected","reason":"published"}}`)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"submit", "--request-id", "stable-id", "--category", "science", "--topic", "discovery", "--body", "text", "--event-at", "2026-10-18T09:00:00Z"}
	code := server.run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, environment(server.URL))
	if code != 1 || !strings.Contains(stderr.String(), "422") || !strings.Contains(stdout.String(), `"receipt"`) || !strings.Contains(stdout.String(), `"request_id": "trace-id"`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}

func TestSubmissionFileIsStableAcrossRetries(t *testing.T) {
	const saved = "{\n  \"request_id\": \"stable-id\", \"category\": \" Science \", \"topic\": \"Launch\",\n  \"body\": \"The launch succeeded.\", \"event_at\": \"2026-10-18T12:34:56.123456789+03:00\"\n}\n"
	filePath := filepath.Join(t.TempDir(), "submission.json")
	if err := os.WriteFile(filePath, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	var sent [][]byte
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/handshake" {
			_, _ = io.WriteString(w, `{"token":"session"}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/notes" {
			t.Errorf("wrong submission route: %s %s", r.Method, r.URL.Path)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		sent = append(sent, data)
		_, _ = io.WriteString(w, `{"status":"accepted","note_id":"note-1"}`)
	}))
	defer server.Close()
	for _, source := range []string{filePath, "-"} {
		var stdout, stderr bytes.Buffer
		if code := server.run(context.Background(), []string{"submit", "--file", source}, strings.NewReader(saved), &stdout, &stderr, environment(server.URL)); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, &stderr)
		}
	}
	if len(sent) != 2 || string(sent[0]) != saved || string(sent[1]) != saved {
		t.Fatalf("retries changed saved request content: %q", sent)
	}
}

func TestCommandsUsePublicContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		method string
		path   string
		query  string
		body   string
	}{
		{"statistics", []string{"all-stats", "--limit", "20", "--offset", "40"}, "GET", "/v1/editor/stats", "limit=20&offset=40", ""},
		{"stories", []string{"stories", "--state", "published", "--category", "Space news"}, "GET", "/v1/stories", "category=Space+news&limit=100&offset=0&state=published", ""},
		{"story", []string{"story", "--id", "story-1"}, "GET", "/v1/stories/story-1", "", ""},
		{"messages", []string{"messages", "--after", "12", "--limit", "5", "--unacked=false"}, "GET", "/v1/messages", "after=12&limit=5&unacked=false", ""},
		{"acknowledge", []string{"ack", "--id", "12"}, "POST", "/v1/messages/12/ack", "", ""},
		{"rotate_key", []string{"rotate-key", "--id", "alice"}, "POST", "/v1/editor/clients/alice/key", "", ""},
		{"create", []string{"create-client", "--id", "reporter", "--name", "Jane", "--role", "outstaff", "--trust", "0.8"}, "POST", "/v1/editor/clients", "", `{"id":"reporter","name":"Jane","role":"outstaff","trust":0.8,"enabled":true}`},
		{"trust", []string{"set-trust", "--id", "reporter", "--trust", "0", "--enabled=false"}, "PATCH", "/v1/editor/clients/reporter", "", `{"trust":0,"enabled":false}`},
		{"policy", []string{"policy"}, "GET", "/v1/editor/policy", "", ""},
		{"set_policy", []string{"set-policy", "--threshold", "2.5", "--half-life-seconds", "3600", "--congratulations-count", "0"}, "PUT", "/v1/editor/policy", "", `{"threshold":2.5,"half_life_seconds":3600,"congratulations_count":0}`},
		{"submit", []string{"submit", "--request-id", "retry-1", "--category", " science ", "--topic", "Topic", "--body", "Report", "--event-at", "2026-10-18T12:00:00+03:00"}, "POST", "/v1/notes", "", `{"request_id":"retry-1","category":" science ","topic":"Topic","body":"Report","event_at":"2026-10-18T09:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/handshake" {
					_, _ = io.WriteString(w, `{"token":"session"}`)
					return
				}
				calls.Add(1)
				if r.Method != tc.method || r.URL.Path != tc.path || r.URL.RawQuery != tc.query {
					t.Errorf("request=%s %s, want %s %s?%s", r.Method, r.URL.String(), tc.method, tc.path, tc.query)
				}
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != tc.body {
					t.Errorf("body=%s want=%s err=%v", data, tc.body, err)
				}
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			if code := server.run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, environment(server.URL)); code != 0 || calls.Load() != 1 {
				t.Fatalf("exit=%d requests=%d stderr=%s", code, calls.Load(), &stderr)
			}
		})
	}
}

func TestInvalidCommandsDoNotAuthenticate(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"},
		{"rotate-key"},
		{"stats", "extra"},
		{"submit", "--request-id", "only-id"},
		{"submit", "--file", "-", "--event-at", "2026-10-18T09:00:00Z"},
		{"submit", "--file", "-"},
		{"set-trust", "--id", "reporter", "--trust", "NaN"},
		{"set-policy", "--threshold", "2", "--half-life-seconds", "1"},
		{"stories", "--limit", "101"},
		{"stories", "--state", "unknown"},
		{"ack", "--id", "0"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), args, strings.NewReader(`{"request_id":"no-other-fields"}`), &stdout, &stderr, func(string) string {
				t.Error("invalid arguments must fail before reading credentials")
				return ""
			})
			if code != 2 {
				t.Fatalf("exit=%d stderr=%s", code, &stderr)
			}
		})
	}
}

func TestCancellationStopsHandshake(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan int, 1)
	go func() {
		finished <- server.run(ctx, []string{"stats"}, strings.NewReader(""), io.Discard, io.Discard, environment(server.URL))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handshake did not start")
	}
	cancel()
	select {
	case code := <-finished:
		if code != 1 {
			t.Fatalf("cancelled command exit=%d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not honor cancellation")
	}
}

// При перенаправлении клиент не должен отправлять API-ключ на другой адрес.
func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var forwarded atomic.Int32
	source := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "elsewhere.test" {
			forwarded.Add(1)
			_, _ = io.WriteString(w, `{"token":"stolen"}`)
			return
		}
		w.Header().Set("Location", "http://elsewhere.test/capture")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	var stdout, stderr bytes.Buffer
	code := source.run(context.Background(), []string{"stats"}, strings.NewReader(""), &stdout, &stderr, environment(source.URL))
	if code != 1 || forwarded.Load() != 0 {
		t.Fatalf("exit=%d forwarded requests=%d stderr=%s", code, forwarded.Load(), &stderr)
	}
}

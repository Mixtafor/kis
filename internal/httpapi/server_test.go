package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"edition/api"
	"edition/internal/app"
	"edition/internal/news"
)

var testNow = time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)

type fakeRepository struct {
	app.Repository
	handshake  func(context.Context, string, string) (news.Session, error)
	submit     func(context.Context, string, news.Submission) (news.Receipt, error)
	create     func(context.Context, news.Client) (string, error)
	rotateKey  func(context.Context, string) (string, error)
	statistics func(context.Context, string) (news.Stats, error)
	stories    func(context.Context, string, string, int, int) ([]news.Story, error)
	ping       func(context.Context) error
}

func (r *fakeRepository) Authenticate(_ context.Context, token string) (news.Client, error) {
	switch token {
	case "staff-token":
		return news.Client{ID: "staff-1", Name: "Reporter", Role: "staff", Trust: 0.8, Enabled: true}, nil
	case "editor-token":
		return news.Client{ID: "editor-1", Name: "Editor", Role: "editor", Trust: 1, Enabled: true}, nil
	default:
		return news.Client{}, news.ErrUnauthorized
	}
}

func (r *fakeRepository) Handshake(ctx context.Context, id, key string) (news.Session, error) {
	return r.handshake(ctx, id, key)
}

func (r *fakeRepository) Submit(ctx context.Context, id string, note news.Submission) (news.Receipt, error) {
	return r.submit(ctx, id, note)
}

func (r *fakeRepository) CreateClient(ctx context.Context, client news.Client) (string, error) {
	return r.create(ctx, client)
}

func (r *fakeRepository) RotateClientKey(ctx context.Context, id string) (string, error) {
	return r.rotateKey(ctx, id)
}

func (r *fakeRepository) Statistics(ctx context.Context, id string) (news.Stats, error) {
	return r.statistics(ctx, id)
}

func (r *fakeRepository) Stories(ctx context.Context, state, category string, limit, offset int) ([]news.Story, error) {
	return r.stories(ctx, state, category, limit, offset)
}

func (r *fakeRepository) Ping(ctx context.Context) error { return r.ping(ctx) }

func testHandler(repo app.Repository) http.Handler {
	server := &Server{
		Service: app.Service{Repo: repo, Now: func() time.Time { return testNow }},
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Version: "test", Commit: "test-commit",
	}
	return server.Handler()
}

func call(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requireAPIError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid error JSON: %v", err)
	}
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("unexpected error: %s", response.Body)
	}
	if body.Error.RequestID == "" || body.Error.RequestID != response.Header().Get("X-Request-ID") {
		t.Fatalf("error must carry the response request ID: %s", response.Body)
	}
}

func TestProtectedRoutesRequireHandshakeToken(t *testing.T) {
	handler := testHandler(&fakeRepository{})
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/notes"}, {"GET", "/v1/stories"}, {"GET", "/v1/stories/story-1"},
		{"GET", "/v1/stats/me"}, {"GET", "/v1/stats/staff-2"}, {"GET", "/v1/messages"},
		{"POST", "/v1/messages/1/ack"}, {"POST", "/v1/editor/clients"},
		{"POST", "/v1/editor/clients/staff-1/key"},
		{"PATCH", "/v1/editor/clients/staff-1"}, {"GET", "/v1/editor/policy"},
		{"PUT", "/v1/editor/policy"}, {"GET", "/v1/editor/stats"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			for _, token := range []string{"", "unknown-session"} {
				response := call(handler, tc.method, tc.path, token, `{}`)
				requireAPIError(t, response, http.StatusUnauthorized, "unauthorized")
				if response.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Error("missing bearer challenge")
				}
			}
		})
	}
}

func TestUnknownClientCannotHandshake(t *testing.T) {
	called := false
	repo := &fakeRepository{handshake: func(_ context.Context, id, key string) (news.Session, error) {
		called = true
		if id != "unknown-client" || key != "unknown-private-api-key" {
			t.Error("handshake did not preserve input credentials")
		}
		return news.Session{}, news.ErrUnauthorized
	}}
	response := call(testHandler(repo), "POST", "/v1/handshake", "", `{"client_id":"unknown-client","api_key":"unknown-private-api-key"}`)
	requireAPIError(t, response, http.StatusUnauthorized, "unauthorized")
	if !called || strings.Contains(response.Body.String(), "unknown-private-api-key") {
		t.Fatal("unknown client must be checked without exposing its API key")
	}
}

func TestEditorActionsRejectStaff(t *testing.T) {
	handler := testHandler(&fakeRepository{})
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/editor/clients"}, {"PATCH", "/v1/editor/clients/staff-1"},
		{"POST", "/v1/editor/clients/staff-1/key"},
		{"GET", "/v1/editor/policy"}, {"PUT", "/v1/editor/policy"}, {"GET", "/v1/editor/stats"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			response := call(handler, tc.method, tc.path, "staff-token", `{}`)
			requireAPIError(t, response, http.StatusForbidden, "forbidden")
		})
	}
}

func TestStatisticsAuthorizeSpecificClient(t *testing.T) {
	for _, tc := range []struct {
		name, path, token, wantID string
		status                    int
	}{
		{"own", "/v1/stats/me", "staff-token", "staff-1", 200},
		{"own_explicit", "/v1/stats/staff-1", "staff-token", "staff-1", 200},
		{"other_denied", "/v1/stats/staff-2", "staff-token", "", 403},
		{"editor_can_inspect_other", "/v1/stats/staff-2", "editor-token", "staff-2", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calledID string
			repo := &fakeRepository{statistics: func(_ context.Context, id string) (news.Stats, error) {
				calledID = id
				return news.Stats{ClientID: id, Accepted: 2, Published: 1, PublicationShare: 0.5}, nil
			}}
			response := call(testHandler(repo), "GET", tc.path, tc.token, "")
			if response.Code != tc.status || calledID != tc.wantID {
				t.Fatalf("status=%d repository client=%q body=%s", response.Code, calledID, response.Body)
			}
			if tc.status == 403 {
				requireAPIError(t, response, tc.status, "forbidden")
			}
		})
	}
}

func TestCreateClientValidatesBeforePersistence(t *testing.T) {
	for _, body := range []string{
		`{"id":"new-client","name":"Reporter","role":"staff","trust":-0.1,"enabled":true}`,
		`{"id":"new-client","name":"Reporter","role":"owner","trust":0.5,"enabled":true}`,
		`{"id":"new-client","name":" ","role":"outstaff","trust":0.5,"enabled":true}`,
	} {
		response := call(testHandler(&fakeRepository{}), "POST", "/v1/editor/clients", "editor-token", body)
		requireAPIError(t, response, http.StatusUnprocessableEntity, "invalid")
	}
	var saved news.Client
	repo := &fakeRepository{create: func(_ context.Context, client news.Client) (string, error) {
		saved = client
		return "new-one-time-api-key", nil
	}}
	response := call(testHandler(repo), "POST", "/v1/editor/clients", "editor-token", `{"id":"new-client","name":"Reporter","role":"outstaff","trust":0.5,"enabled":true}`)
	if response.Code != http.StatusCreated || saved.ID != "new-client" || saved.Role != "outstaff" || saved.Trust != 0.5 || !saved.Enabled {
		t.Fatalf("status=%d saved=%+v body=%s", response.Code, saved, response.Body)
	}
	var created struct {
		Client news.Client `json:"client"`
		APIKey string      `json:"api_key"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.APIKey != "new-one-time-api-key" || created.Client != saved {
		t.Fatalf("client creation response does not provide credentials: %s", response.Body)
	}
}

func TestRotateClientKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"success", nil, http.StatusOK},
		{"missing", news.ErrNotFound, http.StatusNotFound},
		{"storage failure", errors.New("database unavailable"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var target string
			repo := &fakeRepository{rotateKey: func(_ context.Context, id string) (string, error) {
				target = id
				return "replacement-api-key", tc.err
			}}
			response := call(testHandler(repo), "POST", "/v1/editor/clients/alice/key", "editor-token", "")
			if response.Code != tc.code || target != "alice" {
				t.Fatalf("status=%d target=%q body=%s", response.Code, target, response.Body)
			}
			if tc.err == nil {
				var body map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["api_key"] != "replacement-api-key" {
					t.Fatalf("missing replacement key: %s", response.Body)
				}
			} else if strings.Contains(response.Body.String(), "replacement-api-key") {
				t.Fatal("failed rotation must not return a key")
			}
		})
	}
}

func TestMalformedJSONReturnsBadRequest(t *testing.T) {
	for _, tc := range []struct{ name, body, contentType string }{
		{"broken", `{"request_id":`, "application/json"},
		{"unknown_field", `{"surprise":"value"}`, "application/json"},
		{"multiple_objects", `{} {}`, "application/json"},
		{"wrong_type", `{"event_at":42}`, "application/json"},
		{"missing_content_type", `{}`, ""},
		{"wrong_content_type", `{}`, "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/notes", strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer staff-token")
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			testHandler(&fakeRepository{}).ServeHTTP(response, request)
			requireAPIError(t, response, http.StatusBadRequest, "bad_json")
		})
	}
}

// Проверяем квитанции, коды ответов и сохранение исходных полей заметки.
func TestSubmissionReceiptAndIdempotencyStatus(t *testing.T) {
	const valid = `{"request_id":"request-1","category":" Science ","topic":"Launch","body":"A report.","event_at":"2026-10-18T11:00:00Z"}`
	for _, tc := range []struct {
		name, token, body, receipt string
		status                     int
		err                        error
		persist                    bool
	}{
		{"accepted", "staff-token", valid, "accepted", 201, nil, true},
		{"duplicate", "staff-token", valid, "duplicate", 200, nil, true},
		{"conflicting_retry", "staff-token", valid, "", 409, news.ErrConflict, true},
		{"invalid", "staff-token", `{}`, "rejected", 422, nil, false},
		{"future_event", "staff-token", `{"request_id":"future","category":"Science","topic":"Launch","body":"Report","event_at":"2026-10-18T12:02:00Z"}`, "rejected", 422, nil, false},
		{"editor_cannot_submit", "editor-token", valid, "", 403, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			repo := &fakeRepository{submit: func(_ context.Context, id string, note news.Submission) (news.Receipt, error) {
				called = true
				if id != "staff-1" || note.RequestID != "request-1" || note.Category != " Science " || note.EventAt != testNow.Add(-time.Hour) {
					t.Errorf("submission changed: client=%s note=%+v", id, note)
				}
				return news.Receipt{Status: tc.receipt, NoteID: "note-1", StoryID: "story-1", ReceivedAt: testNow}, tc.err
			}}
			response := call(testHandler(repo), "POST", "/v1/notes", tc.token, tc.body)
			if response.Code != tc.status || called != tc.persist {
				t.Fatalf("status=%d persisted=%v body=%s", response.Code, called, response.Body)
			}
			if tc.receipt != "" {
				var receipt news.Receipt
				if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.Status != tc.receipt {
					t.Fatalf("unexpected receipt: %s", response.Body)
				}
				if receipt.Status == "rejected" && receipt.Reason == "" {
					t.Fatal("rejected receipt must explain its reason")
				}
			}
		})
	}
}

func TestCollectionPaginationBounds(t *testing.T) {
	for _, path := range []string{
		"/v1/stories?limit=0", "/v1/stories?limit=101", "/v1/stories?limit=abc", "/v1/stories?offset=-1",
		"/v1/stories?state=unknown", "/v1/editor/stats?limit=101", "/v1/editor/stats?offset=bad",
		"/v1/messages?after=-1", "/v1/messages?after=999999999999999999999", "/v1/messages?unacked=maybe", "/v1/messages?limit=101",
	} {
		t.Run(path, func(t *testing.T) {
			response := call(testHandler(&fakeRepository{}), "GET", path, "editor-token", "")
			requireAPIError(t, response, http.StatusUnprocessableEntity, "invalid")
		})
	}
	for _, limit := range []int{1, 100} {
		repo := &fakeRepository{stories: func(_ context.Context, state, category string, gotLimit, offset int) ([]news.Story, error) {
			if state != "published" || category != " Science " || gotLimit != limit || offset != 12 {
				t.Errorf("filters changed: state=%q category=%q limit=%d offset=%d", state, category, gotLimit, offset)
			}
			return []news.Story{}, nil
		}}
		path := "/v1/stories?state=published&category=+Science+&offset=12&limit=" + strconv.Itoa(limit)
		response := call(testHandler(repo), "GET", path, "staff-token", "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"items":[]`) {
			t.Fatalf("valid pagination failed: %d %s", response.Code, response.Body)
		}
	}
}

func TestReadinessReportsDatabaseFailureWhileLivenessWorks(t *testing.T) {
	var pings int
	repo := &fakeRepository{ping: func(context.Context) error {
		pings++
		return errors.New("database connection failed: secret-password")
	}}
	handler := testHandler(repo)
	live := call(handler, "GET", "/health/live", "", "")
	if live.Code != 200 || pings != 0 {
		t.Fatalf("liveness depends on the database: status=%d pings=%d", live.Code, pings)
	}
	ready := call(handler, "GET", "/health/ready", "", "")
	requireAPIError(t, ready, http.StatusServiceUnavailable, "unavailable")
	if pings != 1 || strings.Contains(ready.Body.String(), "secret-password") {
		t.Fatalf("readiness leaked database details or skipped ping: %s", ready.Body)
	}
}

// Сверяем методы и пути ручек с OpenAPI.
func TestRegisteredRoutesMatchOpenAPI(t *testing.T) {
	var document struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(api.Spec, &document); err != nil {
		t.Fatalf("invalid embedded OpenAPI JSON: %v", err)
	}
	if document.OpenAPI != "3.0.3" {
		t.Fatalf("unexpected OpenAPI version %q", document.OpenAPI)
	}
	documented := make(map[string]bool)
	for path, operations := range document.Paths {
		for method := range operations {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options", "trace":
				documented[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	registered := make(map[string]bool)
	for _, route := range Routes {
		key := route.Method + " " + route.Path
		if registered[key] {
			t.Errorf("duplicate route %s", key)
		}
		registered[key] = true
	}
	if !reflect.DeepEqual(registered, documented) {
		t.Fatalf("routes differ from OpenAPI:\nregistered=%v\ndocumented=%v", registered, documented)
	}
}

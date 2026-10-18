package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"edition/api"
	"edition/internal/app"
	"edition/internal/news"
)

type Server struct {
	Service         app.Service
	Logger          *slog.Logger
	Version, Commit string
}

type route struct{ Method, Path string }

var Routes = []route{
	{"POST", "/v1/handshake"}, {"POST", "/v1/notes"},
	{"GET", "/v1/stories"}, {"GET", "/v1/stories/{id}"},
	{"GET", "/v1/stats/me"}, {"GET", "/v1/stats/{id}"},
	{"GET", "/v1/messages"}, {"POST", "/v1/messages/{id}/ack"},
	{"POST", "/v1/editor/clients"}, {"PATCH", "/v1/editor/clients/{id}"},
	{"POST", "/v1/editor/clients/{id}/key"},
	{"GET", "/v1/editor/policy"}, {"PUT", "/v1/editor/policy"},
	{"GET", "/v1/editor/stats"}, {"GET", "/health/live"}, {"GET", "/health/ready"},
	{"GET", "/docs"},
}

type handler func(http.ResponseWriter, *http.Request, news.Client) error

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Version, "commit": s.Commit})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Service.Repo.Ping(r.Context()); err != nil {
			s.fail(w, r, http.StatusServiceUnavailable, "unavailable", "PostgreSQL is unavailable")
			return
		}
		respond(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, swaggerHTML, api.Spec)
	})
	mux.HandleFunc("POST /v1/handshake", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ClientID string `json:"client_id"`
			APIKey   string `json:"api_key"`
		}
		if err := decode(w, r, &input); err != nil {
			s.failure(w, r, err)
			return
		}
		if !news.Text(input.ClientID, 100) || len(input.APIKey) < 16 || len(input.APIKey) > 256 {
			s.failure(w, r, news.ErrUnauthorized)
			return
		}
		session, err := s.Service.Repo.Handshake(r.Context(), input.ClientID, input.APIKey)
		if err != nil {
			s.failure(w, r, err)
			return
		}
		respond(w, http.StatusOK, session)
	})
	s.register(mux, "POST /v1/notes", false, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		var input news.Submission
		if err := decode(w, r, &input); err != nil {
			return err
		}
		receipt, err := s.Service.Submit(r.Context(), c, input)
		if errors.Is(err, news.ErrInvalid) {
			respond(w, http.StatusUnprocessableEntity, news.Receipt{Status: "rejected", Reason: "request_id, category, topic, body and a valid event_at (at most one minute in the future) are required"})
			return nil
		}
		if err != nil {
			return err
		}
		status := http.StatusCreated
		if receipt.Status == "duplicate" {
			status = http.StatusOK
		}
		respond(w, status, receipt)
		return nil
	})
	s.register(mux, "GET /v1/stories", false, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		limit, offset, err := page(r)
		if err != nil {
			return err
		}
		state := r.URL.Query().Get("state")
		if state != "" && state != "collecting" && state != "published" {
			return news.ErrInvalid
		}
		items, err := s.Service.Repo.Stories(r.Context(), state, r.URL.Query().Get("category"), limit, offset)
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, map[string]any{"items": items})
		return nil
	})
	s.register(mux, "GET /v1/stories/{id}", false, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		v, err := s.Service.Repo.Story(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, v)
		return nil
	})
	stats := func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		id := r.PathValue("id")
		if id == "" {
			id = c.ID
		}
		v, err := s.Service.Statistics(r.Context(), c, id)
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, v)
		return nil
	}
	s.register(mux, "GET /v1/stats/me", false, stats)
	s.register(mux, "GET /v1/stats/{id}", false, stats)
	s.register(mux, "GET /v1/messages", false, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		limit, _, err := page(r)
		if err != nil {
			return err
		}
		after := int64(0)
		if value := r.URL.Query().Get("after"); value != "" {
			after, err = strconv.ParseInt(value, 10, 64)
			if err != nil || after < 0 {
				return news.ErrInvalid
			}
		}
		unacked := true
		if value := r.URL.Query().Get("unacked"); value != "" {
			unacked, err = strconv.ParseBool(value)
			if err != nil {
				return news.ErrInvalid
			}
		}
		items, err := s.Service.Repo.Notifications(r.Context(), c.ID, after, unacked, limit)
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, map[string]any{"items": items})
		return nil
	})
	s.register(mux, "POST /v1/messages/{id}/ack", false, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			return news.ErrInvalid
		}
		if err = s.Service.Repo.Acknowledge(r.Context(), c.ID, id); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	s.register(mux, "POST /v1/editor/clients", true, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		var fields struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Role    string   `json:"role"`
			Trust   *float64 `json:"trust"`
			Enabled *bool    `json:"enabled"`
		}
		if err := decode(w, r, &fields); err != nil {
			return err
		}
		if fields.Trust == nil || fields.Enabled == nil {
			return news.ErrInvalid
		}
		input := news.Client{ID: fields.ID, Name: fields.Name, Role: fields.Role, Trust: *fields.Trust, Enabled: *fields.Enabled}
		key, err := s.Service.CreateClient(r.Context(), c, input)
		if err != nil {
			return err
		}
		respond(w, http.StatusCreated, map[string]any{"client": input, "api_key": key})
		return nil
	})
	s.register(mux, "POST /v1/editor/clients/{id}/key", true, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		id := r.PathValue("id")
		if !news.Text(id, 100) {
			return news.ErrInvalid
		}
		key, err := s.Service.Repo.RotateClientKey(r.Context(), id)
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, map[string]string{"api_key": key})
		return nil
	})
	s.register(mux, "PATCH /v1/editor/clients/{id}", true, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		var input struct {
			Trust   *float64 `json:"trust"`
			Enabled *bool    `json:"enabled"`
		}
		if err := decode(w, r, &input); err != nil {
			return err
		}
		if input.Trust == nil || input.Enabled == nil {
			return news.ErrInvalid
		}
		v := news.Client{ID: r.PathValue("id"), Name: "validation", Role: "staff", Trust: *input.Trust, Enabled: *input.Enabled}
		if err := v.Validate(); err != nil {
			return err
		}
		v, err := s.Service.Repo.UpdateClient(r.Context(), v.ID, v.Trust, v.Enabled)
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, v)
		return nil
	})
	s.register(mux, "GET /v1/editor/policy", true, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		v, err := s.Service.Repo.Policy(r.Context())
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, v)
		return nil
	})
	s.register(mux, "PUT /v1/editor/policy", true, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		var input struct {
			Threshold            *float64 `json:"threshold"`
			HalfLifeSeconds      *int64   `json:"half_life_seconds"`
			CongratulationsCount *int     `json:"congratulations_count"`
		}
		if err := decode(w, r, &input); err != nil {
			return err
		}
		if input.Threshold == nil || input.HalfLifeSeconds == nil || input.CongratulationsCount == nil {
			return news.ErrInvalid
		}
		p := news.Policy{Threshold: *input.Threshold, HalfLifeSeconds: *input.HalfLifeSeconds, CongratulationsCount: *input.CongratulationsCount}
		if err := s.Service.SetPolicy(r.Context(), c, p); err != nil {
			return err
		}
		respond(w, http.StatusOK, p)
		return nil
	})
	s.register(mux, "GET /v1/editor/stats", true, func(w http.ResponseWriter, r *http.Request, c news.Client) error {
		limit, offset, err := page(r)
		if err != nil {
			return err
		}
		v, err := s.Service.Repo.AllStatistics(r.Context(), limit, offset)
		if err != nil {
			return err
		}
		respond(w, http.StatusOK, map[string]any{"items": v})
		return nil
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, http.StatusNotFound, "not_found", "route not found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		start := time.Now()
		w.Header().Set("X-Request-ID", rand.Text())
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if recovered := recover(); recovered != nil {
				s.Logger.Error("request panic", "request_id", w.Header().Get("X-Request-ID"))
				if !writer.wroteHeader {
					s.fail(writer, r, 500, "internal", "internal server error")
				}
			}
			s.Logger.Info("http request", "request_id", w.Header().Get("X-Request-ID"), "method", r.Method, "route", r.Pattern, "status", writer.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		mux.ServeHTTP(writer, r)
	})
}

func (s *Server) register(mux *http.ServeMux, pattern string, editor bool, h handler) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 4096 {
			s.failure(w, r, news.ErrUnauthorized)
			return
		}
		c, err := s.Service.Repo.Authenticate(r.Context(), parts[1])
		if err != nil {
			s.failure(w, r, err)
			return
		}
		if editor && c.Role != "editor" {
			s.failure(w, r, news.ErrForbidden)
			return
		}
		if err = h(w, r, c); err != nil {
			s.failure(w, r, err)
		}
	})
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type badJSON struct{ err error }

func (e badJSON) Error() string { return "invalid JSON: " + e.err.Error() }
func (e badJSON) Unwrap() error { return e.err }

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return badJSON{errors.New("Content-Type must be application/json")}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return badJSON{err}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return badJSON{errors.New("exactly one JSON object is required")}
	}
	return nil
}

func page(r *http.Request) (int, int, error) {
	limit, offset := 50, 0
	var err error
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil {
			return 0, 0, news.ErrInvalid
		}
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil {
			return 0, 0, news.ErrInvalid
		}
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return 0, 0, news.ErrInvalid
	}
	return limit, offset, nil
}

func (s *Server) failure(w http.ResponseWriter, r *http.Request, err error) {
	var malformed badJSON
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		s.fail(w, r, 413, "too_large", "request body exceeds 64 KiB")
	case errors.As(err, &malformed):
		s.fail(w, r, 400, "bad_json", "invalid JSON object, fields or Content-Type")
	case errors.Is(err, news.ErrInvalid):
		s.fail(w, r, 422, "invalid", "input or query parameters are invalid")
	case errors.Is(err, news.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", "Bearer")
		s.fail(w, r, 401, "unauthorized", "handshake required or credentials invalid")
	case errors.Is(err, news.ErrForbidden):
		s.fail(w, r, 403, "forbidden", "access denied")
	case errors.Is(err, news.ErrNotFound):
		s.fail(w, r, 404, "not_found", "resource not found")
	case errors.Is(err, news.ErrConflict):
		s.fail(w, r, 409, "conflict", "resource already exists or request_id reused with different content")
	default:
		s.Logger.Error("request failed", "request_id", w.Header().Get("X-Request-ID"), "error_type", fmt.Sprintf("%T", err))
		s.fail(w, r, 500, "internal", "internal server error")
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": w.Header().Get("X-Request-ID")}})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}

const swaggerHTML = `<!doctype html><html lang="ru"><head><meta charset="utf-8"><title>Издание — API</title><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.31.0/swagger-ui.css"></head><body><div id="swagger-ui"></div><script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.31.0/swagger-ui-bundle.js"></script><script>SwaggerUIBundle({spec:%s,dom_id:'#swagger-ui',persistAuthorization:false});</script></body></html>`

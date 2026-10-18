package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const help = `client COMMAND [OPTIONS]

  submit --request-id ID --category TEXT --topic TEXT --body TEXT [--event-at RFC3339]
  submit --file PATH|-
  stats
  all-stats [--limit 100] [--offset 0]
  stories [--state collecting|published] [--category TEXT] [--limit 100] [--offset 0]
  story --id ID
  messages [--after 0] [--limit 100] [--unacked=true]
  ack --id NUMBER
  create-client --id ID --name TEXT --role staff|outstaff|editor --trust NUMBER
  rotate-key --id ID
  set-trust --id ID --trust NUMBER [--enabled=true]
  policy
  set-policy --threshold NUMBER --half-life-seconds NUMBER --congratulations-count NUMBER
  help

Login: EDITION_CLIENT_ID and EDITION_API_KEY.
Defaults: EDITION_URL=http://localhost:8080, EDITION_TIMEOUT=15s.
Retries: same --request-id and --event-at, or the same --file.
`

type operation struct {
	method string
	path   string
	body   []byte
}

type submission struct {
	RequestID string    `json:"request_id"`
	Category  string    `json:"category"`
	Topic     string    `json:"topic"`
	Body      string    `json:"body"`
	EventAt   time.Time `json:"event_at"`
}

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	return run(ctx, args, stdin, stdout, stderr, getenv, http.DefaultTransport)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, transport http.RoundTripper) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, _ = io.WriteString(stdout, help)
		return 0
	}
	op, err := parse(args, stdin, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "client:", err)
		return 2
	}
	baseURL := getenv("EDITION_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		_, _ = fmt.Fprintln(stderr, "client: EDITION_URL must be an HTTP(S) URL without credentials, query, or fragment")
		return 1
	}
	clientID, apiKey := getenv("EDITION_CLIENT_ID"), getenv("EDITION_API_KEY")
	if clientID == "" || apiKey == "" {
		_, _ = fmt.Fprintln(stderr, "client: EDITION_CLIENT_ID and EDITION_API_KEY are required")
		return 1
	}
	timeout := 15 * time.Second
	if value := getenv("EDITION_TIMEOUT"); value != "" {
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			_, _ = fmt.Fprintln(stderr, "client: EDITION_TIMEOUT must be a positive duration, for example 15s")
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpClient := &http.Client{
		Timeout:   timeout,
		Transport: transport,

		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	credentials, err := json.Marshal(struct {
		ClientID string `json:"client_id"`
		APIKey   string `json:"api_key"`
	}{clientID, apiKey})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "client: cannot encode credentials")
		return 1
	}
	baseURL = strings.TrimRight(base.String(), "/")
	body, status, err := request(ctx, httpClient, baseURL+"/v1/handshake", http.MethodPost, "", credentials)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "client: handshake:", err)
		return 1
	}
	if status < 200 || status >= 300 {

		_, _ = fmt.Fprintf(stderr, "client: handshake failed (HTTP %d)\n", status)
		return 1
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &session); err != nil || session.Token == "" {
		_, _ = fmt.Fprintln(stderr, "client: handshake returned an invalid session")
		return 1
	}
	body, status, err = request(ctx, httpClient, baseURL+op.path, op.method, session.Token, op.body)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "client:", err)
		return 1
	}
	if len(body) > 0 {
		if err := writeJSON(stdout, body); err != nil {
			_, _ = fmt.Fprintln(stderr, "client: cannot write response:", err)
			return 1
		}
	}
	if status < 200 || status >= 300 {
		_, _ = fmt.Fprintf(stderr, "client: request failed (HTTP %d)\n", status)
		return 1
	}
	return 0
}

func request(ctx context.Context, client *http.Client, endpoint, method, token string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTP request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	const maxResponse = 4 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponse {
		return nil, 0, errors.New("response exceeds 4 MiB")
	}
	data = bytes.TrimSpace(data)
	if len(data) > 0 && !json.Valid(data) {
		return nil, 0, fmt.Errorf("server returned a non-JSON response (HTTP %d)", resp.StatusCode)
	}
	return data, resp.StatusCode, nil
}

func writeJSON(w io.Writer, body []byte) error {
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, body, "", "  "); err != nil {
		return err
	}
	formatted.WriteByte('\n')
	_, err := w.Write(formatted.Bytes())
	return err
}

func parse(args []string, stdin io.Reader, stderr io.Writer) (operation, error) {
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, help) }
	op := operation{method: http.MethodGet}
	var id, name, role, category, topic, body, eventAt, file, requestID, state string
	var trust, threshold float64
	var enabled, unacked bool
	var after, halfLife int64
	var limit, offset, congratulations int
	listFlags := func() {
		flags.IntVar(&limit, "limit", 100, "items per page (1..100)")
		flags.IntVar(&offset, "offset", 0, "items to skip")
	}
	switch command {
	case "submit":
		flags.StringVar(&requestID, "request-id", "", "stable idempotency key")
		flags.StringVar(&category, "category", "", "exact category")
		flags.StringVar(&topic, "topic", "", "exact topic")
		flags.StringVar(&body, "body", "", "note text")
		flags.StringVar(&eventAt, "event-at", "", "event timestamp in RFC3339")
		flags.StringVar(&file, "file", "", "saved submission JSON path, or - for stdin")
	case "stats", "policy":
	case "all-stats":
		listFlags()
	case "stories":
		listFlags()
		flags.StringVar(&state, "state", "", "collecting or published")
		flags.StringVar(&category, "category", "", "exact category")
	case "story", "ack", "rotate-key":
		flags.StringVar(&id, "id", "", "resource ID")
	case "messages":
		flags.Int64Var(&after, "after", 0, "return messages with a greater ID")
		flags.IntVar(&limit, "limit", 100, "items per page (1..100)")
		flags.BoolVar(&unacked, "unacked", true, "return only unacknowledged messages")
	case "create-client":
		flags.StringVar(&id, "id", "", "client ID")
		flags.StringVar(&name, "name", "", "client name")
		flags.StringVar(&role, "role", "", "staff, outstaff, or editor")
		flags.Float64Var(&trust, "trust", 0, "initial trust (0..1)")
	case "set-trust":
		flags.StringVar(&id, "id", "", "client ID")
		flags.Float64Var(&trust, "trust", 0, "trust (0..1)")
		flags.BoolVar(&enabled, "enabled", true, "whether the client may authenticate")
	case "set-policy":
		flags.Float64Var(&threshold, "threshold", 0, "publication threshold")
		flags.Int64Var(&halfLife, "half-life-seconds", 0, "trust half-life in seconds")
		flags.IntVar(&congratulations, "congratulations-count", 0, "number of contributors to congratulate")
	default:
		return op, fmt.Errorf("unknown command %q; use client help", command)
	}
	if err := flags.Parse(args[1:]); err != nil {
		return op, err
	}
	if flags.NArg() != 0 {
		return op, errors.New("unexpected positional arguments; use client help")
	}
	seen := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	require := func(names ...string) error {
		for _, key := range names {
			if !seen[key] || strings.TrimSpace(flags.Lookup(key).Value.String()) == "" {
				return fmt.Errorf("--%s is required", key)
			}
		}
		return nil
	}
	encode := func(value any) (operation, error) {
		var err error
		op.body, err = json.Marshal(value)
		return op, err
	}
	query := url.Values{}
	switch command {
	case "submit":
		op.method, op.path = http.MethodPost, "/v1/notes"
		if file != "" {
			if len(seen) != 1 {
				return op, errors.New("--file cannot be combined with submission fields")
			}
			data, err := readSubmission(file, stdin)
			if err != nil {
				return op, err
			}
			op.body = data
			return op, nil
		}
		if err := require("request-id", "category", "topic", "body"); err != nil {
			return op, err
		}
		timestamp := time.Now().UTC()
		if eventAt != "" {
			var err error
			timestamp, err = time.Parse(time.RFC3339Nano, eventAt)
			if err != nil {
				return op, errors.New("--event-at must be an RFC3339 timestamp")
			}
		} else {
			_, _ = fmt.Fprintf(stderr, "submission event_at: %s; reuse this timestamp with the same --request-id for retries\n", timestamp.Format(time.RFC3339Nano))
		}
		return encode(submission{requestID, category, topic, body, timestamp.UTC()})
	case "stats":
		op.path = "/v1/stats/me"
	case "all-stats", "stories":
		if limit < 1 || limit > 100 || offset < 0 {
			return op, errors.New("--limit must be 1..100 and --offset must be nonnegative")
		}
		query.Set("limit", strconv.Itoa(limit))
		query.Set("offset", strconv.Itoa(offset))
		op.path = "/v1/editor/stats"
		if command == "stories" {
			op.path = "/v1/stories"
			if state != "" && state != "collecting" && state != "published" {
				return op, errors.New("--state must be collecting or published")
			}
			if state != "" {
				query.Set("state", state)
			}
			if category != "" {
				query.Set("category", category)
			}
		}
	case "story":
		if err := require("id"); err != nil {
			return op, err
		}
		op.path = "/v1/stories/" + url.PathEscape(id)
	case "messages":
		if after < 0 || limit < 1 || limit > 100 {
			return op, errors.New("--after must be nonnegative and --limit must be 1..100")
		}
		op.path = "/v1/messages"
		query.Set("after", strconv.FormatInt(after, 10))
		query.Set("limit", strconv.Itoa(limit))
		query.Set("unacked", strconv.FormatBool(unacked))
	case "ack":
		if err := require("id"); err != nil {
			return op, err
		}
		value, err := strconv.ParseInt(id, 10, 64)
		if err != nil || value < 1 {
			return op, errors.New("--id must be a positive integer")
		}
		op.method, op.path = http.MethodPost, "/v1/messages/"+strconv.FormatInt(value, 10)+"/ack"
	case "rotate-key":
		if err := require("id"); err != nil {
			return op, err
		}
		op.method, op.path = http.MethodPost, "/v1/editor/clients/"+url.PathEscape(id)+"/key"
	case "create-client", "set-trust":
		if err := require("id", "trust"); err != nil {
			return op, err
		}
		if math.IsNaN(trust) || math.IsInf(trust, 0) || trust < 0 || trust > 1 {
			return op, errors.New("--trust must be between 0 and 1")
		}
		if command == "create-client" {
			if err := require("name", "role"); err != nil {
				return op, err
			}
			if role != "staff" && role != "outstaff" && role != "editor" {
				return op, errors.New("--role must be staff, outstaff, or editor")
			}
			op.method, op.path = http.MethodPost, "/v1/editor/clients"
			return encode(struct {
				ID      string  `json:"id"`
				Name    string  `json:"name"`
				Role    string  `json:"role"`
				Trust   float64 `json:"trust"`
				Enabled bool    `json:"enabled"`
			}{id, name, role, trust, true})
		}
		op.method, op.path = http.MethodPatch, "/v1/editor/clients/"+url.PathEscape(id)
		return encode(struct {
			Trust   float64 `json:"trust"`
			Enabled bool    `json:"enabled"`
		}{trust, enabled})
	case "policy":
		op.path = "/v1/editor/policy"
	case "set-policy":
		if err := require("threshold", "half-life-seconds", "congratulations-count"); err != nil {
			return op, err
		}
		if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 || threshold > 1000000 || halfLife < 1 || halfLife > 31536000 || congratulations < 0 || congratulations > 1000 {
			return op, errors.New("policy requires threshold (0..1000000], half-life-seconds 1..31536000, congratulations-count 0..1000")
		}
		op.method, op.path = http.MethodPut, "/v1/editor/policy"
		return encode(struct {
			Threshold            float64 `json:"threshold"`
			HalfLifeSeconds      int64   `json:"half_life_seconds"`
			CongratulationsCount int     `json:"congratulations_count"`
		}{threshold, halfLife, congratulations})
	}
	if len(query) > 0 {
		op.path += "?" + query.Encode()
	}
	return op, nil
}

func readSubmission(path string, stdin io.Reader) ([]byte, error) {
	reader := stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open submission JSON: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	const maxSubmission = 100 << 10
	data, err := io.ReadAll(io.LimitReader(reader, maxSubmission+1))
	if err != nil {
		return nil, fmt.Errorf("read submission JSON: %w", err)
	}
	if len(data) > maxSubmission {
		return nil, errors.New("submission JSON exceeds 100 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value submission
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid submission JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("submission file must contain exactly one JSON object")
	}
	if strings.TrimSpace(value.RequestID) == "" || strings.TrimSpace(value.Category) == "" || strings.TrimSpace(value.Topic) == "" || strings.TrimSpace(value.Body) == "" || value.EventAt.IsZero() {
		return nil, errors.New("submission JSON requires request_id, category, topic, body, and event_at")
	}
	return data, nil
}

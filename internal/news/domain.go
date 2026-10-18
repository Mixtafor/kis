package news

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalid      = errors.New("invalid input")
	ErrUnauthorized = errors.New("handshake required or credentials invalid")
	ErrForbidden    = errors.New("editor access required")
	ErrNotFound     = errors.New("resource not found")
	ErrConflict     = errors.New("idempotency key reused with different content")
)

type Client struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Role    string  `json:"role"`
	Trust   float64 `json:"trust"`
	Enabled bool    `json:"enabled"`
}

type Policy struct {
	Threshold            float64 `json:"threshold"`
	HalfLifeSeconds      int64   `json:"half_life_seconds"`
	CongratulationsCount int     `json:"congratulations_count"`
}

func (p Policy) Validate() error {
	if math.IsNaN(p.Threshold) || math.IsInf(p.Threshold, 0) || p.Threshold <= 0 || p.Threshold > 1000000 || p.HalfLifeSeconds < 1 || p.HalfLifeSeconds > 31536000 || p.CongratulationsCount < 0 || p.CongratulationsCount > 1000 {
		return ErrInvalid
	}
	return nil
}

func (c Client) Validate() error {
	if !Text(c.ID, 100) || !Text(c.Name, 200) || (c.Role != "editor" && c.Role != "staff" && c.Role != "outstaff") || math.IsNaN(c.Trust) || math.IsInf(c.Trust, 0) || c.Trust < 0 || c.Trust > 1 {
		return ErrInvalid
	}
	return nil
}

func Text(s string, limit int) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= limit && !strings.ContainsRune(s, '\x00')
}

type Submission struct {
	RequestID string    `json:"request_id"`
	Category  string    `json:"category"`
	Topic     string    `json:"topic"`
	Body      string    `json:"body"`
	EventAt   time.Time `json:"event_at"`
}

func (n Submission) Validate(now time.Time) error {
	if !Text(n.RequestID, 100) || !Text(n.Category, 100) || !Text(n.Topic, 500) || !Text(n.Body, 10000) || n.EventAt.IsZero() || n.EventAt.After(now.Add(time.Minute)) {
		return ErrInvalid
	}
	return nil
}

type Receipt struct {
	Status     string    `json:"status"`
	NoteID     string    `json:"note_id"`
	StoryID    string    `json:"story_id"`
	ReceivedAt time.Time `json:"received_at"`
	Published  bool      `json:"published"`
	Reason     string    `json:"reason,omitempty"`
}

type Contribution struct {
	ClientID   string    `json:"client_id"`
	Name       string    `json:"name"`
	Trust      float64   `json:"trust"`
	EventAt    time.Time `json:"event_at"`
	ReceivedAt time.Time `json:"received_at"`
	Rank       int       `json:"rank"`
}

type Admission struct {
	Contribution    *Contribution
	Trust           float64
	Publish         bool
	Congratulations []Contribution
}

func Assess(policy Policy, existing []Contribution, client Client, note Submission, receivedAt time.Time, published bool) Admission {
	if published {
		return Admission{}
	}
	all := append([]Contribution(nil), existing...)
	decision := Admission{}
	first := true
	for _, source := range existing {
		if source.ClientID == client.ID {
			first = false
			break
		}
	}
	if first {
		vote := Contribution{ClientID: client.ID, Name: client.Name, Trust: client.Trust, EventAt: note.EventAt, ReceivedAt: receivedAt, Rank: len(existing) + 1}
		decision.Contribution = &vote
		all = append(all, vote)
	}
	decision.Trust = Score(all, receivedAt, policy.HalfLifeSeconds)
	decision.Publish = decision.Trust >= policy.Threshold
	if decision.Publish {
		for _, source := range all {
			if source.Rank <= policy.CongratulationsCount {
				decision.Congratulations = append(decision.Congratulations, source)
			}
		}
	}
	return decision
}

func Score(contributions []Contribution, now time.Time, halfLifeSeconds int64) float64 {
	if halfLifeSeconds <= 0 {
		return 0
	}
	total := 0.0
	for _, c := range contributions {
		age := math.Max(0, now.Sub(c.EventAt).Seconds())
		total += c.Trust * math.Exp2(-age/float64(halfLifeSeconds))
	}
	return total
}

type Story struct {
	ID           string         `json:"id"`
	Category     string         `json:"category"`
	Topic        string         `json:"topic"`
	State        string         `json:"state"`
	Trust        float64        `json:"trust"`
	PublishedAt  *time.Time     `json:"published_at"`
	Contributors []Contribution `json:"contributors"`
}

type Stats struct {
	ClientID         string  `json:"client_id"`
	Accepted         int64   `json:"accepted"`
	Published        int64   `json:"published"`
	PublicationShare float64 `json:"publication_share"`
	Stories          int64   `json:"stories"`
	Sensations       int64   `json:"sensations"`
	AverageRank      float64 `json:"average_rank"`
}

type Notification struct {
	ID           int64     `json:"id"`
	StoryID      string    `json:"story_id"`
	Rank         int       `json:"rank"`
	Message      string    `json:"message"`
	CreatedAt    time.Time `json:"created_at"`
	Acknowledged bool      `json:"acknowledged"`
}

type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Client    Client    `json:"client"`
}

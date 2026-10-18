package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"edition/internal/news"
)

type repositoryStub struct {
	Repository
	called bool
	err    error
}

func (r *repositoryStub) Submit(_ context.Context, id string, n news.Submission) (news.Receipt, error) {
	r.called = true
	return news.Receipt{Status: "accepted", NoteID: id, StoryID: n.Topic}, r.err
}
func (r *repositoryStub) CreateClient(context.Context, news.Client) (string, error) {
	r.called = true
	return "generated-key", r.err
}
func (r *repositoryStub) SetPolicy(context.Context, news.Policy) error { r.called = true; return r.err }
func (r *repositoryStub) Statistics(_ context.Context, id string) (news.Stats, error) {
	r.called = true
	return news.Stats{ClientID: id}, r.err
}

func TestSubmitGuardsAndFailures(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	valid := news.Submission{RequestID: "request", Category: "city", Topic: "bridge", Body: "open", EventAt: now}
	failure := errors.New("database unavailable")
	for _, tt := range []struct {
		name, role       string
		n                news.Submission
		repoErr, wantErr error
		called           bool
	}{
		{"staff accepted", "staff", valid, nil, nil, true},
		{"outstaff accepted", "outstaff", valid, nil, nil, true},
		{"editor forbidden", "editor", valid, nil, news.ErrForbidden, false},
		{"invalid blocked", "staff", news.Submission{}, nil, news.ErrInvalid, false},
		{"failure propagated", "staff", valid, failure, failure, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &repositoryStub{err: tt.repoErr}
			s := Service{Repo: r, Now: func() time.Time { return now }}
			_, err := s.Submit(context.Background(), news.Client{ID: "source", Role: tt.role}, tt.n)
			if !errors.Is(err, tt.wantErr) || r.called != tt.called {
				t.Fatalf("err=%v called=%v", err, r.called)
			}
		})
	}
}

func TestEditorAndStatisticsAccess(t *testing.T) {
	ctx := context.Background()
	editor := news.Client{ID: "editor", Role: "editor"}
	source := news.Client{ID: "source", Role: "outstaff"}
	for _, tt := range []struct {
		name    string
		actor   news.Client
		target  string
		allowed bool
	}{
		{"self", source, "source", true}, {"other denied", source, "other", false}, {"editor", editor, "other", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &repositoryStub{}
			s := Service{Repo: r}
			_, err := s.Statistics(ctx, tt.actor, tt.target)
			if r.called != tt.allowed || (!tt.allowed && !errors.Is(err, news.ErrForbidden)) {
				t.Fatalf("err=%v called=%v", err, r.called)
			}
		})
	}
	r := &repositoryStub{}
	s := Service{Repo: r}
	client := news.Client{ID: "new", Name: "New", Role: "staff", Trust: 1}
	if _, err := s.CreateClient(ctx, source, client); !errors.Is(err, news.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.CreateClient(ctx, editor, news.Client{}); !errors.Is(err, news.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.CreateClient(ctx, editor, client); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPolicy(ctx, source, news.Policy{Threshold: 1, HalfLifeSeconds: 1}); !errors.Is(err, news.ErrForbidden) {
		t.Fatal(err)
	}
	if err := s.SetPolicy(ctx, editor, news.Policy{}); !errors.Is(err, news.ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.SetPolicy(ctx, editor, news.Policy{Threshold: 1, HalfLifeSeconds: 1}); err != nil {
		t.Fatal(err)
	}
}

package app

import (
	"context"
	"time"

	"edition/internal/news"
)

type Repository interface {
	Handshake(context.Context, string, string) (news.Session, error)
	Authenticate(context.Context, string) (news.Client, error)
	Submit(context.Context, string, news.Submission) (news.Receipt, error)
	CreateClient(context.Context, news.Client) (string, error)
	RotateClientKey(context.Context, string) (string, error)
	UpdateClient(context.Context, string, float64, bool) (news.Client, error)
	Policy(context.Context) (news.Policy, error)
	SetPolicy(context.Context, news.Policy) error
	Story(context.Context, string) (news.Story, error)
	Stories(context.Context, string, string, int, int) ([]news.Story, error)
	Statistics(context.Context, string) (news.Stats, error)
	AllStatistics(context.Context, int, int) ([]news.Stats, error)
	Notifications(context.Context, string, int64, bool, int) ([]news.Notification, error)
	Acknowledge(context.Context, string, int64) error
	Ping(context.Context) error
}

type Service struct {
	Repo Repository
	Now  func() time.Time
}

func (s Service) Submit(ctx context.Context, c news.Client, n news.Submission) (news.Receipt, error) {
	if c.Role == "editor" {
		return news.Receipt{}, news.ErrForbidden
	}
	if err := n.Validate(s.Now()); err != nil {
		return news.Receipt{}, err
	}
	return s.Repo.Submit(ctx, c.ID, n)
}

func (s Service) CreateClient(ctx context.Context, actor news.Client, c news.Client) (string, error) {
	if actor.Role != "editor" {
		return "", news.ErrForbidden
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	return s.Repo.CreateClient(ctx, c)
}

func (s Service) SetPolicy(ctx context.Context, actor news.Client, p news.Policy) error {
	if actor.Role != "editor" {
		return news.ErrForbidden
	}
	if err := p.Validate(); err != nil {
		return err
	}
	return s.Repo.SetPolicy(ctx, p)
}

func (s Service) Statistics(ctx context.Context, actor news.Client, id string) (news.Stats, error) {
	if actor.ID != id && actor.Role != "editor" {
		return news.Stats{}, news.ErrForbidden
	}
	return s.Repo.Statistics(ctx, id)
}

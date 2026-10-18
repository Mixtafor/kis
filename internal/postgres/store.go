package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"edition/internal/auth"
	"edition/internal/news"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool   *pgxpool.Pool
	Tokens *auth.Tokens
}

func New(ctx context.Context, dsn string, tokens *auth.Tokens) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL configuration: %w", err)
	}
	cfg.MaxConns = 10
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "15000"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	return &Store{Pool: p, Tokens: tokens}, nil
}

func Digest(value string) string { v := sha256.Sum256([]byte(value)); return hex.EncodeToString(v[:]) }

func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

func (s *Store) EnsureClient(ctx context.Context, c news.Client, key string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO clients(id,name,role,trust,enabled,key_hash) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO NOTHING`, c.ID, c.Name, c.Role, c.Trust, c.Enabled, Digest(key))
	return err
}

func scanClient(row pgx.Row) (news.Client, error) {
	var c news.Client
	err := row.Scan(&c.ID, &c.Name, &c.Role, &c.Trust, &c.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, news.ErrNotFound
	}
	return c, err
}

func (s *Store) Handshake(ctx context.Context, id, key string) (news.Session, error) {
	var c news.Client
	var hash string
	err := s.Pool.QueryRow(ctx, `SELECT id,name,role,trust,enabled,key_hash FROM clients WHERE id=$1`, id).Scan(&c.ID, &c.Name, &c.Role, &c.Trust, &c.Enabled, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return news.Session{}, news.ErrUnauthorized
	}
	if err != nil {
		return news.Session{}, err
	}
	if !c.Enabled || subtle.ConstantTimeCompare([]byte(hash), []byte(Digest(key))) != 1 {
		return news.Session{}, news.ErrUnauthorized
	}
	if s.Tokens == nil {
		return news.Session{}, errors.New("JWT issuer is not configured")
	}
	token, expires, err := s.Tokens.Issue(c.ID)
	if err != nil {
		return news.Session{}, err
	}
	return news.Session{Token: token, ExpiresAt: expires, Client: c}, nil
}

func (s *Store) Authenticate(ctx context.Context, token string) (news.Client, error) {
	if s.Tokens == nil {
		return news.Client{}, news.ErrUnauthorized
	}
	id, err := s.Tokens.Subject(token)
	if err != nil {
		return news.Client{}, err
	}
	c, err := scanClient(s.Pool.QueryRow(ctx, `SELECT id,name,role,trust,enabled FROM clients WHERE id=$1 AND enabled`, id))
	if errors.Is(err, news.ErrNotFound) {
		return c, news.ErrUnauthorized
	}
	return c, err
}

func (s *Store) CreateClient(ctx context.Context, c news.Client) (string, error) {
	key := rand.Text() + rand.Text()
	_, err := s.Pool.Exec(ctx, `INSERT INTO clients(id,name,role,trust,enabled,key_hash) VALUES($1,$2,$3,$4,$5,$6)`, c.ID, c.Name, c.Role, c.Trust, c.Enabled, Digest(key))
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return "", news.ErrConflict
	}
	return key, err
}

func (s *Store) RotateClientKey(ctx context.Context, id string) (string, error) {
	key := rand.Text() + rand.Text()
	tag, err := s.Pool.Exec(ctx, `UPDATE clients SET key_hash=$2 WHERE id=$1`, id, Digest(key))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", news.ErrNotFound
	}
	return key, nil
}

func (s *Store) UpdateClient(ctx context.Context, id string, trust float64, enabled bool) (news.Client, error) {
	return scanClient(s.Pool.QueryRow(ctx, `UPDATE clients SET trust=$2,enabled=$3 WHERE id=$1 RETURNING id,name,role,trust,enabled`, id, trust, enabled))
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readPolicy(ctx context.Context, q querier, lock bool) (news.Policy, error) {
	query := `SELECT threshold,half_life_seconds,congratulations_count FROM editorial_policy WHERE singleton`
	if lock {
		query += ` FOR SHARE`
	}
	var p news.Policy
	err := q.QueryRow(ctx, query).Scan(&p.Threshold, &p.HalfLifeSeconds, &p.CongratulationsCount)
	return p, err
}

func (s *Store) Policy(ctx context.Context) (news.Policy, error) {
	return readPolicy(ctx, s.Pool, false)
}

func (s *Store) SetPolicy(ctx context.Context, p news.Policy) error {
	_, err := s.Pool.Exec(ctx, `UPDATE editorial_policy SET threshold=$1,half_life_seconds=$2,congratulations_count=$3 WHERE singleton`, p.Threshold, p.HalfLifeSeconds, p.CongratulationsCount)
	return err
}

func contributions(ctx context.Context, q querier, id string) ([]news.Contribution, error) {
	rows, err := q.Query(ctx, `SELECT client_id,source_name,trust,event_at,received_at,rank FROM contributions WHERE story_id=$1 ORDER BY rank`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]news.Contribution, 0)
	for rows.Next() {
		var c news.Contribution
		if err = rows.Scan(&c.ClientID, &c.Name, &c.Trust, &c.EventAt, &c.ReceivedAt, &c.Rank); err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

func (s *Store) Submit(ctx context.Context, clientID string, n news.Submission) (news.Receipt, error) {
	var receipt news.Receipt
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return receipt, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	lockKey, _ := json.Marshal([]string{clientID, n.RequestID})
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, string(lockKey)); err != nil {
		return receipt, err
	}
	n.EventAt = n.EventAt.UTC()
	payload, err := json.Marshal(n)
	if err != nil {
		return receipt, err
	}
	hash := Digest(string(payload))
	var originalHash string
	err = tx.QueryRow(ctx, `SELECT id,story_id,received_at,published,payload_hash FROM notes WHERE client_id=$1 AND request_id=$2`, clientID, n.RequestID).Scan(&receipt.NoteID, &receipt.StoryID, &receipt.ReceivedAt, &receipt.Published, &originalHash)
	if err == nil {
		if originalHash != hash {
			return news.Receipt{}, news.ErrConflict
		}
		receipt.Status = "duplicate"
		return receipt, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return receipt, err
	}
	p, err := readPolicy(ctx, tx, true)
	if err != nil {
		return receipt, err
	}
	c, err := scanClient(tx.QueryRow(ctx, `SELECT id,name,role,trust,enabled FROM clients WHERE id=$1 FOR SHARE`, clientID))
	if err != nil {
		return receipt, err
	}
	if !c.Enabled {
		return receipt, news.ErrUnauthorized
	}
	if c.Role == "editor" {
		return receipt, news.ErrForbidden
	}
	storyID := rand.Text()
	if _, err = tx.Exec(ctx, `INSERT INTO stories(id,category,topic,half_life_seconds) VALUES($1,$2,$3,$4) ON CONFLICT(category,topic) DO NOTHING`, storyID, n.Category, n.Topic, p.HalfLifeSeconds); err != nil {
		return receipt, err
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM stories WHERE category=$1 AND topic=$2 FOR UPDATE`, n.Category, n.Topic).Scan(&storyID); err != nil {
		return receipt, err
	}
	var now time.Time
	var alreadyPublished bool
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp(),EXISTS(SELECT 1 FROM publications WHERE story_id=$1)`, storyID).Scan(&now, &alreadyPublished); err != nil {
		return receipt, err
	}
	if err = n.Validate(now); err != nil {
		return receipt, err
	}
	receipt = news.Receipt{Status: "accepted", NoteID: rand.Text(), StoryID: storyID, ReceivedAt: now, Published: alreadyPublished}
	if _, err = tx.Exec(ctx, `INSERT INTO notes(id,client_id,request_id,payload_hash,story_id,body,event_at,received_at,published) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, receipt.NoteID, clientID, n.RequestID, hash, storyID, n.Body, n.EventAt, now, alreadyPublished); err != nil {
		return receipt, err
	}
	if !alreadyPublished {
		if _, err = tx.Exec(ctx, `UPDATE stories SET half_life_seconds=$2 WHERE id=$1`, storyID, p.HalfLifeSeconds); err != nil {
			return receipt, err
		}
		items, e := contributions(ctx, tx, storyID)
		if e != nil {
			return receipt, e
		}
		decision := news.Assess(p, items, c, n, now, false)
		if vote := decision.Contribution; vote != nil {
			if _, err = tx.Exec(ctx, `INSERT INTO contributions(story_id,client_id,note_id,source_name,trust,event_at,received_at,rank) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, storyID, clientID, receipt.NoteID, vote.Name, vote.Trust, vote.EventAt, vote.ReceivedAt, vote.Rank); err != nil {
				return receipt, err
			}
		}
		if decision.Publish {
			if _, err = tx.Exec(ctx, `INSERT INTO publications(story_id,published_at,trust,threshold,half_life_seconds,congratulations_count) VALUES($1,$2,$3,$4,$5,$6)`, storyID, now, decision.Trust, p.Threshold, p.HalfLifeSeconds, p.CongratulationsCount); err != nil {
				return receipt, err
			}
			if len(decision.Congratulations) > 0 {
				// Allocate notification IDs in commit order so an after cursor cannot skip them.
				// Two-key advisory locks are separate from the request-id locks above.
				if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(0, 1)`); err != nil {
					return receipt, err
				}
			}
			for _, source := range decision.Congratulations {
				message := fmt.Sprintf("Поздравляем, ваша заметка попала в печать. Вы были %d-м, кто сказал нам об этой новости.", source.Rank)
				if _, err = tx.Exec(ctx, `INSERT INTO notifications(client_id,story_id,rank,message,created_at) VALUES($1,$2,$3,$4,$5)`, source.ClientID, storyID, source.Rank, message, now); err != nil {
					return receipt, err
				}
			}
			receipt.Published = true
			if _, err = tx.Exec(ctx, `UPDATE notes SET published=true WHERE id=$1`, receipt.NoteID); err != nil {
				return receipt, err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return news.Receipt{}, fmt.Errorf("commit note: %w", err)
	}
	return receipt, nil
}

func story(ctx context.Context, q querier, id string, now time.Time) (news.Story, error) {
	var v news.Story
	var frozenScore *float64
	var halfLife int64
	err := q.QueryRow(ctx, `SELECT s.id,s.category,s.topic,p.published_at,p.trust,s.half_life_seconds FROM stories s LEFT JOIN publications p ON p.story_id=s.id WHERE s.id=$1`, id).Scan(&v.ID, &v.Category, &v.Topic, &v.PublishedAt, &frozenScore, &halfLife)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, news.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	v.Contributors, err = contributions(ctx, q, id)
	if err != nil {
		return v, err
	}
	v.State = "collecting"
	v.Trust = news.Score(v.Contributors, now, halfLife)
	if frozenScore != nil {
		v.State = "published"
		v.Trust = *frozenScore
	}
	return v, nil
}

func (s *Store) Story(ctx context.Context, id string) (news.Story, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return news.Story{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return news.Story{}, err
	}
	v, err := story(ctx, tx, id, now)
	if err != nil {
		return v, err
	}
	return v, tx.Commit(ctx)
}

func (s *Store) Stories(ctx context.Context, state, category string, limit, offset int) ([]news.Story, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `SELECT s.id FROM stories s LEFT JOIN publications p ON p.story_id=s.id WHERE ($1='' OR ($1='published' AND p.story_id IS NOT NULL) OR ($1='collecting' AND p.story_id IS NULL)) AND ($2='' OR s.category=$2) ORDER BY s.id LIMIT $3 OFFSET $4`, state, category, limit, offset)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	items := make([]news.Story, 0, len(ids))
	for _, id := range ids {
		v, e := story(ctx, tx, id, now)
		if e != nil {
			return nil, e
		}
		items = append(items, v)
	}
	return items, tx.Commit(ctx)
}

const statisticsQuery = `SELECT c.id,
(SELECT count(*) FROM notes n WHERE n.client_id=c.id),
(SELECT count(*) FROM contributions k JOIN publications p ON p.story_id=k.story_id WHERE k.client_id=c.id),
(SELECT count(DISTINCT n.story_id) FROM notes n WHERE n.client_id=c.id),
(SELECT count(*) FROM contributions k JOIN publications p ON p.story_id=k.story_id WHERE k.client_id=c.id AND k.rank=1),
coalesce((SELECT avg(k.rank)::float8 FROM contributions k JOIN publications p ON p.story_id=k.story_id WHERE k.client_id=c.id),0)
FROM clients c`

func scanStats(row pgx.Row) (news.Stats, error) {
	var v news.Stats
	err := row.Scan(&v.ClientID, &v.Accepted, &v.Published, &v.Stories, &v.Sensations, &v.AverageRank)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, news.ErrNotFound
	}
	if v.Accepted > 0 {
		v.PublicationShare = float64(v.Published) / float64(v.Accepted)
	}
	return v, err
}

func (s *Store) Statistics(ctx context.Context, id string) (news.Stats, error) {
	return scanStats(s.Pool.QueryRow(ctx, statisticsQuery+` WHERE c.id=$1`, id))
}

func (s *Store) AllStatistics(ctx context.Context, limit, offset int) ([]news.Stats, error) {
	rows, err := s.Pool.Query(ctx, statisticsQuery+` ORDER BY c.id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]news.Stats, 0)
	for rows.Next() {
		v, e := scanStats(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func (s *Store) Notifications(ctx context.Context, clientID string, after int64, unacked bool, limit int) ([]news.Notification, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,story_id,rank,message,created_at,acknowledged FROM notifications WHERE client_id=$1 AND id>$2 AND (NOT $3 OR NOT acknowledged) ORDER BY id LIMIT $4`, clientID, after, unacked, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]news.Notification, 0)
	for rows.Next() {
		var v news.Notification
		if err = rows.Scan(&v.ID, &v.StoryID, &v.Rank, &v.Message, &v.CreatedAt, &v.Acknowledged); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func (s *Store) Acknowledge(ctx context.Context, clientID string, id int64) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE notifications SET acknowledged=true WHERE client_id=$1 AND id=$2`, clientID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return news.ErrNotFound
	}
	return nil
}

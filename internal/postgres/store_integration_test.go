package postgres

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	"edition/internal/news"
	"edition/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run against PostgreSQL with TEST_DATABASE_URL; all data stays in a temporary schema.
func TestNotificationsCursorAcrossConcurrentPublications(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the PostgreSQL regression test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	control, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close(context.Background()) }()
	schema := "notification_test_" + strings.ToLower(rand.Text())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = control.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := control.Exec(cleanup, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "20000"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Cancel publishers before closing the pool, including on assertion failures.
	defer cancel()
	store := &Store{Pool: pool}
	ddl, err := migrations.Files.ReadFile("000001_newsroom.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatal(err)
	}
	if err = store.SetPolicy(ctx, news.Policy{Threshold: 1.1, HalfLifeSeconds: 21600, CongratulationsCount: 1}); err != nil {
		t.Fatal(err)
	}
	for _, client := range []news.Client{
		{ID: "reader", Name: "Reader", Role: "outstaff", Trust: .7, Enabled: true},
		{ID: "publisher", Name: "Publisher", Role: "outstaff", Trust: .5, Enabled: true},
	} {
		if err = store.EnsureClient(ctx, client, "integration-test-key"); err != nil {
			t.Fatal(err)
		}
	}
	notes := []news.Submission{
		{RequestID: "first", Category: "test", Topic: "first", Body: "first", EventAt: time.Now()},
		{RequestID: "second", Category: "test", Topic: "second", Body: "second", EventAt: time.Now()},
	}
	storyIDs := make([]string, len(notes))
	for i, note := range notes {
		receipt, err := store.Submit(ctx, "reader", note)
		if err != nil {
			t.Fatal(err)
		}
		storyIDs[i] = receipt.StoryID
	}
	// Pause the first publisher AFTER its notification ID is allocated, before commit.
	// The second publisher must not expose a larger ID while the first is paused.
	if _, err = pool.Exec(ctx, `
CREATE FUNCTION pause_first_notification() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.story_id = (SELECT id FROM stories WHERE topic = 'first') THEN
        PERFORM pg_advisory_xact_lock(hashtextextended(TG_TABLE_SCHEMA, 0));
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER pause_first_notification AFTER INSERT ON notifications
FOR EACH ROW EXECUTE FUNCTION pause_first_notification();`); err != nil {
		t.Fatal(err)
	}
	if _, err = control.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = control.Exec(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, schema)
	}()
	publish := func(note news.Submission) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := store.Submit(ctx, "publisher", note)
			done <- err
		}()
		return done
	}
	waitBlocked := func(count int, done <-chan error) {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting int
			if err := control.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory'`, schema).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting >= count {
				return
			}
			select {
			case err := <-done:
				t.Fatalf("publisher finished before the earlier notification could commit: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-ticker.C:
			}
		}
	}
	first := publish(notes[0])
	waitBlocked(1, first)
	second := publish(notes[1])
	waitBlocked(2, second)
	visible, err := store.Notifications(ctx, "reader", 0, false, 100)
	if err != nil || len(visible) != 0 {
		t.Fatalf("notifications visible before first commit: %v, err=%v", visible, err)
	}
	if _, err = control.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, schema); err != nil {
		t.Fatal(err)
	}
	for _, done := range []<-chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for _, unacked := range []bool{false, true} {
		var after int64
		for _, storyID := range storyIDs {
			page, err := store.Notifications(ctx, "reader", after, unacked, 1)
			if err != nil || len(page) != 1 || page[0].StoryID != storyID || page[0].ID <= after {
				t.Fatalf("cursor skipped or reordered a notification: %v, err=%v", page, err)
			}
			after = page[0].ID
		}
		page, err := store.Notifications(ctx, "reader", after, unacked, 1)
		if err != nil || len(page) != 0 {
			t.Fatalf("unexpected extra notifications: %v, err=%v", page, err)
		}
	}
}

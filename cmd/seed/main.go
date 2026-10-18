package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"edition/internal/news"
	"edition/internal/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	key := os.Getenv("DEMO_API_KEY")
	if key == "" {
		key = "demo-only-shared-api-key-2026"
	}
	if len(key) < 16 {
		return errors.New("DEMO_API_KEY must have at least 16 characters")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := postgres.New(ctx, dsn, nil)
	if err != nil {
		return err
	}
	defer s.Pool.Close()
	clients := []news.Client{
		{ID: "sacharissa", Name: "Сахарисса Крипслок", Role: "staff", Trust: 1, Enabled: true},
		{ID: "tonny", Name: "Избирательно Честный Тонни", Role: "outstaff", Trust: .25, Enabled: true},
		{ID: "pew", Name: "Иногда Слепой Пью", Role: "outstaff", Trust: .35, Enabled: true},
		{ID: "third", Name: "Третий внештатный корреспондент", Role: "outstaff", Trust: .2, Enabled: true},
	}
	for _, c := range clients {
		if err = s.EnsureClient(ctx, c, key); err != nil {
			return fmt.Errorf("seed %s: %w", c.ID, err)
		}
	}
	fmt.Println("Demo clients prepared: sacharissa, tonny, pew, third. Existing clients preserved.")
	return nil
}

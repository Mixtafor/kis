package main

import (
	"errors"
	"fmt"
	"os"

	"edition/internal/dbmigration"

	"github.com/golang-migrate/migrate/v4"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migration:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if len(os.Args) > 2 {
		return errors.New("usage: migrate [up|down|version]")
	}
	m, err := dbmigration.Open(dsn)
	if err != nil {
		return err
	}
	switch command {
	case "up":
		err = m.Up()
	case "down":
		err = m.Steps(-1)
	case "version":
		var version uint
		var dirty bool
		version, dirty, err = m.Version()
		if err == nil {
			fmt.Printf("version=%d dirty=%t\n", version, dirty)
		}
	default:
		err = errors.New("usage: migrate [up|down|version]")
	}
	if errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}
	srcErr, dbErr := m.Close()
	return errors.Join(err, srcErr, dbErr)
}

package dbmigration

import (
	"errors"
	"fmt"

	"edition/migrations"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func Open(dsn string) (*migrate.Migrate, error) {
	source, err := iofs.New(migrations.Files, ".")
	if err != nil {
		return nil, fmt.Errorf("open embedded SQL: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	if err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("initialize golang-migrate: %w", err)
	}
	return m, nil
}

func Up(dsn string) error {
	m, err := Open(dsn)
	if err != nil {
		return err
	}
	err = m.Up()
	srcErr, dbErr := m.Close()
	if errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}
	return errors.Join(err, srcErr, dbErr)
}

package config

import (
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"time"
)

type Config struct {
	DatabaseURL, HTTPAddr, EditorAPIKey, JWTSecret string
	SessionTTL                                     time.Duration
	LogLevel                                       slog.Level
}

func Load() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), HTTPAddr: os.Getenv("HTTP_ADDR"), EditorAPIKey: os.Getenv("EDITOR_API_KEY"), JWTSecret: os.Getenv("JWT_SECRET"), SessionTTL: 24 * time.Hour}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return c, errors.New("HTTP_ADDR must be host:port")
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" || u.Path == "" {
		return c, errors.New("DATABASE_URL must be a PostgreSQL URL with host and database")
	}
	if len(c.EditorAPIKey) < 16 || len(c.EditorAPIKey) > 256 {
		return c, errors.New("EDITOR_API_KEY must have 16..256 characters")
	}
	if len(c.JWTSecret) < 32 || len(c.JWTSecret) > 256 {
		return c, errors.New("JWT_SECRET must have 32..256 characters")
	}
	if value := os.Getenv("SESSION_TTL"); value != "" {
		c.SessionTTL, err = time.ParseDuration(value)
		if err != nil || c.SessionTTL < time.Minute || c.SessionTTL > 30*24*time.Hour {
			return c, errors.New("SESSION_TTL must be between 1m and 720h")
		}
	}
	if value := os.Getenv("LOG_LEVEL"); value != "" {
		if err = c.LogLevel.UnmarshalText([]byte(value)); err != nil {
			return c, errors.New("LOG_LEVEL must be debug, info, warn or error")
		}
	}
	return c, nil
}

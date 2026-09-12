// Package config loads service configuration from the environment.
package config

import (
	"errors"
	"os"
)

// Config is the runtime configuration of the API server.
type Config struct {
	// HTTPAddr is the listen address of the HTTP server (default ":8080").
	HTTPAddr string
	// DatabaseURL is the PostgreSQL connection string (required).
	DatabaseURL string
}

// FromEnv reads configuration from HTTP_ADDR and DATABASE_URL.
func FromEnv() (Config, error) {
	cfg := Config{
		HTTPAddr:    os.Getenv("HTTP_ADDR"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	return cfg, nil
}

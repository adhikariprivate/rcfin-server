package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// loadDotEnv reads single-line KEY=VALUE entries from the optional local
// dotenv file. Variables already supplied by the process environment take
// precedence. Values are never included in errors or logs.
func loadDotEnv(filename string) error {
	file, err := os.Open(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open dotenv file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("invalid dotenv assignment on line %d", lineNumber)
		}
		value = strings.TrimSpace(value)
		if value != "" && (value[0] == '\'' || value[0] == '"') {
			quote := value[0]
			end := -1
			for i := 1; i < len(value); i++ {
				if quote == '"' && value[i] == '\\' {
					i++
					continue
				}
				if value[i] == quote {
					end = i
					break
				}
			}
			if end < 0 || strings.TrimSpace(value[end+1:]) != "" && !strings.HasPrefix(strings.TrimSpace(value[end+1:]), "#") {
				return fmt.Errorf("unterminated dotenv value on line %d", lineNumber)
			}
			value = value[1:end]
			if quote == '"' {
				unquoted, unquoteErr := strconv.Unquote("\"" + value + "\"")
				if unquoteErr != nil {
					return fmt.Errorf("invalid quoted dotenv value on line %d", lineNumber)
				}
				value = unquoted
			}
		} else if comment := strings.Index(value, " #"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set dotenv variable %s: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read dotenv file: %w", err)
	}
	return nil
}

// applyProviderCredentialAliases maps conventional unprefixed .env names to
// the RCFin environment names used by the example config. Explicit RCFIN_*
// process variables always win.
func applyProviderCredentialAliases() error {
	aliases := []struct {
		target  string
		sources []string
	}{
		{target: "RCFIN_TMDB_API_KEY", sources: []string{"TMDB_READ_ACCESS_TOKEN", "TMDB_API_KEY"}},
		{target: "RCFIN_OPENSUBTITLES_API_KEY", sources: []string{"OPENSUBTITLES_API_KEY"}},
		{target: "RCFIN_OPENSUBTITLES_USERNAME", sources: []string{"OPENSUBTITLES_USERNAME"}},
		{target: "RCFIN_OPENSUBTITLES_PASSWORD", sources: []string{"OPENSUBTITLES_PASSWORD"}},
	}
	for _, alias := range aliases {
		if value := strings.TrimSpace(os.Getenv(alias.target)); value != "" {
			continue
		}
		for _, source := range alias.sources {
			if value := strings.TrimSpace(os.Getenv(source)); value != "" {
				if err := os.Setenv(alias.target, value); err != nil {
					return fmt.Errorf("map provider environment variable %s: %w", source, err)
				}
				break
			}
		}
	}
	return nil
}

func initializeEnvironment() error {
	filename := strings.TrimSpace(os.Getenv("RCFIN_ENV_FILE"))
	if filename == "" {
		filename = ".env"
	}
	if err := loadDotEnv(filename); err != nil {
		return err
	}
	return applyProviderCredentialAliases()
}

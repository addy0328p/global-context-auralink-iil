// apiconfig prepares private local demo credentials without printing secrets.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/contextapi"
)

func main() {
	if err := run(); err != nil {
		slog.Error("API setup failed", "reason", err.Error())
		os.Exit(1)
	}
	slog.Info("private API configuration ready; secrets remain in ignored .env")
}
func run() error {
	b, err := os.ReadFile(".env")
	if err != nil {
		return errors.New("run from the repository with an existing private .env")
	}
	values, err := godotenv.Unmarshal(string(b))
	if err != nil {
		return errors.New("could not parse private environment configuration")
	}
	path := values["DEMO_SUBJECT_MANIFEST"]
	if path == "" {
		path = "artifacts/exports/historical_dataset/manifest.json"
	}
	subjects, err := contextapi.ReadSubjects(path)
	if err != nil {
		return err
	}
	updates := map[string]string{}
	for _, name := range []string{"DEMO_API_BEARER_TOKEN", "SARVAM_TOOL_BEARER_TOKEN"} {
		if values[name] != "" {
			continue
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return errors.New("could not generate private API credentials")
		}
		updates[name] = hex.EncodeToString(random[:])
		values[name] = updates[name]
	}
	if err := (contextapi.Config{DemoToken: values["DEMO_API_BEARER_TOKEN"], ToolToken: values["SARVAM_TOOL_BEARER_TOKEN"], Subjects: subjects}).Validate(); err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	// Preserve unrelated environment lines (including the PostgreSQL password).
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		for name, value := range updates {
			if strings.HasPrefix(trim, name+"=") {
				lines[i] = name + "=" + value
				delete(updates, name)
			}
		}
	}
	text = strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	for _, name := range []string{"DEMO_API_BEARER_TOKEN", "SARVAM_TOOL_BEARER_TOKEN"} {
		if value, ok := updates[name]; ok {
			text += name + "=" + value + "\n"
		}
	}
	f, err := os.CreateTemp(".", ".env.setup-*")
	if err != nil {
		return errors.New("could not prepare private configuration")
	}
	temp := f.Name()
	defer os.Remove(temp)
	if f.Chmod(0600) != nil {
		f.Close()
		return errors.New("could not restrict private configuration permissions")
	}
	if _, err = f.WriteString(text); err != nil {
		f.Close()
		return errors.New("could not write private configuration")
	}
	if f.Sync() != nil {
		f.Close()
		return errors.New("could not save private configuration")
	}
	if f.Close() != nil {
		return errors.New("could not close private configuration")
	}
	if os.Rename(temp, ".env") != nil {
		return errors.New("could not replace private configuration")
	}
	return nil
}

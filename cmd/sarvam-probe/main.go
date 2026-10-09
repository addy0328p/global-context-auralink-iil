// sarvam-probe fetches short-lived connection URLs for both configured agents.
// It never connects a WebSocket, sends memory/audio, or starts a customer call.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/sarvam"
)

type result struct {
	Role              string `json:"role"`
	Version           int    `json:"configured_version"`
	HTTPStatus        int    `json:"http_status"`
	ConnectionGranted bool   `json:"signed_connection_granted"`
	Code              string `json:"result"`
	Milliseconds      int64  `json:"elapsed_ms"`
}
type report struct {
	CheckedAt    time.Time `json:"checked_at"`
	SDKVersion   string    `json:"sdk_contract_version"`
	AudioStarted bool      `json:"audio_session_started"`
	MemorySent   bool      `json:"customer_memory_sent"`
	Agents       []result  `json:"agents"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("Sarvam probe incomplete", "reason", err.Error())
		os.Exit(1)
	}
}
func run() error {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return errors.New("could not load private environment configuration")
	}
	cfg, err := sarvam.LoadConfig()
	if err != nil {
		return err
	}
	client, err := sarvam.NewClient(cfg)
	if err != nil {
		return err
	}
	r := report{CheckedAt: time.Now().UTC(), SDKVersion: sarvam.SDKContractVersion, Agents: []result{}}
	allOK := true
	for _, role := range []string{"buyer", "seller"} {
		agent, _ := cfg.Agent(role)
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := client.ConnectionURL(ctx, role)
		cancel()
		v := result{Role: role, Version: agent.Version, Milliseconds: time.Since(started).Milliseconds(), ConnectionGranted: err == nil, HTTPStatus: 200, Code: "signed_connection_received"}
		if err != nil {
			allOK = false
			v.HTTPStatus = 0
			v.Code = "local_validation_failed"
			var upstream *sarvam.ProviderError
			if errors.As(err, &upstream) {
				v.HTTPStatus = upstream.Status
				v.Code = upstream.Code
			}
		}
		r.Agents = append(r.Agents, v)
		slog.Info("Sarvam agent connection probe", "role", v.Role, "version", v.Version, "http_status", v.HTTPStatus, "result", v.Code)
	}
	if err := os.MkdirAll("artifacts/sarvam", 0700); err != nil {
		return errors.New("could not prepare private probe report directory")
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.New("could not encode safe probe report")
	}
	path := filepath.Join("artifacts", "sarvam", r.CheckedAt.Format("20060102T150405.000000000Z")+"-probe.json")
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		return errors.New("could not save safe probe report")
	}
	slog.Info("safe probe report saved", "path", path, "audio_started", false, "customer_memory_sent", false)
	if !allOK {
		return errors.New("one or more configured agents could not obtain a signed connection; inspect the safe probe report")
	}
	return nil
}

// Package sarvam implements the narrow Voice Agents connection contract verified
// against the official sarvam-conv-ai-sdk 0.0.42 SocketManager source.
package sarvam

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const SDKContractVersion = "0.0.42"
const runtimeBaseURL = "https://apps.sarvam.ai/api/app-runtime/"

type Agent struct {
	AppID   string `json:"app_id"`
	Version int    `json:"version"`
}
type Config struct {
	OrgID       string
	WorkspaceID string
	Buyer       Agent
	Seller      Agent
	key         string
}

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var appID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,127}$`)
var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func LoadConfig() (Config, error) {
	c := Config{OrgID: os.Getenv("SARVAM_ORG_ID"), WorkspaceID: os.Getenv("SARVAM_WORKSPACE_ID"), key: os.Getenv("SARVAM_VOICE_API_KEY")}
	var err error
	c.Buyer.AppID = os.Getenv("SARVAM_BUYER_APP_ID")
	c.Seller.AppID = os.Getenv("SARVAM_SELLER_APP_ID")
	c.Buyer.Version, err = strconv.Atoi(os.Getenv("SARVAM_BUYER_AGENT_VERSION"))
	if err != nil {
		return Config{}, errors.New("SARVAM_BUYER_AGENT_VERSION must be a committed version number")
	}
	c.Seller.Version, err = strconv.Atoi(os.Getenv("SARVAM_SELLER_AGENT_VERSION"))
	if err != nil {
		return Config{}, errors.New("SARVAM_SELLER_AGENT_VERSION must be a committed version number")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !uuid.MatchString(c.OrgID) || !uuid.MatchString(c.WorkspaceID) {
		return errors.New("Sarvam organisation and workspace IDs must be UUIDs")
	}
	for _, v := range []Agent{c.Buyer, c.Seller} {
		if !appID.MatchString(v.AppID) || v.Version < 1 || v.Version > 1000000 {
			return errors.New("Sarvam app IDs and positive committed versions are required")
		}
	}
	if c.Buyer.AppID == c.Seller.AppID {
		return errors.New("buyer and seller must select separate configured agents")
	}
	if len(c.key) < 10 || c.key != strings.TrimSpace(c.key) || strings.ContainsAny(c.key, "\r\n") || strings.EqualFold(c.key, "REPLACE_ME") || strings.HasPrefix(c.key, "your_") {
		return errors.New("set SARVAM_VOICE_API_KEY in private .env")
	}
	return nil
}
func (c Config) Agent(role string) (Agent, error) {
	switch role {
	case "buyer":
		return c.Buyer, nil
	case "seller":
		return c.Seller, nil
	default:
		return Agent{}, errors.New("unsupported Sarvam agent role")
	}
}

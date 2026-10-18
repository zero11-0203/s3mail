package searchcmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/zero11-0203/s3mail/go/internal/search"
)

// Embedder is the optional semantic-search embedder
// (docs/specs/search.md#semantic-search), or nil when EMBED_MODEL is
// unset. EMBED_KEY_SECRET names the owner-set {"api_key": ...} secret;
// EMBED_BASE_URL defaults to OpenRouter.
func Embedder(cfg aws.Config) search.Embedder {
	model := os.Getenv("EMBED_MODEL")
	if model == "" {
		return nil
	}
	secret := Env("EMBED_KEY_SECRET")["EMBED_KEY_SECRET"]
	base := os.Getenv("EMBED_BASE_URL")
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	sm := secretsmanager.NewFromConfig(cfg)
	return &search.OpenRouter{
		BaseURL: base,
		Name:    model,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Key: func(ctx context.Context) (string, error) {
			out, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(secret)})
			if err != nil {
				return "", err
			}
			var v struct {
				APIKey string `json:"api_key"`
			}
			if err := json.Unmarshal([]byte(aws.ToString(out.SecretString)), &v); err != nil || v.APIKey == "" {
				return "", errors.New(`the embedding secret holds no {"api_key": ...}`)
			}
			return v.APIKey, nil
		},
	}
}

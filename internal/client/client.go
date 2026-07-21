// Package client provides a wrapper around the scaleset client, including
// automatic Installation discovery from the GitHub App API.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v4"
)

// Installation represents a discovered GitHub App installation.
type Installation struct {
	ID    int64
	Scope string // "https://github.com/org" or "https://github.com/org/repo"
}

// GitHubAuth holds GitHub App authentication parameters.
type GitHubAuth struct {
	ClientID   string
	PrivateKey string
}

// DiscoverInstallations fetches all installations for a GitHub App and resolves
// each installation's scope (org or repo URL).
func DiscoverInstallations(ctx context.Context, auth GitHubAuth) ([]Installation, error) {
	jwtToken, err := createAppJWT(auth.ClientID, auth.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("create JWT: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/app/installations?per_page=100", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list installations: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list installations: %s (status %d)", string(body), resp.StatusCode)
	}

	var result []installationResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode installations: %w", err)
	}

	var installations []Installation
	for _, inst := range result {
		scope, err := resolveScope(inst)
		if err != nil {
			return nil, fmt.Errorf("installation %d: %w", inst.ID, err)
		}
		installations = append(installations, Installation{
			ID:    inst.ID,
			Scope: scope,
		})
	}

	return installations, nil
}

// NewClient creates a new scaleset.Client scoped to a specific org/repo.
func NewClient(ctx context.Context, scope string, auth GitHubAuth) (*scaleset.Client, error) {
	return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: scope,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:   auth.ClientID,
			PrivateKey: auth.PrivateKey,
		},
		SystemInfo: scaleset.SystemInfo{
			System: "actions-runner-processor",
		},
	})
}

// installationResponse mirrors the GitHub API response shape.
type installationResponse struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string `json:"repository_selection"`
}

// resolveScope derives the GitHub config URL from an installation.
func resolveScope(inst installationResponse) (string, error) {
	switch inst.Account.Type {
	case "Organization", "User":
		return fmt.Sprintf("https://github.com/%s", inst.Account.Login), nil
	default:
		return "", fmt.Errorf("unsupported account type: %s", inst.Account.Type)
	}
}

// createAppJWT generates a JWT for GitHub App authentication.
func createAppJWT(clientID, privateKey string) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKey))
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    clientID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(key)
}

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
	// APIURL is the GitHub API base URL (default: https://api.github.com).
	// For GHES, set to https://github.mycompany.com/api/v3.
	APIURL string
}

// DiscoverInstallations fetches all installations for a GitHub App and resolves
// each installation's scope.
//
// For Organization installations, returns a single entry with the org scope
// (e.g. https://github.com/my-org). For User installations, expands into
// per-repository scopes (e.g. https://github.com/user/repo) because the
// scaleset SDK's registration-token path uses /orgs/ which does not work
// for personal accounts.
//
// baseURL is the GitHub instance URL (default: https://github.com).
// For GHES, set to the enterprise server URL (e.g. https://github.mycompany.com).
func DiscoverInstallations(ctx context.Context, auth GitHubAuth, baseURL string) ([]Installation, error) {
	jwtToken, err := createAppJWT(auth.ClientID, auth.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("create JWT: %w", err)
	}

	apiURL := auth.APIURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiURL+"/app/installations?per_page=100", nil)
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

	if baseURL == "" {
		baseURL = "https://github.com"
	}

	var installations []Installation
	for _, inst := range result {
		switch inst.Account.Type {
		case "User":
			// Expand User installations to per-repository scopes.
			// The scaleset SDK treats single-path URLs as org scope and calls
			// /orgs/{name}/actions/runners/registration-token, which 404s for
			// personal accounts. Using repo-level scopes works around this.
			repos, err := fetchInstallationRepositories(ctx, inst.ID, apiURL, jwtToken)
			if err != nil {
				return nil, fmt.Errorf("installation %d: fetch repos: %w", inst.ID, err)
			}
			for _, repo := range repos {
				installations = append(installations, Installation{
					ID:    inst.ID,
					Scope: fmt.Sprintf("%s/%s", baseURL, repo),
				})
			}
		default:
			scope, err := resolveScope(inst, baseURL)
			if err != nil {
				return nil, fmt.Errorf("installation %d: %w", inst.ID, err)
			}
			installations = append(installations, Installation{
				ID:    inst.ID,
				Scope: scope,
			})
		}
	}

	return installations, nil
}

// fetchInstallationRepositories returns the full_name (owner/repo) of all
// repositories accessible to the given installation.
func fetchInstallationRepositories(ctx context.Context, installationID int64, apiURL, jwtToken string) ([]string, error) {
	// First, get an installation access token.
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/app/installations/%d/access_tokens", apiURL, installationID), nil)
	if err != nil {
		return nil, fmt.Errorf("create token request: %w", err)
	}
	tokenReq.Header.Set("Authorization", "Bearer "+jwtToken)
	tokenReq.Header.Set("Accept", "application/vnd.github+json")

	tokenResp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		return nil, fmt.Errorf("get installation token: %w", err)
	}
	defer func() { _ = tokenResp.Body.Close() }()

	if tokenResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(tokenResp.Body)
		return nil, fmt.Errorf("get installation token: %s (status %d)", string(body), tokenResp.StatusCode)
	}

	var tokenData struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}

	// Now fetch repositories for this installation.
	var allRepos []string
	page := 1
	for {
		repoReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", apiURL, page), nil)
		if err != nil {
			return nil, fmt.Errorf("create repos request: %w", err)
		}
		repoReq.Header.Set("Authorization", "Bearer "+tokenData.Token)
		repoReq.Header.Set("Accept", "application/vnd.github+json")

		repoResp, err := http.DefaultClient.Do(repoReq)
		if err != nil {
			return nil, fmt.Errorf("list repos: %w", err)
		}

		if repoResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(repoResp.Body)
			_ = repoResp.Body.Close()
			return nil, fmt.Errorf("list repos: %s (status %d)", string(body), repoResp.StatusCode)
		}

		var reposData struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if err := json.NewDecoder(repoResp.Body).Decode(&reposData); err != nil {
			_ = repoResp.Body.Close()
			return nil, fmt.Errorf("decode repos: %w", err)
		}
		_ = repoResp.Body.Close()

		for _, r := range reposData.Repositories {
			allRepos = append(allRepos, r.FullName)
		}

		if len(reposData.Repositories) < 100 {
			break
		}
		page++
	}

	return allRepos, nil
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
func resolveScope(inst installationResponse, baseURL string) (string, error) {
	switch inst.Account.Type {
	case "Organization", "User":
		return fmt.Sprintf("%s/%s", baseURL, inst.Account.Login), nil
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

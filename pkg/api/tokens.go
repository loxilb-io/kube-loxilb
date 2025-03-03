/*
 * Copyright (c) 2025 LoxiLB Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"k8s.io/klog/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenManager stores tokens for multiple hosts
type TokenManager struct {
	Tokens map[string]*HostToken
	mu     sync.Mutex
}

// HostToken stores a token for a specific host
type HostToken struct {
	AccessToken string
	ExpiresAt   time.Time
}

var tokenManager = &TokenManager{Tokens: make(map[string]*HostToken)}
var tokenEndpoint = ""

func sanitizeHostEnv(host string) string {
	// Replace "." and ":" with "_"
	return strings.NewReplacer(".", "_", ":", "_").Replace(host)
}

// LoadTokenForHost initializes the token manager for a host
func LoadTokenForHost(host string) {
	accessToken := os.Getenv(fmt.Sprintf("ACCESS_TOKEN_%s", sanitizeHostEnv(host)))
	expiryStr := os.Getenv(fmt.Sprintf("EXPIRY_TIME_SECONDS_%s", sanitizeHostEnv(host)))

	provider := os.Getenv("LOXI_OAUTH_PROVIDER")
	if provider != "" {
		provider = "google"
	}

	tokenEndpoint = fmt.Sprintf("netlox/v1/oauth/%s/token", provider)

	// Default expiry time
	expiryTime := 3600
	if expiryStr != "" {
		if parsedExpiry, err := strconv.Atoi(strings.TrimSpace(expiryStr)); err == nil {
			expiryTime = parsedExpiry
		}
	}

	tokenManager.mu.Lock()
	defer tokenManager.mu.Unlock()

	if _, ok := tokenManager.Tokens[host]; ok {
		return
	}

	tokenManager.Tokens[host] = &HostToken{
		AccessToken: accessToken,
		ExpiresAt:   time.Now().Add(time.Duration(expiryTime) * time.Second),
	}
}

// GetAccessToken retrieves or refreshes a token for a specific host
func GetAccessToken(rc *RESTClient) (string, error) {
	tokenManager.mu.Lock()
	defer tokenManager.mu.Unlock()

	hostToken, exists := tokenManager.Tokens[rc.baseURL.Host]
	if !exists {
		return "", fmt.Errorf("no token found for host %s", rc.baseURL.Host)
	}

	// If token is still valid, return it
	if time.Now().Before(hostToken.ExpiresAt.Add(-5 * time.Minute)) {
		return hostToken.AccessToken, nil
	}

	// Otherwise, refresh the token
	return refreshTokenForHost(rc)
}

// refreshTokenForHost refreshes the access token for a given host
func refreshTokenForHost(rc *RESTClient) (string, error) {
	tokenManager.mu.Lock()
	defer tokenManager.mu.Unlock()

	refreshToken := os.Getenv(fmt.Sprintf("REFRESH_TOKEN_%s", sanitizeHostEnv(rc.baseURL.Host)))
	if refreshToken == "" {
		return "", fmt.Errorf("missing refresh token for host %s", rc.baseURL.Host)
	}

	// Retrieve existing token or use an empty string
	currentToken := ""
	if hostToken, exists := tokenManager.Tokens[rc.baseURL.Host]; exists {
		currentToken = hostToken.AccessToken
	}

	// Construct request URL
	url := fmt.Sprintf("%s/%s?token=%s&refreshtoken=%s", rc.baseURL.String(), tokenEndpoint, currentToken, refreshToken)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	// Set timeout for the request
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := &http.Client{}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to send token request: %w", err)
	}
	defer resp.Body.Close()

	// Check if response is OK
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to refresh token for host %s: %s", rc.baseURL.Host, string(bodyBytes))
	}

	// Parse JSON response
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}

	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}

	// Update token manager
	tokenManager.Tokens[rc.baseURL.Host] = &HostToken{
		AccessToken: result.AccessToken,
		ExpiresAt:   time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}

	klog.Infof("Token refreshed successfully for host %s", rc.baseURL.Host)
	return result.AccessToken, nil
}

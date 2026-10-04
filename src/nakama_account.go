package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Online names. A player's online name is the account's display name:
// lobbies show it, with a tag from the account ID, and the fight screen shows
// it in place of the name of the character the player controls
// (nakama_hud.go). Names need not be unique; accounts are told apart by their
// IDs. The server's ikemen_account_name RPC
// (nakama/modules/ikemen_account.lua) checks the general rules and stores the
// name; which characters a game accepts is up to the game (its lifebar's name
// fonts, see lifebarNameMissing).

// NakamaAccount is the signed-in account as GET /v2/account returns it.
type NakamaAccount struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// NakamaRPCError is a failed RPC call. Code is the Nakama (gRPC) error code
// the server raised: 3 invalid argument, 5 not found, 6 already exists, 13
// internal. Error() returns the server's message when it sent one.
type NakamaRPCError struct {
	ID      string
	Status  int
	Code    int
	Message string
}

func (e *NakamaRPCError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("nakama rpc %q failed: HTTP %d", e.ID, e.Status)
}

// newNakamaRPCError reads Nakama's error body: {"code", "message", "error"}
// ("error" is the message again, or an object).
func newNakamaRPCError(id string, status int, body []byte) *NakamaRPCError {
	e := &NakamaRPCError{ID: id, Status: status}
	var reply struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Code    int             `json:"code"`
	}
	if json.Unmarshal(body, &reply) == nil {
		e.Code = reply.Code
		e.Message = reply.Message
		var text string
		if e.Message == "" && json.Unmarshal(reply.Error, &text) == nil {
			e.Message = text
		}
	}
	if e.Message == "" {
		text := strings.TrimSpace(string(body))
		if text != "" {
			e.Message = fmt.Sprintf("nakama rpc %q failed: HTTP %d: %s", id, status, text)
		}
	}
	return e
}

// nakamaRPCErrorCode returns the Nakama error code of an RPC error, or 0.
func nakamaRPCErrorCode(err error) int {
	var rpcErr *NakamaRPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code
	}
	return 0
}

// GetAccount reads the signed-in account.
func (n *NakamaClient) GetAccount(ctx context.Context) (NakamaAccount, error) {
	session, err := n.freshSession(ctx)
	if err != nil {
		return NakamaAccount{}, err
	}
	scheme := "http"
	if n.cfg.UseTLS {
		scheme = "https"
	}
	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTTL)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.endpoint(scheme)+"/v2/account", nil)
	if err != nil {
		return NakamaAccount{}, err
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return NakamaAccount{}, fmt.Errorf("nakama account request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return NakamaAccount{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return NakamaAccount{}, fmt.Errorf("nakama account request failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var reply struct {
		User struct {
			ID          string `json:"id"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return NakamaAccount{}, fmt.Errorf("decode nakama account: %w", err)
	}
	n.mu.Lock()
	n.displayName = reply.User.DisplayName
	n.mu.Unlock()
	return NakamaAccount{UserID: reply.User.ID, Username: reply.User.Username, DisplayName: reply.User.DisplayName}, nil
}

// SetAccountName sets the account's display name, the player's online name,
// and returns it as the server stored it. A lobby reads a member's name from
// the account when the member joins, so a new name shows in the next lobby
// joined.
func (n *NakamaClient) SetAccountName(ctx context.Context, name string) (string, error) {
	body, err := n.CallRPC(ctx, "ikemen_account_name", map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	var result struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.DisplayName == "" {
		return "", errors.New("the server did not confirm the new name")
	}
	n.mu.Lock()
	n.displayName = result.DisplayName
	n.mu.Unlock()
	return result.DisplayName, nil
}

// DisplayName returns the account's display name as last read (GetAccount)
// or set (SetAccountName), or "" before either.
func (n *NakamaClient) DisplayName() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.displayName
}

// GetUsers reads other accounts: their IDs, usernames and display names, in
// the order the server returns them. Unknown IDs are left out.
func (n *NakamaClient) GetUsers(ctx context.Context, ids []string) ([]NakamaAccount, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	session, err := n.freshSession(ctx)
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if n.cfg.UseTLS {
		scheme = "https"
	}
	query := url.Values{}
	for _, id := range ids {
		if id != "" {
			query.Add("ids", id)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTTL)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.endpoint(scheme)+"/v2/user?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nakama users request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("nakama users request failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var reply struct {
		Users []struct {
			ID          string `json:"id"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		} `json:"users"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("decode nakama users: %w", err)
	}
	users := make([]NakamaAccount, 0, len(reply.Users))
	for _, u := range reply.Users {
		users = append(users, NakamaAccount{UserID: u.ID, Username: u.Username, DisplayName: u.DisplayName})
	}
	return users, nil
}

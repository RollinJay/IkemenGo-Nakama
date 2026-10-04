package main

// Live tests of online names (nakama/modules/ikemen_account.lua and the
// lobby's names and tags). Enable with NAKAMA_LIVE=1 (see
// nakama_live_test.go for the server flags).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tagFor is the lobby's tag of an account: the first eight hex digits of its
// ID as a number, modulo 10000, in four digits.
func tagFor(userID string) string {
	n, err := strconv.ParseUint(strings.ReplaceAll(userID, "-", "")[:8], 16, 32)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%04d", n%10000)
}

// A player's online name is the account's display name. Names need not be
// unique, may use any characters but control characters, and have 2 to 16
// of them; lobbies show them with a tag from the account ID.
func TestNakamaLive_OnlineName(t *testing.T) {
	fixedEnabled(t)
	ctx := context.Background()
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, _ := liveClient(t)
	usernameA := a.Session().Username
	name := "KAI " + strings.ToUpper(randID()[7:10])
	// Two players take the same name.
	for _, cl := range []*NakamaClient{a, b} {
		got, err := cl.SetAccountName(ctx, "  "+name+" ")
		if err != nil || got != name || cl.DisplayName() != name {
			t.Fatalf("SetAccountName: %q %v (cached %q)", got, err, cl.DisplayName())
		}
	}
	account, err := a.GetAccount(ctx)
	if err != nil || account.DisplayName != name || account.Username != usernameA {
		t.Fatalf("account %+v %v; want display name %q and username %q unchanged", account, err, name, usernameA)
	}
	if !a.IsConnected() {
		t.Fatal("setting the name closed the connection")
	}
	for _, good := range []string{"Émile", "カイ", "a.b-c_d", "16 characters ok"} {
		if got, err := c.SetAccountName(ctx, good); err != nil || got != good {
			t.Fatalf("valid name %q: %q %v", good, got, err)
		}
	}
	for _, bad := range []string{"x", " x ", "seventeen chars x", "tab\there", "nul\x00x", "\u0085next"} {
		if _, err := c.SetAccountName(ctx, bad); nakamaRPCErrorCode(err) != 3 {
			t.Fatalf("invalid name %q: %v", bad, err)
		}
	}
	// A member who never chose a name shows under the username, cut to 16
	// characters: a username can have 128 bytes and spaces.
	long := "a long username " + randID()[7:15]
	fresh := NewNakamaClient(NakamaConfig{Host: "127.0.0.1", Port: 7350, ServerKey: "defaultkey", DeviceID: randID(),
		Username: long, Game: "g", GameBuild: "1.0.0", Region: "r", RequestTTL: 5 * time.Second})
	recF := newRecorder()
	fresh.SetEventHandler(recF.fn)
	if err := fresh.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(fresh.Disconnect)
	if fresh.Session().Username != long {
		t.Fatalf("username %q, want %q", fresh.Session().Username, long)
	}
	shown := strings.TrimSpace(string([]rune(long)[:16]))

	id := createLobby(t, a, "queue")
	join(t, a, recA, id)
	join(t, b, recB, id)
	join(t, fresh, recF, id)
	st := waitLobby(t, a, "three members", func(st map[string]any) bool {
		m, _ := st["members"].([]any)
		return len(m) == 3
	})
	for _, cl := range []*NakamaClient{a, b} {
		uid := cl.Session().UserID
		if memberField(st, uid, "name") != name || memberField(st, uid, "display_name") != name {
			t.Fatalf("member %s: name %v, display_name %v; want %q", uid, memberField(st, uid, "name"), memberField(st, uid, "display_name"), name)
		}
	}
	freshID := fresh.Session().UserID
	if memberField(st, freshID, "name") != shown || memberField(st, freshID, "display_name") != "" {
		t.Fatalf("unnamed member: name %v, display_name %v; want name %q", memberField(st, freshID, "name"), memberField(st, freshID, "display_name"), shown)
	}
	// Notices carry the account's username and the online name.
	joined := waitLobbyEvent(t, a, "b joined", func(ev map[string]any) bool {
		return ev["kind"] == "notice" && ev["notice"] == "joined" && ev["user_id"] == b.Session().UserID
	})
	if joined["username"] != b.Session().Username || joined["name"] != name {
		t.Fatalf("joined notice %v", joined)
	}
	if tagFor(a.Session().UserID) == tagFor(b.Session().UserID) {
		t.Logf("the two accounts share a tag (1 in 10000): %s", tagFor(a.Session().UserID))
	}
	// Lobby search lists the names with their tags (Nakama updates listings
	// about once a second).
	var label map[string]any
	var names, tags []any
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		label = listLabel(t, fresh, id)
		names, _ = label["members"].([]any)
		tags, _ = label["tags"].([]any)
		if len(names) == 3 {
			break
		}
	}
	wantTags := []string{tagFor(a.Session().UserID), tagFor(b.Session().UserID), tagFor(freshID)}
	if len(names) != 3 || names[0] != name || names[1] != name || names[2] != shown || len(tags) != 3 {
		t.Fatalf("label members %v tags %v", label["members"], label["tags"])
	}
	for i, want := range wantTags {
		if tags[i] != want {
			t.Fatalf("label tags %v, want %v", tags, wantTags)
		}
	}
	if label["host"] != name || label["host_tag"] != wantTags[0] {
		t.Fatalf("label host %v (%v)", label["host"], label["host_tag"])
	}
	// Chat and notices carry the name; the tag comes from user_id.
	if err := a.SendLobbyCommand("chat", map[string]any{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	ev := waitLobbyEvent(t, b, "chat", func(ev map[string]any) bool {
		return ev["kind"] == "chat" && ev["user_id"] == a.Session().UserID
	})
	if ev["name"] != name {
		t.Fatalf("chat event %v", ev)
	}
	t.Logf("two members named %q (tags %s, %s), an unnamed member shown as %q", name, wantTags[0], wantTags[1], shown)
}

// Nakama's own account update (PUT /v2/account) holds display names to the
// same rules as ikemen_account_name. Usernames follow Nakama's own rules:
// lobbies show one, cut to 16 characters, only until the player chooses a
// name.
func TestNakamaLive_OnlineNameUpdateHook(t *testing.T) {
	fixedEnabled(t)
	ctx := context.Background()
	a, _ := liveClient(t)
	put := func(body map[string]any) (int, int) {
		t.Helper()
		session, err := a.freshSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, a.endpoint("http")+"/v2/account", bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+session.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		reply, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, 0
		}
		return resp.StatusCode, newNakamaRPCError("UpdateAccount", resp.StatusCode, reply).Code
	}
	for _, bad := range []string{"x", "seventeen chars x", "bell\x07"} {
		if status, code := put(map[string]any{"display_name": bad}); code != 3 {
			t.Fatalf("PUT /v2/account with display name %q: HTTP %d, code %d; want code 3", bad, status, code)
		}
	}
	// Input too long to be a name is refused before it is trimmed: trimming
	// a long run of spaces with Lua patterns takes quadratic time.
	long := "a" + strings.Repeat(" ", 100000) + "b"
	start := time.Now()
	if status, code := put(map[string]any{"display_name": long}); code != 3 {
		t.Fatalf("a 100 KB display name: HTTP %d, code %d; want code 3", status, code)
	}
	if _, err := a.SetAccountName(ctx, long); nakamaRPCErrorCode(err) != 3 {
		t.Fatalf("a 100 KB name through the RPC: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("refusing two 100 KB names took %v", d)
	}
	if status, code := put(map[string]any{"display_name": "  Anything at all ", "timezone": "UTC"}); code != 0 {
		t.Fatalf("a valid display name: HTTP %d, code %d", status, code)
	}
	username := "free_" + randID()[7:13]
	if status, code := put(map[string]any{"username": username}); code != 0 {
		t.Fatalf("a username change: HTTP %d, code %d", status, code)
	}
	account, err := a.GetAccount(ctx)
	if err != nil || account.DisplayName != "Anything at all" || account.Username != username {
		t.Fatalf("account after the updates: %+v %v", account, err)
	}
	t.Logf("PUT /v2/account refuses invalid display names (a 100 KB one at once), trims valid ones and lets usernames through")
}

// A username Nakama would refuse when it creates an account (taken by an
// account, or with "[]", which Nakama 3.41's username check refuses) is left
// out, and Nakama generates one: the sign-in succeeds. A username with a
// space is kept, since Nakama accepts spaces, and an email sign-in by
// username (no email address) keeps its username.
func TestNakamaLive_SignInUsername(t *testing.T) {
	fixedEnabled(t)
	ctx := context.Background()
	taken, _ := liveClient(t)
	takenName := taken.Session().Username
	signIn := func(username string) string {
		t.Helper()
		c := NewNakamaClient(NakamaConfig{Host: "127.0.0.1", Port: 7350, ServerKey: "defaultkey", DeviceID: randID(),
			Username: username, RequestTTL: 5 * time.Second})
		if err := c.Authenticate(ctx); err != nil {
			t.Fatalf("sign in with %q: %v", username, err)
		}
		return c.Session().Username
	}
	free := "free_" + randID()[7:13]
	spaced := "has space " + randID()[7:11]
	for _, kept := range []string{free, spaced} {
		if got := signIn(kept); got != kept {
			t.Fatalf("username %q became %q", kept, got)
		}
	}
	for _, refused := range []string{takenName, "a[]b" + randID()[7:11]} {
		if got := signIn(refused); got == refused || got == "" {
			t.Fatalf("sign in with %q: account named %q", refused, got)
		}
	}
	// Email accounts: created with an email address and a username, then
	// signed in to by username and password alone.
	emailAuth := func(email, username string, create bool) (string, int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"email": email, "password": "password123"})
		url := fmt.Sprintf("http://127.0.0.1:7350/v2/account/authenticate/email?create=%t&username=%s", create, username)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		req.SetBasicAuth("defaultkey", "")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Token, resp.StatusCode
	}
	userOf := func(token string) string {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:7350/v2/account", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			User struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.User.ID + " " + out.User.Username
	}
	emailName := "mail_" + randID()[7:13]
	created, status := emailAuth("k"+randID()[7:15]+"@example.com", emailName, true)
	if created == "" {
		t.Fatalf("email account: HTTP %d", status)
	}
	byName, status := emailAuth("", emailName, false)
	if byName == "" {
		t.Fatalf("sign in by username: HTTP %d", status)
	}
	if a, b := userOf(created), userOf(byName); a != b || !strings.HasSuffix(a, " "+emailName) {
		t.Fatalf("sign in by username reached %q, want %q", b, a)
	}
	t.Logf("sign-in usernames: %q and %q kept; a taken one and one with [] replaced; email sign-in by username works", free, spaced)
}

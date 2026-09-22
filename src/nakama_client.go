package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type NakamaConfig struct {
	Host       string
	Port       int
	ServerKey  string
	UseTLS     bool
	Status     bool
	DeviceID   string
	Username   string
	Game       string
	GameBuild  string
	Region     string
	RequestTTL time.Duration
}

type NakamaSession struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	Created      int64  `json:"created"`
	Expires      int64  `json:"expires_at"`
	Refresh      int64  `json:"refresh_expires_at"`
	Username     string `json:"username"`
	UserID       string `json:"user_id"`
}

type NakamaMatchUser struct {
	Presence struct {
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
		Username  string `json:"username"`
		Node      string `json:"node"`
	} `json:"presence"`
	Properties map[string]any `json:"properties"`
}

type NakamaMatchmakerMatched struct {
	Ticket  string            `json:"ticket"`
	Token   string            `json:"token"`
	MatchID string            `json:"match_id"`
	Users   []NakamaMatchUser `json:"users"`
}

type NakamaMatchData struct {
	MatchID  string `json:"match_id"`
	Presence struct {
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
		Username  string `json:"username"`
	} `json:"presence"`
	OpCode     int64  `json:"op_code"`
	Data       string `json:"data"`
	Reliable   bool   `json:"reliable"`
	BinaryData []byte `json:"-"`
}

type NakamaMatchmakerRequest struct {
	Mode                       string
	Elo                        int
	EloRange                   int
	Game                       string
	GameBuild                  string
	Region                     string
	MinCount                   int
	MaxCount                   int
	RankedBestOf               int
	RankedSwitchSides          bool
	RankedWinnerKeepsSelection bool
}

type NakamaLobbyConfig struct {
	Name       string         `json:"name"`
	Game       string         `json:"game"`
	GameBuild  string         `json:"build"`
	Format     string         `json:"format"`
	Rules      map[string]any `json:"rules"`
	MaxPlayers int            `json:"max_players"`
	MaxGames   int            `json:"max_games,omitempty"`
}

type NakamaResultRequest struct {
	MatchID    string `json:"match_id"`
	OpponentID string `json:"opponent_id"`
	Game       string `json:"game"`
	Result     int    `json:"result"`
	ReplayHash string `json:"replay_hash,omitempty"`
}

const (
	nakamaReplayHeaderOp    uint32 = 0x494B_5210
	nakamaReplayChunkOp     uint32 = 0x494B_5211
	nakamaReplayEndOp       uint32 = 0x494B_5212
	nakamaReplayResetOp     uint32 = 0x494B_5213
	nakamaSignalOp          uint32 = 0x494B_5347
	nakamaLobbyResultOp     uint32 = 0x494B_4C11
	nakamaRematchChoiceOp   uint32 = 0x494B_5243
	nakamaRematchDecisionOp uint32 = 0x494B_5244
)

type NakamaEvent struct {
	Type           string
	Matchmaker     *NakamaMatchmakerMatched
	MatchData      *NakamaMatchData
	MatchID        string
	MatchToken     string
	Ticket         string
	ChannelID      string
	ChannelMessage map[string]any
	Payload        any
	Error          error
	Raw            map[string]json.RawMessage
}

type NakamaClient struct {
	mu          sync.Mutex
	cfg         NakamaConfig
	session     *NakamaSession
	conn        net.Conn
	reader      *bufio.Reader
	writeMu     sync.Mutex
	writeQueue  chan []byte
	closed      chan struct{}
	closeOne    sync.Once
	cid         atomic.Uint64
	ticket      string
	matchID     string
	stunServers []string
	p2p         *NakamaP2P
	replay      *NakamaReplayBuffer

	eventMu sync.RWMutex
	eventFn func(NakamaEvent)
}

func NewNakamaClient(cfg NakamaConfig) *NakamaClient {
	if cfg.Port == 0 {
		cfg.Port = 7350
	}
	if cfg.RequestTTL <= 0 {
		cfg.RequestTTL = 10 * time.Second
	}
	return &NakamaClient{cfg: cfg, closed: make(chan struct{})}
}

func (n *NakamaClient) SetSTUNServers(servers []string) {
	n.mu.Lock()
	n.stunServers = append([]string(nil), servers...)
	n.mu.Unlock()
}

func (n *NakamaClient) SetEventHandler(fn func(NakamaEvent)) {
	n.eventMu.Lock()
	n.eventFn = fn
	n.eventMu.Unlock()
}

func (n *NakamaClient) emit(ev NakamaEvent) {
	n.eventMu.RLock()
	fn := n.eventFn
	n.eventMu.RUnlock()
	if fn != nil {
		fn(ev)
	}
}

func (n *NakamaClient) Session() *NakamaSession {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.session == nil {
		return nil
	}
	copy := *n.session
	return &copy
}

func (n *NakamaClient) IsConnected() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conn != nil
}

func (n *NakamaClient) endpoint(scheme string) string {
	return fmt.Sprintf("%s://%s:%d", scheme, n.cfg.Host, n.cfg.Port)
}

func (n *NakamaClient) Authenticate(ctx context.Context) error {
	if strings.TrimSpace(n.cfg.ServerKey) == "" {
		return errors.New("nakama server key is empty")
	}
	if strings.TrimSpace(n.cfg.DeviceID) == "" {
		seed := make([]byte, 16)
		if _, err := rand.Read(seed); err != nil {
			return fmt.Errorf("generate device id: %w", err)
		}
		n.cfg.DeviceID = hex.EncodeToString(seed)
	}

	payload := map[string]string{"id": n.cfg.DeviceID}
	if n.cfg.Username != "" {
		payload["username"] = n.cfg.Username
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTTL)
	defer cancel()
	scheme := "http"
	if n.cfg.UseTLS {
		scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.endpoint(scheme)+"/v2/account/authenticate/device?create=true",
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.SetBasicAuth(n.cfg.ServerKey, "")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("nakama authentication request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("nakama authentication failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var session NakamaSession
	if err := json.Unmarshal(respBody, &session); err != nil {
		return fmt.Errorf("decode nakama session: %w", err)
	}
	if session.Token == "" {
		return errors.New("nakama authentication returned an empty session token")
	}
	n.mu.Lock()
	n.session = &session
	n.mu.Unlock()
	return nil
}

func (n *NakamaClient) Connect(ctx context.Context) error {
	if n.cfg.Host == "" {
		return errors.New("nakama host is empty")
	}
	if n.Session() == nil {
		if err := n.Authenticate(ctx); err != nil {
			return err
		}
	}
	if n.IsConnected() {
		return nil
	}

	addr := net.JoinHostPort(n.cfg.Host, strconv.Itoa(n.cfg.Port))
	dialer := &net.Dialer{Timeout: n.cfg.RequestTTL}
	var conn net.Conn
	var err error
	if n.cfg.UseTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: n.cfg.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("nakama socket dial: %w", err)
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return fmt.Errorf("generate websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	scheme := "ws"
	if n.cfg.UseTLS {
		scheme = "wss"
	}
	q := url.Values{}
	q.Set("format", "json")
	q.Set("status", strconv.FormatBool(n.cfg.Status))
	q.Set("token", n.Session().Token)
	reqURL := fmt.Sprintf("%s://%s/ws?%s", scheme, addr, q.Encode())
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		conn.Close()
		return err
	}
	req.Host = addr
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if err := req.Write(conn); err != nil {
		conn.Close()
		return fmt.Errorf("websocket handshake write: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		return fmt.Errorf("websocket handshake response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		conn.Close()
		return fmt.Errorf("nakama websocket handshake failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	expected := websocketAccept(key)
	if !strings.EqualFold(resp.Header.Get("Sec-WebSocket-Accept"), expected) {
		conn.Close()
		return errors.New("nakama websocket handshake returned an invalid Sec-WebSocket-Accept")
	}
	resp.Body.Close()

	n.mu.Lock()
	n.conn = conn
	n.reader = reader
	n.closed = make(chan struct{})
	n.writeQueue = make(chan []byte, 1024)
	n.closeOne = sync.Once{}
	n.mu.Unlock()

	go n.writeLoop(conn, n.closed, n.writeQueue)
	go n.readLoop()
	return nil
}

func websocketAccept(key string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (n *NakamaClient) Disconnect() {
	n.StopP2P()
	n.mu.Lock()
	conn := n.conn
	closed := n.closed
	n.conn = nil
	n.reader = nil
	n.ticket = ""
	n.matchID = ""
	replay := n.replay
	n.mu.Unlock()
	if replay != nil {
		replay.Reset("client_disconnected")
	}
	if closed != nil {
		n.closeOne.Do(func() { close(closed) })
	}
	if conn != nil {
		n.writeMu.Lock()
		_ = writeWebSocketFrame(conn, 0x8, nil)
		n.writeMu.Unlock()
		_ = conn.Close()
	}
}

func (n *NakamaClient) StartP2P(localPort int, stunServers []string) error {
	n.mu.Lock()
	matchID := n.matchID
	if len(stunServers) == 0 {
		stunServers = append([]string(nil), n.stunServers...)
	}
	n.mu.Unlock()
	if matchID == "" {
		return errors.New("cannot start P2P before joining a Nakama match")
	}
	n.StopP2P()
	p, err := NewNakamaP2P(matchID, localPort, func(signal P2PSignal) error {
		return n.PublishSignal(signal)
	})
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.p2p = p
	n.mu.Unlock()
	go func() {
		if err := p.Start(context.Background(), stunServers); err != nil {
			n.emit(NakamaEvent{Type: "p2p_error", Error: err})
			return
		}
		n.emit(NakamaEvent{Type: "p2p_candidates", MatchID: matchID})
		if err := p.Wait(context.Background()); err != nil {
			n.emit(NakamaEvent{Type: "p2p_error", Error: err})
			return
		}
		n.emit(NakamaEvent{Type: "p2p_connected", MatchID: matchID})
	}()
	return nil
}

func (n *NakamaClient) StopP2P() {
	n.mu.Lock()
	p := n.p2p
	n.p2p = nil
	n.mu.Unlock()
	if p != nil {
		p.Close()
	}
}

func (n *NakamaClient) P2P() *NakamaP2P {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.p2p
}

// P2PReady reports whether the current Nakama P2P socket has completed its
// candidate exchange and UDP hole-punch handshake.
func (n *NakamaClient) P2PReady() bool {
	n.mu.Lock()
	p := n.p2p
	n.mu.Unlock()
	if p == nil {
		return false
	}
	select {
	case <-p.ready:
		return true
	default:
		return false
	}
}

// TakeP2PTransport transfers the already-established NAT-traversed UDP socket
// and the peer endpoint to the gameplay transport. After this call the Nakama
// client no longer owns the P2P object, so StopP2P cannot close the transferred
// socket.
func (n *NakamaClient) TakeP2PTransport() (*net.UDPConn, *net.UDPAddr, error) {
	n.mu.Lock()
	p := n.p2p
	n.mu.Unlock()
	if p == nil {
		return nil, nil, errors.New("nakama p2p connection is not started")
	}
	remote := p.RemoteAddr()
	if remote == nil {
		return nil, nil, errors.New("nakama p2p connection is not ready")
	}
	conn, err := p.TakeUDPConn()
	if err != nil {
		return nil, nil, err
	}
	n.mu.Lock()
	if n.p2p == p {
		n.p2p = nil
	}
	n.mu.Unlock()
	p.Close()
	return conn, remote, nil
}

func (n *NakamaClient) ReplayBuffer() *NakamaReplayBuffer {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.replay == nil {
		n.replay = NewNakamaReplayBuffer()
	}
	return n.replay
}

func (n *NakamaClient) nextCID() string {
	return fmt.Sprintf("ikemen-%d-%d", time.Now().UnixNano(), n.cid.Add(1))
}

func (n *NakamaClient) sendEnvelope(field string, payload any) error {
	n.mu.Lock()
	conn := n.conn
	n.mu.Unlock()
	if conn == nil {
		return errors.New("nakama socket is not connected")
	}
	env := map[string]any{
		"cid": n.nextCID(),
		field: payload,
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	n.mu.Lock()
	queue := n.writeQueue
	closed := n.closed
	n.mu.Unlock()
	if queue == nil {
		return errors.New("nakama socket output queue is not initialized")
	}
	select {
	case queue <- body:
		return nil
	case <-closed:
		return io.ErrClosedPipe
	default:
		return errors.New("nakama socket output queue is full")
	}
}

func (n *NakamaClient) CurrentMatchID() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.matchID
}

func (n *NakamaClient) LeaveMatch() error {
	n.mu.Lock()
	matchID := n.matchID
	n.mu.Unlock()
	if matchID == "" {
		return nil
	}
	if err := n.sendEnvelope("match_leave", map[string]any{"match_id": matchID}); err != nil {
		return err
	}
	n.mu.Lock()
	n.matchID = ""
	n.mu.Unlock()
	return nil
}

func (n *NakamaClient) Matchmake(req NakamaMatchmakerRequest) error {
	if req.MinCount <= 0 {
		req.MinCount = 2
	}
	if req.MaxCount <= 0 {
		req.MaxCount = 2
	}
	if req.Game == "" {
		req.Game = n.cfg.Game
	}
	if req.GameBuild == "" {
		req.GameBuild = n.cfg.GameBuild
	}
	if req.Region == "" {
		req.Region = n.cfg.Region
	}
	if req.Mode == "" {
		req.Mode = "unranked"
	}
	bestOf := normalizeRankedBestOf(req.RankedBestOf)
	stringsProps := map[string]string{
		"game":                          req.Game,
		"build":                         req.GameBuild,
		"mode":                          req.Mode,
		"region":                        req.Region,
		"ranked_best_of":                strconv.Itoa(bestOf),
		"ranked_switch_sides":           strconv.FormatBool(req.RankedSwitchSides),
		"ranked_winner_keeps_selection": strconv.FormatBool(req.RankedWinnerKeepsSelection),
	}
	numericProps := map[string]float64{
		"elo":       float64(req.Elo),
		"elo_range": float64(req.EloRange),
	}
	queryParts := make([]string, 0, 4)
	for key, value := range stringsProps {
		if value != "" {
			queryParts = append(queryParts, "+properties."+key+":"+value)
		}
	}
	sort.Strings(queryParts)
	if err := n.sendEnvelope("matchmaker_add", map[string]any{
		"query":              strings.Join(queryParts, " "),
		"min_count":          req.MinCount,
		"max_count":          req.MaxCount,
		"count_multiple":     1,
		"string_properties":  stringsProps,
		"numeric_properties": numericProps,
	}); err != nil {
		return err
	}
	return nil
}

func (n *NakamaClient) CancelMatchmaking() error {
	n.mu.Lock()
	ticket := n.ticket
	n.mu.Unlock()
	if ticket == "" {
		return nil
	}
	return n.sendEnvelope("matchmaker_remove", map[string]any{"ticket": ticket})
}

func (n *NakamaClient) JoinMatch(matchID, token string) error {
	return n.JoinMatchWithMetadata(matchID, token, nil)
}

func (n *NakamaClient) JoinMatchWithMetadata(matchID, token string, metadata map[string]string) error {
	if matchID == "" && token == "" {
		return errors.New("nakama match id or token is required")
	}
	metadataCopy := make(map[string]string, len(metadata)+2)
	for key, value := range metadata {
		metadataCopy[key] = value
	}
	n.mu.Lock()
	if metadataCopy["game"] == "" {
		metadataCopy["game"] = n.cfg.Game
	}
	if metadataCopy["build"] == "" {
		metadataCopy["build"] = n.cfg.GameBuild
	}
	n.mu.Unlock()
	payload := map[string]any{
		"match_id": matchID,
		"token":    token,
	}
	if len(metadataCopy) > 0 {
		payload["metadata"] = metadataCopy
	}
	return n.sendEnvelope("match", payload)
}

func (n *NakamaClient) SendMatchData(opCode uint32, data []byte, reliable bool) error {
	n.mu.Lock()
	matchID := n.matchID
	n.mu.Unlock()
	if matchID == "" {
		return errors.New("nakama match is not joined")
	}
	return n.sendEnvelope("match_data_send", map[string]any{
		"match_id": matchID,
		"op_code":  opCode,
		"data":     data,
		"reliable": reliable,
	})
}

func (n *NakamaClient) ReportLobbyResult(winnerID, loserID string) error {
	winnerID = strings.TrimSpace(winnerID)
	loserID = strings.TrimSpace(loserID)
	if winnerID == "" || loserID == "" || winnerID == loserID {
		return errors.New("lobby result requires distinct winner and loser user ids")
	}
	body, err := json.Marshal(map[string]string{
		"kind":   "match_result",
		"winner": winnerID,
		"loser":  loserID,
	})
	if err != nil {
		return err
	}
	return n.SendMatchData(nakamaLobbyResultOp, body, true)
}

func (n *NakamaClient) JoinChat(target string, channelType int, persistence, hidden bool) error {
	if channelType <= 0 {
		channelType = 1
	}
	return n.sendEnvelope("channel_join", map[string]any{
		"target":      target,
		"type":        channelType,
		"persistence": persistence,
		"hidden":      hidden,
	})
}

func (n *NakamaClient) SendChat(channelID string, content map[string]string) error {
	if channelID == "" {
		return errors.New("nakama chat channel id is empty")
	}
	return n.sendEnvelope("channel_message_send", map[string]any{
		"channel_id": channelID,
		"content":    content,
	})
}

func (n *NakamaClient) CallRPC(ctx context.Context, id string, payload any) ([]byte, error) {
	if id == "" {
		return nil, errors.New("nakama rpc id is empty")
	}
	n.mu.Lock()
	session := n.session
	n.mu.Unlock()
	if session == nil {
		return nil, errors.New("nakama session is not established")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode nakama rpc %q: %w", id, err)
	}
	scheme := "http"
	if n.cfg.UseTLS {
		scheme = "https"
	}
	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTTL)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.endpoint(scheme)+"/v2/rpc/"+url.PathEscape(id),
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nakama rpc %q request: %w", id, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("nakama rpc %q failed: HTTP %d: %s", id, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

func normalizeNakamaLobbyFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "winner_stays_on", "winner_stays":
		return "winner_stays_on"
	case "round_robin":
		return "round_robin"
	case "bracket":
		return "bracket"
	default:
		return "queue"
	}
}

func normalizeNakamaLobbyMaxGames(maxGames int) int {
	if maxGames < 1 {
		return 3
	}
	if maxGames > 99 {
		return 99
	}
	return maxGames
}

func (n *NakamaClient) CreateLobby(ctx context.Context, cfg NakamaLobbyConfig) (string, error) {
	if cfg.Game == "" {
		cfg.Game = n.cfg.Game
	}
	if cfg.GameBuild == "" {
		cfg.GameBuild = n.cfg.GameBuild
	}
	cfg.Format = normalizeNakamaLobbyFormat(cfg.Format)
	if cfg.MaxPlayers <= 0 || cfg.MaxPlayers > 8 {
		cfg.MaxPlayers = 8
	}
	if cfg.Rules == nil {
		cfg.Rules = map[string]any{}
	}
	if v, ok := cfg.Rules["max_games"]; ok {
		switch n := v.(type) {
		case int:
			cfg.MaxGames = n
		case int32:
			cfg.MaxGames = int(n)
		case int64:
			cfg.MaxGames = int(n)
		case float64:
			cfg.MaxGames = int(n)
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				cfg.MaxGames = int(parsed)
			}
		}
	}
	cfg.MaxGames = normalizeNakamaLobbyMaxGames(cfg.MaxGames)
	if _, ok := cfg.Rules["max_games"]; !ok {
		cfg.Rules["max_games"] = cfg.MaxGames
	}
	body, err := n.CallRPC(ctx, "ikemen_lobby_create", cfg)
	if err != nil {
		return "", err
	}
	var result struct {
		MatchID string `json:"match_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode lobby creation response: %w", err)
	}
	if result.MatchID == "" {
		return "", errors.New("lobby creation returned an empty match id")
	}
	return result.MatchID, nil
}

func (n *NakamaClient) SubmitResult(ctx context.Context, req NakamaResultRequest) ([]byte, error) {
	return n.CallRPC(ctx, "ikemen_submit_result", req)
}

func (n *NakamaClient) GetRating(ctx context.Context, game string) ([]byte, error) {
	if game == "" {
		game = n.cfg.Game
	}
	return n.CallRPC(ctx, "ikemen_get_rating", map[string]string{"game": game})
}

func (n *NakamaClient) ListMatches(ctx context.Context, authoritative bool, query, label string, minSize, maxSize, limit int) ([]map[string]any, error) {
	n.mu.Lock()
	session := n.session
	n.mu.Unlock()
	if session == nil {
		return nil, errors.New("nakama session is not established")
	}
	if limit <= 0 {
		limit = 100
	}
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))
	params.Set("authoritative", strconv.FormatBool(authoritative))
	if query != "" {
		params.Set("query", query)
	}
	if label != "" {
		params.Set("label", label)
	}
	if minSize > 0 {
		params.Set("min_size", strconv.Itoa(minSize))
	}
	if maxSize > 0 {
		params.Set("max_size", strconv.Itoa(maxSize))
	}
	scheme := "http"
	if n.cfg.UseTLS {
		scheme = "https"
	}
	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTTL)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.endpoint(scheme)+"/v2/match?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("nakama match listing failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Matches []map[string]any `json:"matches"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Matches, nil
}

func (n *NakamaClient) writeLoop(conn net.Conn, closed <-chan struct{}, queue <-chan []byte) {
	for {
		select {
		case <-closed:
			return
		case payload := <-queue:
			if payload == nil {
				continue
			}
			n.writeMu.Lock()
			err := writeWebSocketFrame(conn, 0x1, payload)
			n.writeMu.Unlock()
			if err != nil {
				n.emit(NakamaEvent{Type: "error", Error: fmt.Errorf("nakama websocket write: %w", err)})
				return
			}
		}
	}
}

func (n *NakamaClient) readLoop() {
	for {
		select {
		case <-n.closed:
			return
		default:
		}
		frame, opcode, err := readWebSocketFrame(n.currentReader())
		if err != nil {
			if !n.IsConnected() {
				return
			}
			log.Printf("Nakama websocket read failed: %v", err)
			n.emit(NakamaEvent{Type: "error", Error: err})
			n.Disconnect()
			return
		}
		switch opcode {
		case 0x1:
			if len(frame) > 0 {
				n.handleJSON(frame)
			}
		case 0x8:
			n.Disconnect()
			return
		case 0x9:
			n.mu.Lock()
			conn := n.conn
			queue := n.writeQueue
			closed := n.closed
			n.mu.Unlock()
			if conn != nil && queue != nil {
				// A pong is a control frame and must not wait behind game/replay data.
				n.writeMu.Lock()
				_ = writeWebSocketFrame(conn, 0xA, frame)
				n.writeMu.Unlock()
				_ = closed
			}
		case 0xA:
		}
	}
}

func (n *NakamaClient) currentReader() *bufio.Reader {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reader
}

func (n *NakamaClient) handleJSON(data []byte) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		n.emit(NakamaEvent{Type: "error", Error: fmt.Errorf("decode nakama realtime message: %w", err)})
		return
	}
	if v, ok := raw["matchmaker_ticket"]; ok {
		var p struct {
			Ticket string `json:"ticket"`
		}
		if json.Unmarshal(v, &p) == nil {
			n.mu.Lock()
			n.ticket = p.Ticket
			n.mu.Unlock()
			n.emit(NakamaEvent{Type: "matchmaker_ticket", Ticket: p.Ticket, Raw: raw})
		}
		return
	}
	if v, ok := raw["matchmaker_matched"]; ok {
		var matched NakamaMatchmakerMatched
		if err := json.Unmarshal(v, &matched); err != nil {
			n.emit(NakamaEvent{Type: "error", Error: err, Raw: raw})
			return
		}
		n.mu.Lock()
		n.ticket = ""
		n.mu.Unlock()
		n.emit(NakamaEvent{Type: "matchmaker_matched", Matchmaker: &matched, MatchID: matched.MatchID, MatchToken: matched.Token, Raw: raw})
		return
	}
	if v, ok := raw["match"]; ok {
		var p struct {
			MatchID string `json:"match_id"`
		}
		if json.Unmarshal(v, &p) == nil && p.MatchID != "" {
			n.mu.Lock()
			n.matchID = p.MatchID
			n.mu.Unlock()
			n.emit(NakamaEvent{Type: "match_joined", MatchID: p.MatchID, Raw: raw})
		}
		return
	}
	if v, ok := raw["match_data"]; ok {
		var p NakamaMatchData
		if err := json.Unmarshal(v, &p); err != nil {
			n.emit(NakamaEvent{Type: "error", Error: err, Raw: raw})
			return
		}
		if p.Data != "" {
			decoded, err := base64.StdEncoding.DecodeString(p.Data)
			if err != nil {
				n.emit(NakamaEvent{Type: "error", Error: fmt.Errorf("decode Nakama match data: %w", err), Raw: raw})
				return
			}
			p.BinaryData = decoded
		}
		switch uint32(p.OpCode) {
		case nakamaReplayHeaderOp:
			var header ReplayStreamHeader
			if err := json.Unmarshal(p.BinaryData, &header); err != nil {
				n.emit(NakamaEvent{Type: "error", Error: fmt.Errorf("decode replay header: %w", err), Raw: raw})
				return
			}
			n.ReplayBuffer().ApplyHeader(header)
			n.emit(NakamaEvent{Type: "replay_header", MatchID: p.MatchID, Payload: header, MatchData: &p, Raw: raw})
			return
		case nakamaReplayChunkOp:
			var chunk ReplayStreamChunk
			if err := json.Unmarshal(p.BinaryData, &chunk); err != nil {
				n.emit(NakamaEvent{Type: "error", Error: fmt.Errorf("decode replay chunk: %w", err), Raw: raw})
				return
			}
			if err := n.ReplayBuffer().ApplyChunk(chunk); err != nil {
				n.emit(NakamaEvent{Type: "error", Error: fmt.Errorf("store replay chunk: %w", err), Raw: raw})
				return
			}
			n.emit(NakamaEvent{Type: "replay_chunk", MatchID: p.MatchID, Payload: chunk, MatchData: &p, Raw: raw})
			return
		case nakamaReplayEndOp:
			var end ReplayStreamEnd
			if err := json.Unmarshal(p.BinaryData, &end); err != nil {
				n.emit(NakamaEvent{Type: "error", Error: fmt.Errorf("decode replay end: %w", err), Raw: raw})
				return
			}
			n.ReplayBuffer().ApplyEnd(end)
			n.emit(NakamaEvent{Type: "replay_end", MatchID: p.MatchID, Payload: end, MatchData: &p, Raw: raw})
			return
		case nakamaReplayResetOp:
			var reset struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(p.BinaryData, &reset)
			n.ReplayBuffer().Reset(reset.Reason)
			n.emit(NakamaEvent{Type: "replay_reset", MatchID: p.MatchID, Payload: reset, MatchData: &p, Raw: raw})
			return
		case nakamaRematchDecisionOp:
			var decision map[string]any
			if err := json.Unmarshal(p.BinaryData, &decision); err != nil {
				n.emit(NakamaEvent{Type: "error", Error: fmt.Errorf("decode rematch decision: %w", err), Raw: raw})
				return
			}
			n.emit(NakamaEvent{Type: "rematch_decision", MatchID: p.MatchID, Payload: decision, MatchData: &p, Raw: raw})
			return
		}
		n.emit(NakamaEvent{Type: "match_data", MatchData: &p, MatchID: p.MatchID, Raw: raw})
		return
	}
	if v, ok := raw["matchmaker_remove"]; ok {
		_ = v
		n.mu.Lock()
		n.ticket = ""
		n.mu.Unlock()
		n.emit(NakamaEvent{Type: "matchmaker_removed", Raw: raw})
		return
	}
	if v, ok := raw["channel_join"]; ok {
		var p struct {
			ChannelID string `json:"channel_id"`
		}
		if json.Unmarshal(v, &p) == nil {
			n.emit(NakamaEvent{Type: "channel_joined", ChannelID: p.ChannelID, Raw: raw})
		}
		return
	}
	if v, ok := raw["channel_message_ack"]; ok {
		n.emit(NakamaEvent{Type: "chat_ack", ChannelMessage: map[string]any{}, Raw: raw})
		_ = v
		return
	}
	if v, ok := raw["channel_message"]; ok {
		var msg map[string]any
		if json.Unmarshal(v, &msg) == nil {
			n.emit(NakamaEvent{Type: "chat_message", ChannelMessage: msg, Raw: raw})
		}
		return
	}
	n.emit(NakamaEvent{Type: "realtime", Raw: raw})
}

func (n *NakamaClient) AttachReplaySink() {
	// The sink is attached by the rollback session when a match begins. This
	// method exists as a named integration point for the engine-side bridge.
}

func (n *NakamaClient) PublishSignal(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return n.SendMatchData(nakamaSignalOp, data, true)
}

type nakamaReplaySink struct {
	client *NakamaClient
}

func (s *nakamaReplaySink) PublishReplayHeader(h ReplayStreamHeader) {
	if s == nil || s.client == nil {
		return
	}
	body, err := json.Marshal(h)
	if err != nil {
		log.Printf("Nakama replay header encode failed: %v", err)
		return
	}
	if err := s.client.SendMatchData(nakamaReplayHeaderOp, body, true); err != nil {
		log.Printf("Nakama replay header send failed: %v", err)
	}
}

func (s *nakamaReplaySink) PublishReplayChunk(c ReplayStreamChunk) {
	if s == nil || s.client == nil {
		return
	}
	body, err := json.Marshal(c)
	if err != nil {
		log.Printf("Nakama replay chunk encode failed: %v", err)
		return
	}
	if err := s.client.SendMatchData(nakamaReplayChunkOp, body, false); err != nil {
		log.Printf("Nakama replay chunk send failed: %v", err)
	}
}

func (s *nakamaReplaySink) PublishReplayEnd(e ReplayStreamEnd) {
	if s == nil || s.client == nil {
		return
	}
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := s.client.SendMatchData(nakamaReplayEndOp, body, true); err != nil {
		log.Printf("Nakama replay end send failed: %v", err)
	}
}

func (s *nakamaReplaySink) ReplayStreamReset(reason string) {
	if s == nil || s.client == nil {
		return
	}
	body, _ := json.Marshal(map[string]string{"reason": reason})
	if err := s.client.SendMatchData(nakamaReplayResetOp, body, true); err != nil {
		log.Printf("Nakama replay reset send failed: %v", err)
	}
}

// WebSocket framing ----------------------------------------------------------

func writeWebSocketFrame(w io.Writer, opcode byte, payload []byte) error {
	if w == nil {
		return errors.New("nil websocket writer")
	}
	var header [14]byte
	header[0] = 0x80 | opcode
	maskKey := [4]byte{}
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}
	maskBit := byte(0x80)
	plen := len(payload)
	idx := 2
	switch {
	case plen < 126:
		header[1] = maskBit | byte(plen)
	case plen <= 0xffff:
		header[1] = maskBit | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(plen))
		idx = 4
	default:
		header[1] = maskBit | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(plen))
		idx = 10
	}
	copy(header[idx:idx+4], maskKey[:])
	if _, err := w.Write(header[:idx+4]); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ maskKey[i&3]
	}
	_, err := w.Write(masked)
	return err
}

func readWebSocketFrame(r *bufio.Reader) ([]byte, byte, error) {
	if r == nil {
		return nil, 0, io.ErrClosedPipe
	}
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, 0, err
	}
	fin := h[0]&0x80 != 0
	opcode := h[0] & 0x0f
	if !fin {
		return nil, 0, errors.New("fragmented websocket frames are not supported")
	}
	masked := h[1]&0x80 != 0
	plen := int64(h[1] & 0x7f)
	if plen == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, 0, err
		}
		plen = int64(binary.BigEndian.Uint16(b[:]))
	} else if plen == 127 {
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, 0, err
		}
		plen = int64(binary.BigEndian.Uint64(b[:]))
		if plen < 0 {
			return nil, 0, errors.New("invalid websocket payload length")
		}
	}
	if plen > 64<<20 {
		return nil, 0, errors.New("websocket frame exceeds 64 MiB")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return nil, 0, err
		}
	}
	payload := make([]byte, int(plen))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, 0, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return payload, opcode, nil
}

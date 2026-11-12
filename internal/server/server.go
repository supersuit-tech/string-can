package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/listenaddr"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/store"
)

// Config is the serve-time configuration.
type Config struct {
	Home          string
	Name          string
	Port          int
	Host          string
	Local         bool
	OutboundOnly  bool
	WakeEvery     time.Duration
	TailscaleWait time.Duration
	WebhookURL    string
	WebhookToken  string
	HTTPClient    *http.Client
	Logger        *log.Logger
}

// Server is the HTTP listener and wake loop.
type Server struct {
	dir           home.Dir
	store         *store.Store
	priv          ed25519.PrivateKey
	pub           string
	port          int
	host          string
	local         bool
	outboundOnly  bool
	wakeEvery     time.Duration
	tailscaleWait time.Duration
	webhookURL    string
	webhookToken  string
	client        *http.Client
	log           *log.Logger

	lock            *home.Lock
	ln              net.Listener
	http            *http.Server
	baseURL         string
	bindIP          string
	advertiseHost   string
	swap            *swappingListener
	lastOfflineWarn time.Time
}

// New loads identity, name, and the database. It does not listen.
func New(cfg Config) (*Server, error) {
	if cfg.Home == "" {
		d, err := home.Resolve()
		if err != nil {
			return nil, err
		}
		cfg.Home = d.Path
	}
	dir := home.Dir{Path: cfg.Home}
	if err := dir.Ensure(); err != nil {
		return nil, err
	}
	priv, err := dir.Identity()
	if err != nil {
		return nil, err
	}
	if cfg.Name != "" {
		if err := dir.SetName(cfg.Name); err != nil {
			return nil, err
		}
	} else if !dir.HasName() {
		if err := dir.SetName(home.DefaultName); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(dir.DBPath())
	if err != nil {
		return nil, err
	}
	if cfg.WakeEvery == 0 {
		cfg.WakeEvery = 5 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = push.DefaultClient()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	return &Server{
		dir:           dir,
		store:         st,
		priv:          priv,
		pub:           canon.PublicHexFromPrivate(priv),
		port:          cfg.Port,
		host:          strings.TrimSpace(cfg.Host),
		local:         cfg.Local,
		outboundOnly:  cfg.OutboundOnly,
		wakeEvery:     cfg.WakeEvery,
		tailscaleWait: cfg.TailscaleWait,
		webhookURL:    cfg.WebhookURL,
		webhookToken:  cfg.WebhookToken,
		client:        cfg.HTTPClient,
		log:           logger,
	}, nil
}

// Dir returns the home directory.
func (s *Server) Dir() home.Dir { return s.dir }

// Store returns the SQLite store.
func (s *Server) Store() *store.Store { return s.store }

// PublicKey returns this agent's 64-hex public key.
func (s *Server) PublicKey() string { return s.pub }

// BaseURL returns the bound URL, with no trailing slash.
func (s *Server) BaseURL() string { return s.baseURL }

// Local reports whether the server was asked to bind on loopback.
func (s *Server) Local() bool { return s.local }

// OutboundOnly reports whether this agent cannot accept inbound push.
func (s *Server) OutboundOnly() bool { return s.outboundOnly }

func (s *Server) skipTailnet() bool { return s.local || s.outboundOnly }

// Close releases the listener, database, and serve lock.
func (s *Server) Close() error {
	if s.http != nil {
		_ = s.http.Close()
		s.http = nil
	}
	if s.ln != nil {
		_ = s.ln.Close()
		s.ln = nil
	}
	if s.lock != nil {
		s.lock.Unlock()
		s.lock = nil
	}
	return s.store.Close()
}

// Bind takes the home lock and listens on the Tailscale IPv4 address, or 127.0.0.1 with --local.
func (s *Server) Bind() error {
	return s.BindContext(context.Background())
}

// BindContext is Bind with a context used while waiting for Tailscale.
func (s *Server) BindContext(ctx context.Context) error {
	lock, err := s.dir.Lock()
	if err != nil {
		return err
	}
	s.lock = lock
	if err := s.dir.SetOutboundOnly(s.outboundOnly); err != nil {
		s.lock.Unlock()
		s.lock = nil
		return err
	}
	bindIP, advHost, err := s.resolveBind(ctx)
	if err != nil {
		s.lock.Unlock()
		s.lock = nil
		return err
	}
	if listenaddr.IsLoopbackHost(s.host) && !s.local {
		s.lock.Unlock()
		s.lock = nil
		return fmt.Errorf("--host %s is loopback; pass --local to bind on this machine only", s.host)
	}
	if err := s.listenOn(bindIP, advHost); err != nil {
		s.lock.Unlock()
		s.lock = nil
		return err
	}
	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	s.warnIfTailnetOffline(ctx)
	return nil
}

func (s *Server) resolveBind(ctx context.Context) (bindIP, advertiseHost string, err error) {
	if s.skipTailnet() {
		adv := "127.0.0.1"
		if s.host != "" && s.local && !s.outboundOnly {
			adv = s.host
		}
		return "127.0.0.1", adv, nil
	}
	onWait := func(msg string) { s.log.Print(msg) }
	ip, err := listenaddr.WaitForTailscale(ctx, s.tailscaleWait, 2*time.Second, onWait)
	if err != nil {
		return "", "", err
	}
	adv := ip
	if s.host != "" {
		adv = s.host
	}
	return ip, adv, nil
}

func (s *Server) listenOn(bindIP, advertiseHost string) error {
	addr := net.JoinHostPort(bindIP, strconv.Itoa(s.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			return fmt.Errorf("port %d is already in use", s.port)
		}
		return err
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return fmt.Errorf("unexpected listen address %s", ln.Addr())
	}
	s.port = tcpAddr.Port
	s.bindIP = tcpAddr.IP.String()
	s.advertiseHost = advertiseHost
	s.baseURL = listenaddr.URL(advertiseHost, s.port)
	s.swap = newSwappingListener(ln)
	s.ln = s.swap
	if err := s.dir.SetListenURL(s.baseURL); err != nil {
		ln.Close()
		s.ln = nil
		s.swap = nil
		return err
	}
	return nil
}

func (s *Server) rebindIfNeeded() {
	if s.skipTailnet() || s.swap == nil {
		return
	}
	ip, ok := listenaddr.TailscaleIPv4()
	if !ok || ip == s.bindIP {
		return
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(s.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.log.Printf("rebind to %s: %v", ip, err)
		return
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return
	}
	s.swap.Swap(ln)
	s.bindIP = tcpAddr.IP.String()
	if s.host == "" {
		s.advertiseHost = s.bindIP
		s.baseURL = listenaddr.URL(s.advertiseHost, s.port)
		if err := s.dir.SetListenURL(s.baseURL); err != nil {
			s.log.Printf("update listen.url: %v", err)
		}
	}
	s.log.Printf("rebound to Tailscale address %s (advertised %s)", s.bindIP, s.baseURL)
}

// Serve runs the HTTP server and wake loop until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil || s.http == nil {
		return fmt.Errorf("server is not bound")
	}
	go s.wake(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.http.Serve(s.ln)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
		err := <-errCh
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *Server) wake(ctx context.Context) {
	ticker := time.NewTicker(s.wakeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick retries undelivered outbound messages and webhook nudges.
func (s *Server) Tick(ctx context.Context) {
	s.rebindIfNeeded()
	s.warnIfTailnetOffline(ctx)
	s.retryOutbound(ctx)
	s.nudgeInbound(ctx)
}

func (s *Server) warnIfTailnetOffline(ctx context.Context) {
	if s.skipTailnet() {
		return
	}
	online, checked := listenaddr.SelfOnline(ctx)
	if !checked {
		return
	}
	if online {
		s.lastOfflineWarn = time.Time{}
		return
	}
	now := time.Now()
	if !s.lastOfflineWarn.IsZero() && now.Sub(s.lastOfflineWarn) < time.Minute {
		return
	}
	s.lastOfflineWarn = now
	s.log.Print(listenaddr.OfflineMessage)
}

func (s *Server) retryOutbound(ctx context.Context) {
	msgs, err := s.store.Undelivered()
	if err != nil {
		s.log.Printf("wake: list undelivered: %v", err)
		return
	}
	for _, m := range msgs {
		c, err := s.store.GetContact(m.PeerPub)
		if err != nil {
			continue
		}
		if c.PullOnly {
			if err := s.store.MarkDelivered(m.ID); err != nil {
				s.log.Printf("wake: mark pull-only delivered id=%d: %v", m.ID, err)
			}
			continue
		}
		if c.BaseURL == "" {
			continue
		}
		if err := push.Message(ctx, s.client, c.BaseURL, m.FromPub, m.Signature, []byte(m.Body)); err != nil {
			s.log.Printf("wake: deliver id=%d: %v", m.ID, err)
			if serr := s.store.SetLastError(m.ID, err.Error()); serr != nil {
				s.log.Printf("wake: save last_error id=%d: %v", m.ID, serr)
			}
			continue
		}
		if err := s.store.MarkDelivered(m.ID); err != nil {
			s.log.Printf("wake: mark delivered id=%d: %v", m.ID, err)
		}
	}
}

func (s *Server) nudgeInbound(ctx context.Context) {
	if s.webhookURL == "" {
		return
	}
	msgs, err := s.store.UnnudgedInbound()
	if err != nil {
		s.log.Printf("wake: list unnudged: %v", err)
		return
	}
	for _, m := range msgs {
		if err := push.Nudge(ctx, s.client, s.webhookURL, s.webhookToken, m.FromPub, m.PeerPub); err != nil {
			s.log.Printf("wake: nudge id=%d: %v", m.ID, err)
			continue
		}
		if err := s.store.MarkNudged(m.ID); err != nil {
			s.log.Printf("wake: mark nudged id=%d: %v", m.ID, err)
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		if r.Method == http.MethodGet {
			s.handleInfo(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if path == "connect" {
		if r.Method == http.MethodPost {
			s.handleConnect(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if rest, ok := strings.CutPrefix(path, "allow/"); ok && !strings.Contains(rest, "/") {
		if r.Method == http.MethodGet {
			s.handleAllow(w, r, rest)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if strings.Contains(path, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	norm, err := canon.NormalizeKey(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleThread(w, r, norm)
	case http.MethodPost:
		s.handlePost(w, r, norm)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	name, err := s.dir.Name()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		V            int    `json:"v"`
		Name         string `json:"name"`
		PublicKey    string `json:"public_key"`
		OutboundOnly bool   `json:"outbound_only,omitempty"`
	}{V: 1, Name: name, PublicKey: s.pub, OutboundOnly: s.outboundOnly})
}

func (s *Server) handleAllow(w http.ResponseWriter, _ *http.Request, key string) {
	norm, err := canon.NormalizeKey(key)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	ok, err := s.store.IsContact(norm)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"contact": ok})
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	limited := http.MaxBytesReader(w, r.Body, 16<<10)
	body, err := io.ReadAll(limited)
	if err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeError(w, http.StatusBadRequest, "malformed body")
		return
	}
	var hs push.HandshakeBody
	if err := json.Unmarshal(body, &hs); err != nil {
		writeError(w, http.StatusBadRequest, "malformed body")
		return
	}
	if hs.V != 1 {
		writeError(w, http.StatusBadRequest, "unsupported version")
		return
	}
	key, err := canon.NormalizeKey(hs.PublicKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid public key")
		return
	}
	sig := r.Header.Get("X-String-Can-Signature")
	if err := canon.Verify(key, body, sig); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	baseURL := ""
	if strings.TrimSpace(hs.BaseURL) != "" {
		var err error
		baseURL, err = NormalizeBaseURL(hs.BaseURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		fromLoopback := remoteIsLoopback(r)
		if listenaddr.IsLoopbackURL(baseURL) && !fromLoopback && !hs.OutboundOnly {
			writeError(w, http.StatusBadRequest, "loopback URL is not reachable from here")
			return
		}
	} else if !hs.OutboundOnly {
		writeError(w, http.StatusBadRequest, "base URL is empty")
		return
	}
	label := hs.Name
	if label == "" {
		label = canon.ShortKey(key)
	}
	if err := s.store.UpsertContact(store.Contact{
		PublicKey:  key,
		Label:      label,
		BaseURL:    baseURL,
		StatedName: hs.Name,
		PullOnly:   hs.OutboundOnly,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"public_key": key,
	})
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request, peer string) {
	since := int64(0)
	if raw := r.URL.Query().Get("since"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid since")
			return
		}
		since = n
	}
	msgs, err := s.store.VerifiedThread(peer, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	type httpMsg struct {
		ID        int64  `json:"id"`
		From      string `json:"from"`
		Ts        int64  `json:"ts"`
		Text      string `json:"text"`
		Signature string `json:"signature"`
	}
	out := make([]httpMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, httpMsg{
			ID:        m.ID,
			From:      m.FromPub,
			Ts:        m.Ts,
			Text:      m.Text,
			Signature: m.Signature,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Messages []httpMsg `json:"messages"`
	}{Messages: out})
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, pathKey string) {
	limited := http.MaxBytesReader(w, r.Body, canon.MaxBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeError(w, http.StatusBadRequest, "malformed body")
		return
	}
	sig := r.Header.Get("X-String-Can-Signature")
	if err := canon.Verify(pathKey, body, sig); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	msg, err := canon.ParseAndCheck(body, pathKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ok, err := s.store.IsContact(pathKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, push.ForbiddenContact)
		return
	}
	id, dup, err := s.store.InsertMessage(store.Message{
		PeerPub:   pathKey,
		FromPub:   msg.From,
		Ts:        msg.Ts,
		Text:      msg.Text,
		Body:      string(body),
		Signature: sig,
		Delivered: true,
		Nudged:    false,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if dup {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "duplicate": true})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func isTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return strings.Contains(err.Error(), "http: request body too large")
}

func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return isAddrInUse(op.Err)
	}
	var sys *os.SyscallError
	if errors.As(err, &sys) {
		return isAddrInUse(sys.Err)
	}
	return strings.Contains(err.Error(), "address already in use")
}

func remoteIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return listenaddr.IsLoopbackHost(host)
}

// NormalizeBaseURL stores a peer URL with no trailing slash. Only http:// is accepted.
func NormalizeBaseURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "http://") {
		return "", fmt.Errorf("base URL must start with http://")
	}
	s = strings.TrimRight(s, "/")
	if s == "http://" {
		return "", fmt.Errorf("base URL must start with http://")
	}
	rest := strings.TrimPrefix(s, "http://")
	if rest == "" || strings.Contains(rest, "://") {
		return "", fmt.Errorf("base URL must start with http://")
	}
	if listenaddr.HostOf(s) == "" {
		return "", fmt.Errorf("base URL host is empty")
	}
	return s, nil
}

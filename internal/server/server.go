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
	Home         string
	Name         string
	Port         int
	WakeEvery    time.Duration
	WebhookURL   string
	WebhookToken string
	HTTPClient   *http.Client
}

// Server is the HTTP listener and wake loop.
type Server struct {
	dir          home.Dir
	store        *store.Store
	priv         ed25519.PrivateKey
	pub          string
	port         int
	wakeEvery    time.Duration
	webhookURL   string
	webhookToken string
	client       *http.Client

	lock    *home.Lock
	ln      net.Listener
	http    *http.Server
	baseURL string
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
	return &Server{
		dir:          dir,
		store:        st,
		priv:         priv,
		pub:          canon.PublicHexFromPrivate(priv),
		port:         cfg.Port,
		wakeEvery:    cfg.WakeEvery,
		webhookURL:   cfg.WebhookURL,
		webhookToken: cfg.WebhookToken,
		client:       cfg.HTTPClient,
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

// Close releases the database and serve lock.
func (s *Server) Close() error {
	if s.lock != nil {
		s.lock.Unlock()
		s.lock = nil
	}
	return s.store.Close()
}

// Bind takes the home lock and listens on the Tailscale IPv4 address, or 127.0.0.1.
func (s *Server) Bind() error {
	lock, err := s.dir.Lock()
	if err != nil {
		return err
	}
	s.lock = lock
	host := listenaddr.Host()
	addr := net.JoinHostPort(host, strconv.Itoa(s.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.lock.Unlock()
		s.lock = nil
		if isAddrInUse(err) {
			return fmt.Errorf("port %d is already in use", s.port)
		}
		return err
	}
	s.ln = ln
	s.baseURL = "http://" + ln.Addr().String()
	if err := s.dir.SetListenURL(s.baseURL); err != nil {
		ln.Close()
		s.lock.Unlock()
		s.lock = nil
		return err
	}
	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return nil
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
	s.retryOutbound(ctx)
	s.nudgeInbound(ctx)
}

func (s *Server) retryOutbound(ctx context.Context) {
	msgs, err := s.store.Undelivered()
	if err != nil {
		log.Printf("wake: list undelivered: %v", err)
		return
	}
	for _, m := range msgs {
		c, err := s.store.GetContact(m.PeerPub)
		if err != nil {
			continue
		}
		if err := push.Message(ctx, s.client, c.BaseURL, m.FromPub, m.Signature, []byte(m.Body)); err != nil {
			log.Printf("wake: deliver id=%d: %v", m.ID, err)
			continue
		}
		if err := s.store.MarkDelivered(m.ID); err != nil {
			log.Printf("wake: mark delivered id=%d: %v", m.ID, err)
		}
	}
}

func (s *Server) nudgeInbound(ctx context.Context) {
	if s.webhookURL == "" {
		return
	}
	msgs, err := s.store.UnnudgedInbound()
	if err != nil {
		log.Printf("wake: list unnudged: %v", err)
		return
	}
	for _, m := range msgs {
		if err := push.Nudge(ctx, s.client, s.webhookURL, s.webhookToken, m.FromPub, m.PeerPub); err != nil {
			log.Printf("wake: nudge id=%d: %v", m.ID, err)
			continue
		}
		if err := s.store.MarkNudged(m.ID); err != nil {
			log.Printf("wake: mark nudged id=%d: %v", m.ID, err)
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		if r.Method == http.MethodGet {
			s.handleInfo(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(key, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	norm, err := canon.NormalizeKey(key)
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
		V         int    `json:"v"`
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}{V: 1, Name: name, PublicKey: s.pub})
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
		writeError(w, http.StatusForbidden, "not a contact")
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

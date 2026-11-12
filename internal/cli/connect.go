package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/listenaddr"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/server"
	"github.com/supersuit-tech/string-can/internal/store"
)

// ProbeWait is how long connect retries GET / when the peer is not yet reachable.
var ProbeWait = 20 * time.Second

// ProbeInterval is the pause between connect probes.
var ProbeInterval = time.Second

func (e env) cmdInvite(args []string) int {
	if help, err := parseFlags("invite", args, nil); help {
		fmt.Fprint(e.stdout, commandHelp("invite"))
		return 0
	} else if err != nil {
		return e.fail(err)
	}
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	u := ""
	if stored, err := dir.ListenURL(); err == nil && stored != "" {
		u = stored
	} else {
		u = listenaddr.WouldBind(listenaddr.DefaultPort)
	}
	if listenaddr.IsLoopbackURL(u) {
		fmt.Fprintln(e.stderr, "string-can: warning: this invite URL is loopback; do not give it to a peer on another machine")
	}
	fmt.Fprintf(e.stdout, "string-can connect %s\n", u)
	return 0
}

func (e env) cmdConnect(ctx context.Context, args []string) int {
	var label string
	var local bool
	fs := newFlagSet("connect")
	fs.StringVar(&label, "name", "", "local label for this contact")
	fs.BoolVar(&local, "local", false, "allow a loopback peer URL")
	pos, help, err := parseWith(fs, args, map[string]bool{"name": true})
	if help {
		fmt.Fprint(e.stdout, commandHelp("connect"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	var wantKey, rawURL string
	switch len(pos) {
	case 1:
		rawURL = pos[0]
	case 2:
		wantKey, err = canon.NormalizeKey(pos[0])
		if err != nil {
			return e.fail(err)
		}
		rawURL = pos[1]
	default:
		return e.fail(fmt.Errorf("usage: string-can connect <url> [--name <label>] [--local]"))
	}
	baseURL, err := server.NormalizeBaseURL(rawURL)
	if err != nil {
		return e.fail(err)
	}
	if listenaddr.IsLoopbackURL(baseURL) && !local {
		return e.fail(fmt.Errorf("refusing loopback URL %s (that would be this machine). Pass --local for two agents on one host, or use the peer's Tailscale / MagicDNS URL", baseURL))
	}
	stated, gotKey, reachable, mismatch, err := probePeer(ctx, baseURL, wantKey)
	if err != nil {
		return e.fail(err)
	}
	if mismatch {
		return e.fail(fmt.Errorf("peer public key mismatch"))
	}
	key := wantKey
	if key == "" {
		key = gotKey
	}
	if key == "" {
		return e.fail(fmt.Errorf("peer was not reachable; cannot learn public key. Retry connect, or pass the public key as well"))
	}
	if label == "" {
		if reachable && stated != "" {
			label = stated
		} else {
			label = canon.ShortKey(key)
		}
	}
	st, dir, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	c := store.Contact{
		PublicKey:  key,
		Label:      label,
		BaseURL:    baseURL,
		StatedName: stated,
	}
	if err := st.UpsertContact(c); err != nil {
		return e.fail(err)
	}
	if !reachable {
		fmt.Fprintln(e.stderr, "string-can: saved; delivery will retry")
	} else {
		e.tryHandshake(ctx, dir, baseURL)
	}
	return e.writeJSON(contactJSON{
		Label:      c.Label,
		PublicKey:  c.PublicKey,
		BaseURL:    c.BaseURL,
		StatedName: c.StatedName,
	})
}

func (e env) tryHandshake(ctx context.Context, dir home.Dir, peerURL string) {
	priv, err := dir.Identity()
	if err != nil {
		return
	}
	name, err := dir.Name()
	if err != nil {
		name = home.DefaultName
	}
	ourURL := ""
	if stored, err := dir.ListenURL(); err == nil {
		ourURL = stored
	}
	if ourURL == "" {
		ourURL = listenaddr.WouldBind(listenaddr.DefaultPort)
	}
	body, err := push.EncodeHandshake(canon.PublicHexFromPrivate(priv), name, ourURL)
	if err != nil {
		return
	}
	sig := canon.Sign(priv, body)
	if err := push.Handshake(ctx, push.DefaultClient(), peerURL, sig, body); err != nil {
		fmt.Fprintf(e.stderr, "string-can: they have not added you yet; both sides must connect. Ask them to run:\nstring-can connect %s\n", ourURL)
	}
}

func probePeer(ctx context.Context, baseURL, wantKey string) (stated, gotKey string, reachable, mismatch bool, err error) {
	deadline := time.Now().Add(ProbeWait)
	for {
		stated, gotKey, reachable, mismatch, err = probeOnce(ctx, baseURL, wantKey)
		if err != nil {
			return "", "", false, false, err
		}
		if mismatch || reachable {
			return stated, gotKey, reachable, mismatch, nil
		}
		if !time.Now().Before(deadline) {
			return "", "", false, false, nil
		}
		timer := time.NewTimer(ProbeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", "", false, false, nil
		case <-timer.C:
		}
	}
}

func probeOnce(ctx context.Context, baseURL, wantKey string) (stated, gotKey string, reachable, mismatch bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info, err := push.GetInfo(ctx, push.DefaultClient(), baseURL)
	if err != nil {
		return "", "", false, false, nil
	}
	if info.PublicKey == "" {
		return "", "", false, false, nil
	}
	got, nerr := canon.NormalizeKey(info.PublicKey)
	if nerr != nil {
		return "", "", false, false, nil
	}
	if wantKey != "" && got != wantKey {
		return "", got, false, true, nil
	}
	return info.Name, got, true, false, nil
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/listenaddr"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/store"
	"github.com/supersuit-tech/string-can/internal/wake"
)

func (e env) cmdFetch(ctx context.Context, args []string) int {
	var watch bool
	every := 5 * time.Second
	fs := newFlagSet("fetch")
	fs.BoolVar(&watch, "watch", false, "poll until interrupted")
	fs.DurationVar(&every, "every", 5*time.Second, "watch poll interval")
	pos, help, err := parseWith(fs, args, map[string]bool{"every": true})
	if help {
		fmt.Fprint(e.stdout, commandHelp("fetch"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if len(pos) != 1 {
		return e.fail(fmt.Errorf("usage: string-can fetch <public-key> [--watch] [--every <duration>]"))
	}
	key, err := canon.NormalizeKey(pos[0])
	if err != nil {
		return e.fail(err)
	}
	if every <= 0 {
		every = 5 * time.Second
	}
	st, dir, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	c, err := st.GetContact(key)
	if err != nil {
		return e.fail(err)
	}
	if c.BaseURL == "" {
		return e.fail(fmt.Errorf("contact has no URL to fetch from"))
	}
	priv, err := dir.Identity()
	if err != nil {
		return e.fail(err)
	}
	ourKey := canon.PublicHexFromPrivate(priv)
	if watch {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		lk, lerr := dir.FetchLock(key)
		if lerr != nil {
			if errors.Is(lerr, home.ErrAlreadyWatching) {
				return e.fail(fmt.Errorf("already watching %s", canon.ShortKey(key)))
			}
			return e.fail(lerr)
		}
		defer lk.Unlock()
	}
	for {
		// supervise runs this loop as `fetch --watch`. A miss here is how
		// an outbound-only agent notices a frozen peer and wakes it.
		n, code := e.fetchOnce(ctx, st, c, ourKey)
		if code != 0 && !watch {
			return code
		}
		if !watch {
			fmt.Fprintf(e.stdout, "fetched %d\n", n)
			return 0
		}
		if n > 0 {
			fmt.Fprintf(e.stdout, "fetched %d\n", n)
			if f, ok := e.stdout.(*os.File); ok {
				_ = f.Sync()
			}
		}
		timer := time.NewTimer(every)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		}
	}
}

func (e env) fetchOnce(ctx context.Context, st *store.Store, c store.Contact, ourKey string) (int, int) {
	key := c.PublicKey
	remote, err := push.Thread(ctx, push.PeerClient(), c.BaseURL, ourKey)
	if err != nil && !c.PullOnly && c.WakeURL != "" && wake.FetchNeedsWake(err) {
		if wake.Try(ctx, st, key, c.BaseURL, ourKey, e.logf()) {
			remote, err = push.Thread(ctx, push.PeerClient(), c.BaseURL, ourKey)
		}
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "string-can: %v\n", err)
		return 0, 1
	}
	n := 0
	for _, rm := range remote {
		from, err := canon.NormalizeKey(rm.From)
		if err != nil {
			continue
		}
		if from != ourKey && from != key {
			continue
		}
		body, err := canon.Encode(from, rm.Ts, rm.Text)
		if err != nil {
			continue
		}
		if err := canon.Verify(from, body, rm.Signature); err != nil {
			continue
		}
		id, dup, err := st.InsertMessage(store.Message{
			PeerPub:   key,
			FromPub:   from,
			Ts:        rm.Ts,
			Text:      rm.Text,
			Body:      string(body),
			Signature: rm.Signature,
			Delivered: true,
			Nudged:    true,
		})
		if err != nil || dup {
			continue
		}
		n++
		if code := e.writeJSON(struct {
			ID   int64  `json:"id"`
			From string `json:"from"`
			Ts   int64  `json:"ts"`
			Text string `json:"text"`
		}{id, from, rm.Ts, rm.Text}); code != 0 {
			return n, code
		}
	}
	return n, 0
}

func (e env) cmdStatus(args []string) int {
	if help, err := parseFlags("status", args, nil); help {
		fmt.Fprint(e.stdout, commandHelp("status"))
		return 0
	} else if err != nil {
		return e.fail(err)
	}
	st, _, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	msgs, err := st.Undelivered()
	if err != nil {
		return e.fail(err)
	}
	for _, m := range msgs {
		state := "pending"
		if m.State == store.StateUnconfirmed {
			state = store.StateUnconfirmed
		}
		if code := e.writeJSON(struct {
			ID        int64  `json:"id"`
			Peer      string `json:"peer"`
			Ts        int64  `json:"ts"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
		}{m.ID, m.PeerPub, m.Ts, state, m.LastError}); code != 0 {
			return code
		}
	}
	return 0
}

func (e env) cmdDoctor(ctx context.Context, args []string) int {
	if help, err := parseFlags("doctor", args, nil); help {
		fmt.Fprint(e.stdout, commandHelp("doctor"))
		return 0
	} else if err != nil {
		return e.fail(err)
	}
	ok := true
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	priv, err := dir.Identity()
	if err != nil {
		fmt.Fprintf(e.stdout, "identity: missing (%v)\n", err)
		return 1
	}
	ourKey := canon.PublicHexFromPrivate(priv)
	name, _ := dir.Name()
	fmt.Fprintf(e.stdout, "identity: ok\n")
	fmt.Fprintf(e.stdout, "  public_key: %s\n", ourKey)
	fmt.Fprintf(e.stdout, "  name: %s\n", name)

	serving, err := dir.Serving()
	if err != nil {
		fmt.Fprintf(e.stdout, "server: unknown (%v)\n", err)
		ok = false
	} else if !serving {
		fmt.Fprintln(e.stdout, "server: not running")
		ok = false
	} else {
		fmt.Fprintln(e.stdout, "server: running")
	}

	listen, lerr := dir.ListenURL()
	if lerr != nil || listen == "" {
		fmt.Fprintln(e.stdout, "  listen: (none)")
		ok = false
		e.reportTailnet(ctx, "", dir.OutboundOnly(), &ok)
	} else {
		fmt.Fprintf(e.stdout, "  listen: %s\n", listen)
		host := listenaddr.HostOf(listen)
		outbound := dir.OutboundOnly()
		switch {
		case outbound:
			fmt.Fprintln(e.stdout, "  bind: loopback (outbound-only; peers cannot push)")
		case listenaddr.IsTailscaleIPv4(host):
			fmt.Fprintln(e.stdout, "  bind: tailnet")
		case listenaddr.IsLoopbackHost(host):
			fmt.Fprintln(e.stdout, "  bind: loopback (OK for same-machine; unreachable from other machines)")
		default:
			fmt.Fprintf(e.stdout, "  bind: hostname %s (MagicDNS)\n", host)
		}
		if serving && !dialOK(listen) {
			fmt.Fprintln(e.stdout, "  reachability: listen URL did not accept a connection")
			ok = false
		}
		e.reportTailnet(ctx, host, outbound, &ok)
	}

	webhook := os.Getenv("STRING_CAN_WEBHOOK_URL")
	if webhook == "" {
		fmt.Fprintln(e.stdout, "webhook: not set")
	} else {
		fmt.Fprintln(e.stdout, "webhook: configured")
	}
	fmt.Fprintf(e.stdout, "wake proxy: %s\n", wake.ProxyLabel(""))

	st, err := store.Open(dir.DBPath())
	if err != nil {
		fmt.Fprintf(e.stdout, "store: %v\n", err)
		return 1
	}
	defer st.Close()
	pending, err := st.CountPending()
	if err != nil {
		fmt.Fprintf(e.stdout, "pending: %v\n", err)
		ok = false
	} else {
		fmt.Fprintf(e.stdout, "pending: %d\n", pending)
	}
	unconfirmed, err := st.CountUnconfirmed()
	if err != nil {
		fmt.Fprintf(e.stdout, "unconfirmed: %v\n", err)
		ok = false
	} else if unconfirmed > 0 {
		fmt.Fprintf(e.stdout, "unconfirmed: %d\n", unconfirmed)
	}

	contacts, err := st.ListContacts()
	if err != nil {
		fmt.Fprintf(e.stdout, "contacts: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "contacts: %d\n", len(contacts))
	client := push.DefaultClient()
	for _, c := range contacts {
		line := fmt.Sprintf("  %s (%s)", c.Label, canon.ShortKey(c.PublicKey))
		if listen != "" && listenaddr.IsLoopbackURL(listen) && c.BaseURL != "" && !listenaddr.IsLoopbackURL(c.BaseURL) && !dir.OutboundOnly() && !c.PullOnly {
			fmt.Fprintf(e.stdout, "%s: remote URL while this server is on loopback\n", line)
			ok = false
		}
		fmt.Fprintf(e.stdout, "%s: %s\n", line, contactStatus(ctx, client, c, ourKey, &ok))
		fmt.Fprintf(e.stdout, "    %s\n", wakeStatus(c))
	}
	if dir.OutboundOnly() {
		e.reportFetch(dir, contacts, &ok)
	}
	if !ok {
		return 1
	}
	return 0
}

func contactStatus(ctx context.Context, client *http.Client, c store.Contact, ourKey string, ok *bool) string {
	if c.PullOnly {
		if c.BaseURL == "" {
			return "pull-only (expected)"
		}
		if _, err := push.GetInfo(ctx, client, c.BaseURL); err != nil {
			return "pull-only (expected)"
		}
		return "reachable, pull-only (will not push)"
	}
	info, err := push.GetInfo(ctx, client, c.BaseURL)
	if err != nil {
		*ok = false
		return fmt.Sprintf("unreachable (%v)", err)
	}
	got, nerr := canon.NormalizeKey(info.PublicKey)
	if nerr != nil || got != c.PublicKey {
		*ok = false
		return "reachable, public key mismatch"
	}
	allowed, checked, err := push.Allow(ctx, client, c.BaseURL, ourKey)
	switch {
	case err != nil:
		*ok = false
		return fmt.Sprintf("reachable, mutual check failed (%v)", err)
	case !checked:
		return "reachable, mutual unknown"
	case !allowed:
		*ok = false
		return "reachable, they have not added you; ask them to run connect with your URL"
	default:
		return "reachable, mutual"
	}
}

func wakeStatus(c store.Contact) string {
	if c.WakeURL == "" {
		return "wake: not set"
	}
	token := "(not set)"
	if c.WakeTokenSet {
		token = wake.MaskedToken
	}
	return fmt.Sprintf("wake: %s token %s proxy %s", wake.MaskURL(c.WakeURL), token, wake.ProxyLabel(c.WakeProxy))
}

// reportFetch says whether fetch --watch is alive for each peer an
// outbound-only agent must poll. serve retries outbound delivery; if it
// dies and only fetch is up, sending stalls with no signal.
func (e env) reportFetch(dir home.Dir, contacts []store.Contact, ok *bool) {
	n := 0
	for _, c := range contacts {
		if c.BaseURL == "" {
			continue
		}
		n++
		name := c.Label
		if name == "" {
			name = canon.ShortKey(c.PublicKey)
		}
		watching, err := dir.Fetching(c.PublicKey)
		if err != nil {
			fmt.Fprintf(e.stdout, "fetch: %s (%v)\n", name, err)
			*ok = false
			continue
		}
		if !watching {
			fmt.Fprintf(e.stdout, "fetch: %s not watching; run string-can supervise --outbound-only\n", name)
			*ok = false
			continue
		}
		fmt.Fprintf(e.stdout, "fetch: %s watching\n", name)
	}
	if n == 0 {
		fmt.Fprintln(e.stdout, "fetch: no peers yet")
	}
}

func (e env) reportTailnet(ctx context.Context, bindHost string, outbound bool, ok *bool) {
	if outbound {
		fmt.Fprintln(e.stdout, "tailnet: unused (outbound-only)")
		return
	}
	if listenaddr.IsLoopbackHost(bindHost) {
		fmt.Fprintln(e.stdout, "tailnet: unused (loopback bind)")
		return
	}
	ip, hasIP := listenaddr.TailscaleIPv4()
	online, checked := listenaddr.SelfOnline(ctx)
	switch {
	case checked && !online:
		fmt.Fprintln(e.stdout, "tailnet: offline (Self.Online=false; peers cannot reach you)")
		*ok = false
	case checked && online:
		if hasIP {
			fmt.Fprintf(e.stdout, "tailnet: online (%s)\n", ip)
		} else {
			fmt.Fprintln(e.stdout, "tailnet: online")
		}
	case hasIP:
		fmt.Fprintf(e.stdout, "tailnet: %s assigned; Self.Online unknown (tailscale status --json failed)\n", ip)
	case bindHost == "":
		fmt.Fprintln(e.stdout, "tailnet: no 100.x interface")
		*ok = false
	default:
		fmt.Fprintln(e.stdout, "tailnet: Self.Online unknown (tailscale status --json failed)")
	}
}

func dialOK(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

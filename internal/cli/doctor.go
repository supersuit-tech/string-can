package cli

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/listenaddr"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/store"
)

func (e env) cmdFetch(ctx context.Context, args []string) int {
	_, pos, help, err := flags("fetch", args, nil)
	if help {
		fmt.Fprint(e.stdout, commandHelp("fetch"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if len(pos) != 1 {
		return e.fail(fmt.Errorf("usage: string-can fetch <public-key>"))
	}
	key, err := canon.NormalizeKey(pos[0])
	if err != nil {
		return e.fail(err)
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
	priv, err := dir.Identity()
	if err != nil {
		return e.fail(err)
	}
	ourKey := canon.PublicHexFromPrivate(priv)
	remote, err := push.Thread(ctx, push.PeerClient(), c.BaseURL, ourKey)
	if err != nil {
		return e.fail(err)
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
			return code
		}
	}
	fmt.Fprintf(e.stdout, "fetched %d\n", n)
	return 0
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
	} else {
		fmt.Fprintf(e.stdout, "  listen: %s\n", listen)
		host := listenaddr.HostOf(listen)
		switch {
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
	}

	webhook := os.Getenv("STRING_CAN_WEBHOOK_URL")
	if webhook == "" {
		fmt.Fprintln(e.stdout, "webhook: not set")
	} else {
		fmt.Fprintln(e.stdout, "webhook: configured")
	}

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
		if listen != "" && listenaddr.IsLoopbackURL(listen) && !listenaddr.IsLoopbackURL(c.BaseURL) {
			fmt.Fprintf(e.stdout, "%s: remote URL while this server is on loopback\n", line)
			ok = false
		}
		info, err := push.GetInfo(ctx, client, c.BaseURL)
		if err != nil {
			fmt.Fprintf(e.stdout, "%s: unreachable (%v)\n", line, err)
			ok = false
			continue
		}
		got, nerr := canon.NormalizeKey(info.PublicKey)
		if nerr != nil || got != c.PublicKey {
			fmt.Fprintf(e.stdout, "%s: reachable, public key mismatch\n", line)
			ok = false
			continue
		}
		allowed, checked, err := push.Allow(ctx, client, c.BaseURL, ourKey)
		switch {
		case err != nil:
			fmt.Fprintf(e.stdout, "%s: reachable, mutual check failed (%v)\n", line, err)
			ok = false
		case !checked:
			fmt.Fprintf(e.stdout, "%s: reachable, mutual unknown\n", line)
		case !allowed:
			fmt.Fprintf(e.stdout, "%s: reachable, they have not added you; ask them to run connect with your URL\n", line)
			ok = false
		default:
			fmt.Fprintf(e.stdout, "%s: reachable, mutual\n", line)
		}
	}
	if !ok {
		return 1
	}
	return 0
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

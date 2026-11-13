package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/files"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/listenaddr"
	"github.com/supersuit-tech/string-can/internal/push"
	"github.com/supersuit-tech/string-can/internal/server"
	"github.com/supersuit-tech/string-can/internal/store"
	"github.com/supersuit-tech/string-can/internal/wake"
)

const usage = `string-can — signed messages between agents

Both sides must add each other as contacts. Run string-can invite and give
the printed connect command to the other agent, then connect to their URL.

Usage:
  string-can pubkey
  string-can url
  string-can name <display-name>
  string-can serve [--name <display-name>] [--port <port>] [--host <name>] [--local] [--outbound-only] [--wait <duration>]
  string-can supervise [--name <display-name>] [--port <port>] [--host <name>] [--local] [--outbound-only] [--wait <duration>] [--every <duration>] [public-key...]
  string-can invite
  string-can connect <url> [--name <label>] [--local] [--pull-only] [--wake-url <url> [--wake-token-file <path> | --wake-token-env <NAME> | --wake-token <literal>]]
  string-can connect <public-key> <base-url> [--name <label>] [--local] [--pull-only] [--wake-url <url> [--wake-token-file <path> | --wake-token-env <NAME> | --wake-token <literal>]]
  string-can connect --pull-only <public-key> [--name <label>]
  string-can rename <public-key> <label>
  string-can contacts
  string-can wake set <public-key> <url> [--token-file <path> | --token-env <NAME> | --token <literal>] [--proxy <url|direct|env>]
  string-can wake show <public-key>
  string-can wake clear <public-key>
  string-can send [--auto] [--text-file <path>] [--file <path>]... <public-key> [<text>]
  string-can thread <public-key> [--since <id>]
  string-can inbox [--peek] [--watch] [--every <duration>] [public-key...]
  string-can fetch <public-key> [--watch] [--every <duration>]
  string-can download <public-key> <message-id> [--out <dir>]
  string-can status
  string-can doctor

Environment:
  STRING_CAN_HOME            data directory (default ~/.string-can)
  STRING_CAN_WEBHOOK_URL     POST target when a message arrives here
  STRING_CAN_WEBHOOK_TOKEN   Bearer token for that inbound webhook
  STRING_CAN_PEER_TIMEOUT    deadline for one send or thread check (default 20s)
  STRING_CAN_MAX_FILE_BYTES  per-file limit (default 26214400, 25 MB)
  STRING_CAN_WAKE_WAIT       how long to poll a peer after waking it (default 60s)
  STRING_CAN_WAKE_INTERVAL   minimum gap between wakes of one contact (default 2m)
  STRING_CAN_WAKE_POLL       pause between those health checks (default 1s)
  STRING_CAN_WAKE_PROXY      proxy for wake POSTs only (http, https, socks5 URL, or direct).
                             Unset uses HTTP_PROXY / HTTPS_PROXY. A per-contact --proxy wins.
  STRING_CAN_AUTO_CAP        consecutive send --auto calls per peer before hold (default 6, 0 disables)
  STRING_CAN_AUTO_COOLDOWN   minimum gap between send --auto calls to one peer (default 0s, off)
`

type env struct {
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
}

// sendUsageLine is the send command synopsis. Reply text belongs in a file
// or on stdin so a shell does not expand $ before string-can sees it.
const sendUsageLine = "string-can send [--auto] [--text-file <path>] [--file <path>]... <public-key> [<text>]"

var errSendTextMissing = fmt.Errorf("message missing: pass <text>, --text-file <path>, or pipe the message on stdin")

// Run executes the CLI and returns a process exit code.
func Run(args []string) int {
	return RunContext(context.Background(), args, os.Stdout, os.Stderr)
}

// RunContext is Run with a context and streams, for tests.
func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return (env{stdout: stdout, stderr: stderr, stdin: os.Stdin}).run(ctx, args)
}

func (e env) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	cmd := args[0]
	rest := args[1:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Fprint(e.stdout, usage)
		return 0
	}
	switch cmd {
	case "pubkey":
		return e.cmdPubkey(rest)
	case "url":
		return e.cmdURL(rest)
	case "name":
		return e.cmdName(rest)
	case "serve":
		return e.cmdServe(ctx, rest)
	case "supervise":
		return e.cmdSupervise(ctx, rest)
	case "invite":
		return e.cmdInvite(rest)
	case "connect":
		return e.cmdConnect(ctx, rest)
	case "rename":
		return e.cmdRename(rest)
	case "contacts":
		return e.cmdContacts(rest)
	case "wake":
		return e.cmdWake(rest)
	case "send":
		return e.cmdSend(ctx, rest)
	case "thread":
		return e.cmdThread(rest)
	case "inbox":
		return e.cmdInbox(ctx, rest)
	case "fetch":
		return e.cmdFetch(ctx, rest)
	case "download":
		return e.cmdDownload(ctx, rest)
	case "status":
		return e.cmdStatus(rest)
	case "doctor":
		return e.cmdDoctor(ctx, rest)
	default:
		fmt.Fprintf(e.stderr, "string-can: unknown command %q\n", cmd)
		fmt.Fprint(e.stderr, usage)
		return 2
	}
}

func (e env) fail(err error) int {
	fmt.Fprintf(e.stderr, "string-can: %v\n", err)
	return 1
}

func (e env) cmdPubkey(args []string) int {
	if help, err := parseFlags("pubkey", args, nil); help {
		fmt.Fprint(e.stdout, commandHelp("pubkey"))
		return 0
	} else if err != nil {
		return e.fail(err)
	}
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	priv, err := dir.Identity()
	if err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.stdout, "string-can public key: %s\n", canon.PublicHexFromPrivate(priv))
	return 0
}

func (e env) cmdURL(args []string) int {
	if help, err := parseFlags("url", args, nil); help {
		fmt.Fprint(e.stdout, commandHelp("url"))
		return 0
	} else if err != nil {
		return e.fail(err)
	}
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	if u, err := dir.ListenURL(); err == nil && u != "" {
		if listenaddr.IsLoopbackURL(u) {
			fmt.Fprintln(e.stderr, "string-can: warning: listen URL is loopback; unreachable from other machines")
		}
		fmt.Fprintln(e.stdout, u)
		return 0
	}
	u := listenaddr.WouldBind(listenaddr.DefaultPort)
	if listenaddr.IsLoopbackURL(u) {
		fmt.Fprintln(e.stderr, "string-can: warning: no Tailscale IPv4; this URL is loopback")
	}
	fmt.Fprintln(e.stdout, u)
	return 0
}

func (e env) cmdName(args []string) int {
	fs, pos, help, err := flags("name", args, nil)
	if help {
		fmt.Fprint(e.stdout, commandHelp("name"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	_ = fs
	if len(pos) != 1 {
		return e.fail(fmt.Errorf("usage: string-can name <display-name>"))
	}
	name := pos[0]
	if name == "" {
		return e.fail(fmt.Errorf("display name is empty"))
	}
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	if err := dir.SetName(name); err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.stdout, "name: %s\n", name)
	return 0
}

func (e env) cmdServe(ctx context.Context, args []string) int {
	var name, host string
	var local, outboundOnly bool
	var wait time.Duration
	port := listenaddr.DefaultPort
	fs := newFlagSet("serve")
	fs.StringVar(&name, "name", "", "display name")
	fs.IntVar(&port, "port", listenaddr.DefaultPort, "listen port")
	fs.StringVar(&host, "host", "", "advertised hostname (MagicDNS)")
	fs.BoolVar(&local, "local", false, "bind to 127.0.0.1 for same-machine peers")
	fs.BoolVar(&outboundOnly, "outbound-only", false, "this agent cannot accept inbound push; bind locally and advertise pull-only")
	fs.DurationVar(&wait, "wait", 0, "how long to wait for a Tailscale address (0 = forever)")
	pos, help, err := parseWith(fs, args, map[string]bool{"name": true, "port": true, "host": true, "wait": true})
	if help {
		fmt.Fprint(e.stdout, commandHelp("serve"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if len(pos) != 0 {
		return e.fail(fmt.Errorf("usage: string-can serve [--name <display-name>] [--port <port>] [--host <name>] [--local] [--outbound-only] [--wait <duration>]"))
	}
	if port < 0 || port > 65535 {
		return e.fail(fmt.Errorf("invalid port"))
	}
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	webhookURL := os.Getenv("STRING_CAN_WEBHOOK_URL")
	webhookToken := os.Getenv("STRING_CAN_WEBHOOK_TOKEN")
	srv, err := server.New(server.Config{
		Home:          dir.Path,
		Name:          name,
		Port:          port,
		Host:          host,
		Local:         local,
		OutboundOnly:  outboundOnly,
		TailscaleWait: wait,
		WebhookURL:    webhookURL,
		WebhookToken:  webhookToken,
	})
	if err != nil {
		return e.fail(err)
	}
	defer srv.Close()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.BindContext(ctx); err != nil {
		return e.fail(err)
	}
	fmt.Fprintf(e.stdout, "string-can public key: %s\n", srv.PublicKey())
	fmt.Fprintf(e.stdout, "string-can listen: %s\n", srv.BaseURL())
	if srv.OutboundOnly() {
		fmt.Fprintln(e.stderr, "string-can: outbound-only; bound to 127.0.0.1. Peers cannot push here. Connect to their URL, then fetch or fetch --watch to receive.")
	} else if listenaddr.IsLoopbackURL(srv.BaseURL()) {
		fmt.Fprintln(e.stderr, "string-can: warning: bound to 127.0.0.1; unreachable from other machines. Pass --local only for two agents on one host.")
	}
	e.printWebhookStatus(ctx, webhookURL, webhookToken)
	if f, ok := e.stdout.(*os.File); ok {
		_ = f.Sync()
	}
	if err := srv.Serve(ctx); err != nil {
		return e.fail(err)
	}
	return 0
}

func (e env) printWebhookStatus(ctx context.Context, webhookURL, token string) {
	if webhookURL == "" {
		fmt.Fprintln(e.stderr, "string-can webhook: not set")
		return
	}
	if err := push.SelfTest(ctx, push.DefaultClient(), webhookURL, token); err != nil {
		fmt.Fprintf(e.stderr, "string-can webhook: configured, self-test failed: %v\n", err)
		return
	}
	fmt.Fprintln(e.stderr, "string-can webhook: configured, self-test ok")
}

func (e env) cmdRename(args []string) int {
	_, pos, help, err := flags("rename", args, nil)
	if help {
		fmt.Fprint(e.stdout, commandHelp("rename"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if len(pos) != 2 {
		return e.fail(fmt.Errorf("usage: string-can rename <public-key> <label>"))
	}
	key, err := canon.NormalizeKey(pos[0])
	if err != nil {
		return e.fail(err)
	}
	label := pos[1]
	if label == "" {
		return e.fail(fmt.Errorf("label is empty"))
	}
	st, _, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	c, err := st.RenameContact(key, label)
	if err != nil {
		return e.fail(err)
	}
	return e.writeJSON(viewContact(c))
}

func (e env) cmdContacts(args []string) int {
	if help, err := parseFlags("contacts", args, nil); help {
		fmt.Fprint(e.stdout, commandHelp("contacts"))
		return 0
	} else if err != nil {
		return e.fail(err)
	}
	st, _, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	list, err := st.ListContacts()
	if err != nil {
		return e.fail(err)
	}
	for _, c := range list {
		if err := e.writeJSON(viewContact(c)); err != 0 {
			return err
		}
	}
	return 0
}

func (e env) cmdSend(ctx context.Context, args []string) int {
	var auto bool
	var textFile string
	var paths stringList
	fs := newFlagSet("send")
	fs.BoolVar(&auto, "auto", false, "count this send as an auto-reply")
	fs.StringVar(&textFile, "text-file", "", "read the message from this file")
	fs.Var(&paths, "file", "attach a file; repeat for more than one")
	pos, help, err := parseWith(fs, args, map[string]bool{"text-file": true, "file": true})
	if help {
		fmt.Fprint(e.stdout, commandHelp("send"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	sawTextFile := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "text-file" {
			sawTextFile = true
		}
	})
	if sawTextFile && textFile == "" {
		return e.fail(fmt.Errorf("--text-file path is empty"))
	}
	keyText, text, err := e.sendText(pos, textFile, len(paths) > 0)
	if err != nil {
		return e.fail(err)
	}
	key, err := canon.NormalizeKey(keyText)
	if err != nil {
		return e.fail(err)
	}
	if text != "" {
		if err := canon.CheckText(text); err != nil {
			return e.fail(err)
		}
	} else if len(paths) == 0 {
		return e.fail(fmt.Errorf("text is empty"))
	}
	if len(paths) > canon.MaxFiles {
		return e.fail(fmt.Errorf("too many files"))
	}
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	priv, err := dir.Identity()
	if err != nil {
		return e.fail(err)
	}
	from := canon.PublicHexFromPrivate(priv)
	st, err := store.Open(dir.DBPath())
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	c, err := st.GetContact(key)
	if err != nil {
		return e.fail(err)
	}
	now := time.Now()
	// The reply cap runs before any row or blob is stored, same as text-only --auto.
	if auto {
		if err := autoGuard(st, key, now); err != nil {
			return e.fail(err)
		}
	}
	var attached []canon.File
	if len(paths) > 0 {
		max, err := files.MaxBytes()
		if err != nil {
			return e.fail(err)
		}
		blobs, err := files.Open(dir.Path)
		if err != nil {
			return e.fail(err)
		}
		seen := map[string]bool{}
		for _, path := range paths {
			f, err := blobs.Ingest(path, max)
			if err != nil {
				return e.fail(err)
			}
			if seen[f.Name] {
				return e.fail(fmt.Errorf("duplicate file name %s", f.Name))
			}
			seen[f.Name] = true
			attached = append(attached, f)
		}
	}
	ts := now.UnixMilli()
	body, err := canon.EncodeMessage(from, ts, text, attached)
	if err != nil {
		return e.fail(err)
	}
	sig := canon.Sign(priv, body)
	id, _, err := st.InsertMessage(store.Message{
		PeerPub:   key,
		FromPub:   from,
		Ts:        ts,
		Text:      text,
		Body:      string(body),
		Signature: sig,
		Delivered: false,
		Nudged:    false,
	})
	if err != nil {
		return e.fail(err)
	}
	if auto {
		if err := st.RecordAutoSend(key, text, ts); err != nil {
			return e.fail(err)
		}
	} else if err := st.ResetAuto(key); err != nil {
		return e.fail(err)
	}
	if c.PullOnly {
		if err := st.MarkDelivered(id); err != nil {
			return e.fail(err)
		}
		fmt.Fprintf(e.stdout, "sent %d (pull)\n", id)
		return 0
	}
	opt := push.DeliverOptions{}
	if len(attached) > 0 {
		blobs, err := files.Open(dir.Path)
		if err != nil {
			return e.fail(err)
		}
		opt.OpenBlob = blobs.Open
	}
	res := push.Deliver(ctx, push.PeerClient(), c.BaseURL, from, sig, body, opt)
	if res.Kind != push.KindDelivered && c.WakeURL != "" && wake.SendNeedsWake(res.Kind, res.Err) {
		if wake.Try(ctx, st, key, c.BaseURL, from, e.logf()) {
			opt.VerifyFirst = res.Kind == push.KindUnconfirmed
			res = push.Deliver(ctx, push.PeerClient(), c.BaseURL, from, sig, body, opt)
		}
	}
	switch res.Kind {
	case push.KindDelivered:
		if err := st.MarkDelivered(id); err != nil {
			return e.fail(err)
		}
		fmt.Fprintf(e.stdout, "sent %d\n", id)
		if res.FilesSkipped && res.Detail != "" {
			fmt.Fprintf(e.stderr, "string-can: %s\n", res.Detail)
		}
	case push.KindUnconfirmed:
		detail := res.Detail
		if detail == "" {
			detail = push.UnconfirmedDetail
		}
		next := time.Now().Add(push.RetryDelay(1)).UnixMilli()
		_ = st.RecordOutcome(id, store.StateUnconfirmed, detail, 1, next)
		fmt.Fprintf(e.stdout, "sent %d (unconfirmed — %s)\n", id, detail)
	default:
		detail := res.Detail
		if detail == "" && res.Err != nil {
			detail = res.Err.Error()
		}
		if detail == "" {
			detail = "delivery failed"
		}
		next := time.Now().Add(push.RetryDelay(1)).UnixMilli()
		_ = st.RecordOutcome(id, "", detail, 1, next)
		fmt.Fprintf(e.stdout, "sent %d pending\n", id)
		fmt.Fprintf(e.stderr, "string-can: %s\n", detail)
	}
	return 0
}

// sendText takes the message from exactly one of <text>, --text-file, or stdin.
// File and stdin bytes are the message as-is, including a trailing newline.
// With --file and no text on a terminal, the text is empty.
func (e env) sendText(pos []string, textFile string, hasFiles bool) (key, text string, err error) {
	if textFile != "" {
		if len(pos) > 1 {
			return "", "", fmt.Errorf("pass the message as <text> or --text-file, not both")
		}
		if len(pos) != 1 {
			return "", "", fmt.Errorf("usage: %s", sendUsageLine)
		}
		text, err = readTextFile(textFile)
		if err != nil {
			return "", "", err
		}
		return pos[0], text, nil
	}
	switch len(pos) {
	case 2:
		return pos[0], pos[1], nil
	case 1:
		if hasFiles {
			text, used, err := e.readOptionalStdin()
			if err != nil {
				return "", "", err
			}
			if !used {
				return pos[0], "", nil
			}
			return pos[0], text, nil
		}
		text, err = e.readSendStdin()
		if err != nil {
			return "", "", err
		}
		return pos[0], text, nil
	default:
		return "", "", fmt.Errorf("usage: %s", sendUsageLine)
	}
}

func (e env) readOptionalStdin() (text string, used bool, err error) {
	if e.stdin == nil {
		return "", false, nil
	}
	if f, ok := e.stdin.(*os.File); ok && isTerminal(f.Fd()) {
		return "", false, nil
	}
	text, err = readCapped(e.stdin)
	return text, true, err
}

func readTextFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--text-file path is empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readCapped(f)
}

// isTerminal reports whether a file is a terminal. Tests replace it.
var isTerminal = isatty.IsTerminal

func (e env) readSendStdin() (string, error) {
	if e.stdin == nil {
		return "", errSendTextMissing
	}
	if f, ok := e.stdin.(*os.File); ok && isTerminal(f.Fd()) {
		return "", errSendTextMissing
	}
	return readCapped(e.stdin)
}

func readCapped(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(canon.MaxTextBytes)+1))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (e env) cmdThread(args []string) int {
	var since int64
	fs := newFlagSet("thread")
	fs.Int64Var(&since, "since", 0, "exclusive local id")
	pos, help, err := parseWith(fs, args, map[string]bool{"since": true})
	if help {
		fmt.Fprint(e.stdout, commandHelp("thread"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if len(pos) != 1 {
		return e.fail(fmt.Errorf("usage: string-can thread <public-key> [--since <id>]"))
	}
	key, err := canon.NormalizeKey(pos[0])
	if err != nil {
		return e.fail(err)
	}
	st, _, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	msgs, err := st.VerifiedThread(key, since)
	if err != nil {
		return e.fail(err)
	}
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	for _, m := range msgs {
		if code := e.writeJSON(e.threadLine(dir, m)); code != 0 {
			return code
		}
	}
	return 0
}

type contactJSON struct {
	Label      string `json:"label"`
	PublicKey  string `json:"public_key"`
	BaseURL    string `json:"base_url"`
	StatedName string `json:"stated_name"`
	PullOnly   bool   `json:"pull_only"`
	WakeURL    string `json:"wake_url"`
	WakeToken  string `json:"wake_token"`
	WakeProxy  string `json:"wake_proxy"`
}

func viewContact(c store.Contact) contactJSON {
	token := ""
	if c.WakeTokenSet {
		token = wake.MaskedToken
	}
	return contactJSON{
		Label:      c.Label,
		PublicKey:  c.PublicKey,
		BaseURL:    c.BaseURL,
		StatedName: c.StatedName,
		PullOnly:   c.PullOnly,
		WakeURL:    wake.MaskURL(c.WakeURL),
		WakeToken:  token,
		WakeProxy:  wake.ProxyLabel(c.WakeProxy),
	}
}

func (e env) logf() func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(e.stderr, "string-can "+format+"\n", args...)
	}
}

func (e env) writeJSON(v any) int {
	enc := json.NewEncoder(e.stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return e.fail(err)
	}
	return 0
}

func openStore() (*store.Store, home.Dir, error) {
	dir, err := home.Resolve()
	if err != nil {
		return nil, dir, err
	}
	if err := dir.Ensure(); err != nil {
		return nil, dir, err
	}
	if _, err := dir.Identity(); err != nil {
		return nil, dir, err
	}
	st, err := store.Open(dir.DBPath())
	return st, dir, err
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseFlags(name string, args []string, valued map[string]bool) (help bool, err error) {
	fs := newFlagSet(name)
	pos, help, err := parseWith(fs, args, valued)
	if err != nil {
		return false, err
	}
	if help {
		return true, nil
	}
	if len(pos) != 0 {
		return false, fmt.Errorf("usage: string-can %s", name)
	}
	return false, nil
}

func flags(name string, args []string, valued map[string]bool) (*flag.FlagSet, []string, bool, error) {
	fs := newFlagSet(name)
	pos, help, err := parseWith(fs, args, valued)
	return fs, pos, help, err
}

func parseWith(fs *flag.FlagSet, args []string, valued map[string]bool) (pos []string, help bool, err error) {
	if valued == nil {
		valued = map[string]bool{}
	}
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if a == "-h" || a == "--help" {
			help = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			key, val, hasEq := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			if _, ok := valued[key]; !ok && fs.Lookup(key) == nil {
				return nil, false, fmt.Errorf("unknown flag --%s", key)
			}
			if hasEq {
				flagArgs = append(flagArgs, "--"+key, val)
				continue
			}
			if valued[key] || (fs.Lookup(key) != nil && fs.Lookup(key).Value.String() != "true" && fs.Lookup(key).Value.String() != "false") {
				if i+1 >= len(args) {
					return nil, false, fmt.Errorf("flag --%s requires a value", key)
				}
				i++
				flagArgs = append(flagArgs, "--"+key, args[i])
				continue
			}
			flagArgs = append(flagArgs, a)
			continue
		}
		if strings.HasPrefix(a, "-") {
			return nil, false, fmt.Errorf("unknown flag %s", a)
		}
		pos = append(pos, a)
	}
	if help {
		return pos, true, nil
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, false, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		pos = append(pos, extra...)
	}
	return pos, false, nil
}

func commandHelp(name string) string {
	switch name {
	case "pubkey":
		return "usage: string-can pubkey\n"
	case "url":
		return "usage: string-can url\n"
	case "name":
		return "usage: string-can name <display-name>\n"
	case "serve":
		return "usage: string-can serve [--name <display-name>] [--port <port>] [--host <name>] [--local] [--outbound-only] [--wait <duration>]\n"
	case "supervise":
		return "usage: string-can supervise [--name <display-name>] [--port <port>] [--host <name>] [--local] [--outbound-only] [--wait <duration>] [--every <duration>] [public-key...]\n"
	case "invite":
		return "usage: string-can invite\n"
	case "connect":
		return "usage: string-can connect <url> [--name <label>] [--local] [--pull-only] [--wake-url <url> [--wake-token-file <path> | --wake-token-env <NAME> | --wake-token <literal>]]\n       string-can connect <public-key> <base-url> [--name <label>] [--local] [--pull-only] [--wake-url <url> [--wake-token-file <path> | --wake-token-env <NAME> | --wake-token <literal>]]\n       string-can connect --pull-only <public-key> [--name <label>]\n"
	case "rename":
		return "usage: string-can rename <public-key> <label>\n"
	case "contacts":
		return "usage: string-can contacts\n"
	case "wake":
		return "usage: string-can wake set <public-key> <url> [--token-file <path> | --token-env <NAME> | --token <literal>] [--proxy <url|direct|env>]\n       string-can wake show <public-key>\n       string-can wake clear <public-key>\n"
	case "send":
		return "usage: " + sendUsageLine + "\n       Omit <text> and --text-file to read the message from stdin.\n       With --file and no text, the message text is empty.\n"
	case "thread":
		return "usage: string-can thread <public-key> [--since <id>]\n"
	case "inbox":
		return "usage: string-can inbox [--peek] [--watch] [--every <duration>] [public-key...]\n"
	case "fetch":
		return "usage: string-can fetch <public-key> [--watch] [--every <duration>]\n"
	case "download":
		return "usage: string-can download <public-key> <message-id> [--out <dir>]\n"
	case "status":
		return "usage: string-can status\n"
	case "doctor":
		return "usage: string-can doctor\n"
	default:
		return usage
	}
}

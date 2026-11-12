package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/store"
)

const defaultAutoCap = 6

const (
	replyOK        = "ok"
	replyHold      = "hold"
	reasonEcho     = "echo"
	reasonCap      = "auto-reply cap"
	reasonCooldown = "cooldown"
)

type inboxLine struct {
	ID     int64  `json:"id"`
	Peer   string `json:"peer"`
	From   string `json:"from"`
	Ts     int64  `json:"ts"`
	Text   string `json:"text"`
	Reply  string `json:"reply"`
	Reason string `json:"reason,omitempty"`
}

type inboxItem struct {
	line    inboxLine
	advance bool
	silent  bool
}

func autoCap() (int, error) {
	v := strings.TrimSpace(os.Getenv("STRING_CAN_AUTO_CAP"))
	if v == "" {
		return defaultAutoCap, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("STRING_CAN_AUTO_CAP must be a non-negative integer")
	}
	return n, nil
}

func autoCooldown() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv("STRING_CAN_AUTO_COOLDOWN"))
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("STRING_CAN_AUTO_COOLDOWN must be a non-negative duration")
	}
	return d, nil
}

// decideReply classifies one inbound line.
// Echo and the cap are final: the caller advances the watermark.
// Cooldown is temporary: the caller leaves the watermark so the line returns later.
func decideReply(text, autoText string, streak, cap int, autoAt int64, now time.Time, cool time.Duration) (reply, reason string, advance bool) {
	if autoText != "" && text == autoText {
		return replyHold, reasonEcho, true
	}
	if cap > 0 && streak >= cap {
		return replyHold, reasonCap, true
	}
	if cool > 0 && autoAt > 0 && now.Sub(time.UnixMilli(autoAt)) < cool {
		return replyHold, reasonCooldown, false
	}
	return replyOK, "", true
}

// autoGuard is the send --auto backstop. It refuses the send before a row is stored.
func autoGuard(st *store.Store, peer string, now time.Time) error {
	capN, err := autoCap()
	if err != nil {
		return err
	}
	cool, err := autoCooldown()
	if err != nil {
		return err
	}
	in, err := st.InboxState(peer)
	if err != nil {
		return err
	}
	if capN > 0 && in.AutoStreak >= capN {
		return fmt.Errorf("auto-reply cap reached for %s (%d); a send without --auto resets it", canon.ShortKey(peer), capN)
	}
	if cool > 0 && in.AutoAt > 0 && now.Sub(time.UnixMilli(in.AutoAt)) < cool {
		return fmt.Errorf("auto-reply cooldown for %s (%s)", canon.ShortKey(peer), cool)
	}
	return nil
}

func (e env) cmdInbox(ctx context.Context, args []string) int {
	var peek, watch bool
	every := 5 * time.Second
	fs := newFlagSet("inbox")
	fs.BoolVar(&peek, "peek", false, "print without advancing the watermark")
	fs.BoolVar(&watch, "watch", false, "print new arrivals until interrupted")
	fs.DurationVar(&every, "every", 5*time.Second, "watch poll interval")
	pos, help, err := parseWith(fs, args, map[string]bool{"every": true})
	if help {
		fmt.Fprint(e.stdout, commandHelp("inbox"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if peek && watch {
		return e.fail(fmt.Errorf("--watch cannot be combined with --peek"))
	}
	if every <= 0 {
		every = 5 * time.Second
	}
	var keys []string
	for _, p := range pos {
		key, err := canon.NormalizeKey(p)
		if err != nil {
			return e.fail(err)
		}
		keys = append(keys, key)
	}
	st, dir, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	for _, key := range keys {
		if _, err := st.GetContact(key); err != nil {
			return e.fail(err)
		}
	}
	created, err := dir.EnsureReplyRules()
	if err != nil {
		return e.fail(err)
	}
	if created {
		fmt.Fprintf(e.stderr, "string-can: wrote reply rules %s\n", dir.ReplyRulesPath())
	}
	if !peek {
		lk, lerr := dir.InboxLock()
		if lerr != nil {
			return e.fail(lerr)
		}
		defer lk.Unlock()
	}
	if watch {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	var suppress map[int64]bool
	if watch {
		suppress = map[int64]bool{}
	}
	for {
		n, code := e.inboxOnce(st, keys, !peek, time.Now(), suppress)
		if code != 0 && !watch {
			return code
		}
		if !watch {
			return 0
		}
		if n > 0 {
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

func (e env) inboxOnce(st *store.Store, keys []string, advance bool, now time.Time, suppress map[int64]bool) (int, int) {
	capN, err := autoCap()
	if err != nil {
		return 0, e.fail(err)
	}
	cool, err := autoCooldown()
	if err != nil {
		return 0, e.fail(err)
	}
	peers := keys
	if len(peers) == 0 {
		list, err := st.ListContacts()
		if err != nil {
			return 0, e.fail(err)
		}
		for _, c := range list {
			peers = append(peers, c.PublicKey)
		}
	}
	type cand struct {
		m  store.Message
		in store.Inbox
	}
	var cands []cand
	for _, peer := range peers {
		in, err := st.InboxState(peer)
		if err != nil {
			return 0, e.fail(err)
		}
		msgs, err := st.InboundSince(peer, in.Watermark)
		if err != nil {
			return 0, e.fail(err)
		}
		for _, m := range msgs {
			cands = append(cands, cand{m: m, in: in})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].m.ID < cands[j].m.ID })

	var queued []inboxItem
	for _, c := range cands {
		reply, reason, adv := decideReply(c.m.Text, c.in.AutoText, c.in.AutoStreak, capN, c.in.AutoAt, now, cool)
		if !advance {
			adv = false
		}
		item := inboxItem{
			line: inboxLine{
				ID:     c.m.ID,
				Peer:   c.m.PeerPub,
				From:   c.m.FromPub,
				Ts:     c.m.Ts,
				Text:   c.m.Text,
				Reply:  reply,
				Reason: reason,
			},
			advance: adv,
		}
		if reason == reasonCooldown && suppress != nil {
			if suppress[c.m.ID] {
				item.silent = true
			} else {
				suppress[c.m.ID] = true
			}
		}
		queued = append(queued, item)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	n := 0
	for _, item := range queued {
		if item.silent {
			continue
		}
		if err := enc.Encode(item.line); err != nil {
			return 0, e.fail(err)
		}
		n++
	}
	if buf.Len() > 0 {
		if _, err := e.stdout.Write(buf.Bytes()); err != nil {
			return 0, e.fail(err)
		}
	}
	if !advance {
		return n, 0
	}
	// A cooldown line stays above the watermark. Do not advance past it,
	// or a later final hold in the same batch would drop it.
	blocked := map[string]bool{}
	mark := map[string]int64{}
	for _, item := range queued {
		if !item.advance {
			blocked[item.line.Peer] = true
			continue
		}
		if blocked[item.line.Peer] {
			continue
		}
		if item.line.ID > mark[item.line.Peer] {
			mark[item.line.Peer] = item.line.ID
		}
	}
	for peer, id := range mark {
		if err := st.SetWatermark(peer, id); err != nil {
			return n, e.fail(err)
		}
	}
	return n, 0
}

func (e env) reportReply(dir home.Dir, ok *bool) {
	capN, err := autoCap()
	if err != nil {
		fmt.Fprintf(e.stdout, "reply: %v\n", err)
		*ok = false
		return
	}
	cool, err := autoCooldown()
	if err != nil {
		fmt.Fprintf(e.stdout, "reply: %v\n", err)
		*ok = false
		return
	}
	coolLabel := "off"
	if cool > 0 {
		coolLabel = cool.String()
	}
	rules := "rules not written yet"
	if _, err := os.Stat(dir.ReplyRulesPath()); err == nil {
		rules = "rules " + dir.ReplyRulesPath()
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(e.stdout, "reply: rules %v\n", err)
		*ok = false
		return
	}
	fmt.Fprintf(e.stdout, "reply: auto cap %d, cooldown %s, %s\n", capN, coolLabel, rules)
}

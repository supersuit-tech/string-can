package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/store"
)

// superviseScan is how often supervise looks for contacts to fetch.
// Tests shorten it.
var superviseScan = 2 * time.Second

// child is one supervised process.
type child interface {
	Wait() error
	Stop()
}

// startChild starts this binary with args (the string-can subcommand, not
// including the executable path). Tests replace it.
var startChild = startExecChild

func startExecChild(stdout, stderr io.Writer, args ...string) (child, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, rerr := filepath.EvalSymlinks(bin); rerr == nil {
		bin = resolved
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execChild{cmd: cmd, done: make(chan struct{})}, nil
}

type execChild struct {
	cmd      *exec.Cmd
	done     chan struct{}
	stopOnce sync.Once
	doneOnce sync.Once
}

func (c *execChild) Wait() error {
	err := c.cmd.Wait()
	c.doneOnce.Do(func() { close(c.done) })
	return err
}

func (c *execChild) Stop() {
	c.stopOnce.Do(func() {
		if c.cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		go func() {
			select {
			case <-c.done:
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			}
		}()
	})
}

type superviseOpts struct {
	serveArgs []string
	explicit  []string
	auto      bool
	every     time.Duration
	scan      time.Duration
	backoff   func(crashes int, ran time.Duration) time.Duration
	contacts  func() ([]string, error)
	stdout    io.Writer
	stderr    io.Writer
}

func (e env) cmdSupervise(ctx context.Context, args []string) int {
	var name, host string
	var local, outboundOnly bool
	var wait, every time.Duration
	port := 0
	fs := newFlagSet("supervise")
	fs.StringVar(&name, "name", "", "display name")
	fs.IntVar(&port, "port", 0, "listen port")
	fs.StringVar(&host, "host", "", "advertised hostname (MagicDNS)")
	fs.BoolVar(&local, "local", false, "bind to 127.0.0.1 for same-machine peers")
	fs.BoolVar(&outboundOnly, "outbound-only", false, "this agent cannot accept inbound push")
	fs.DurationVar(&wait, "wait", 0, "how long serve waits for a Tailscale address (0 = forever)")
	fs.DurationVar(&every, "every", 5*time.Second, "fetch --watch poll interval")
	pos, help, err := parseWith(fs, args, map[string]bool{
		"name": true, "port": true, "host": true, "wait": true, "every": true,
	})
	if help {
		fmt.Fprint(e.stdout, commandHelp("supervise"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if every <= 0 {
		every = 5 * time.Second
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	keys, err := normalizeKeys(pos)
	if err != nil {
		return e.fail(err)
	}
	if portSet := set["port"]; portSet && (port < 0 || port > 65535) {
		return e.fail(fmt.Errorf("invalid port"))
	}
	dir, err := home.Resolve()
	if err != nil {
		return e.fail(err)
	}
	// A home that already served outbound-only stays that way, so a restarted
	// supervisor keeps the wake loop and the fetch watchers.
	if !outboundOnly && dir.OutboundOnly() {
		outboundOnly = true
	}
	serveArgs := []string{"serve"}
	if set["name"] && name != "" {
		serveArgs = append(serveArgs, "--name", name)
	}
	if set["port"] {
		serveArgs = append(serveArgs, "--port", strconv.Itoa(port))
	}
	if set["host"] && host != "" {
		serveArgs = append(serveArgs, "--host", host)
	}
	if local {
		serveArgs = append(serveArgs, "--local")
	}
	if outboundOnly {
		serveArgs = append(serveArgs, "--outbound-only")
	}
	if set["wait"] {
		serveArgs = append(serveArgs, "--wait", wait.String())
	}
	auto := outboundOnly && len(keys) == 0
	if auto || len(keys) > 0 {
		fmt.Fprintln(e.stderr, "string-can: supervise: restarting serve and fetch --watch if either exits")
	} else {
		fmt.Fprintln(e.stderr, "string-can: supervise: restarting serve if it exits")
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	runSupervisor(ctx, superviseOpts{
		serveArgs: serveArgs,
		explicit:  keys,
		auto:      auto,
		every:     every,
		scan:      superviseScan,
		backoff:   defaultSuperviseBackoff,
		contacts: func() ([]string, error) {
			if len(keys) > 0 {
				return watchKeys(dir, keys)
			}
			return watchKeys(dir, nil)
		},
		stdout: e.stdout,
		stderr: e.stderr,
	})
	return 0
}

func normalizeKeys(pos []string) ([]string, error) {
	var keys []string
	seen := map[string]bool{}
	for _, p := range pos {
		key, err := canon.NormalizeKey(p)
		if err != nil {
			return nil, err
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys, nil
}

// watchKeys returns peers supervise should poll. explicit limits the set;
// an empty explicit lists every contact that has a URL.
func watchKeys(dir home.Dir, explicit []string) ([]string, error) {
	if _, err := os.Stat(dir.DBPath()); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	st, err := store.Open(dir.DBPath())
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if len(explicit) > 0 {
		var out []string
		seen := map[string]bool{}
		for _, key := range explicit {
			if seen[key] {
				continue
			}
			c, err := st.GetContact(key)
			if err != nil || c.BaseURL == "" {
				continue
			}
			seen[key] = true
			out = append(out, key)
		}
		return out, nil
	}
	list, err := st.ListContacts()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range list {
		if c.BaseURL == "" {
			continue
		}
		out = append(out, c.PublicKey)
	}
	return out, nil
}

func defaultSuperviseBackoff(crashes int, ran time.Duration) time.Duration {
	if ran >= 30*time.Second {
		return time.Second
	}
	if crashes < 1 {
		crashes = 1
	}
	if crashes > 5 {
		crashes = 5
	}
	return time.Second << (crashes - 1)
}

func runSupervisor(ctx context.Context, opt superviseOpts) {
	if opt.stdout == nil {
		opt.stdout = io.Discard
	}
	if opt.stderr == nil {
		opt.stderr = io.Discard
	}
	if opt.scan <= 0 {
		opt.scan = superviseScan
	}
	if opt.backoff == nil {
		opt.backoff = defaultSuperviseBackoff
	}
	if opt.every <= 0 {
		opt.every = 5 * time.Second
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		keepAlive(ctx, "serve", opt.serveArgs, opt.stdout, opt.stderr, opt.backoff)
	}()
	if opt.auto || len(opt.explicit) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manageFetch(ctx, opt)
		}()
	}
	wg.Wait()
}

func keepAlive(ctx context.Context, label string, args []string, stdout, stderr io.Writer, backoff func(int, time.Duration) time.Duration) {
	crashes := 0
	announced := false
	for ctx.Err() == nil {
		if !announced {
			fmt.Fprintf(stderr, "string-can: starting %s\n", label)
			announced = true
		}
		proc, err := startChild(stdout, stderr, args...)
		ran := time.Duration(0)
		if err == nil {
			start := time.Now()
			err = waitOrStop(ctx, proc)
			ran = time.Since(start)
		}
		if ctx.Err() != nil {
			return
		}
		if ran >= 30*time.Second {
			crashes = 0
		}
		crashes++
		delay := backoff(crashes, ran)
		fmt.Fprintf(stderr, "string-can: %s exited (%s); restarting in %s\n", label, exitText(err), delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func waitOrStop(ctx context.Context, proc child) error {
	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Wait() }()
	select {
	case <-ctx.Done():
		proc.Stop()
		return <-waitCh
	case err := <-waitCh:
		return err
	}
}

func exitText(err error) string {
	if err == nil {
		return "exit 0"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.Error()
	}
	return err.Error()
}

type fetchSlot struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func manageFetch(ctx context.Context, opt superviseOpts) {
	if opt.contacts == nil {
		return
	}
	var (
		mu      sync.Mutex
		running = map[string]*fetchSlot{}
	)
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range running {
			s.cancel()
		}
		for _, s := range running {
			<-s.done
		}
	}()
	ticker := time.NewTicker(opt.scan)
	defer ticker.Stop()
	warned := false
	for {
		if ctx.Err() != nil {
			return
		}
		keys, err := opt.contacts()
		if err != nil {
			fmt.Fprintf(opt.stderr, "string-can: contacts: %v\n", err)
		} else {
			if len(opt.explicit) > 0 && len(keys) == 0 && !warned {
				fmt.Fprintln(opt.stderr, "string-can: fetch waiting until the peer is a contact with a URL")
				warned = true
			}
			reconcileFetch(ctx, opt, &mu, running, keys)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reconcileFetch(ctx context.Context, opt superviseOpts, mu *sync.Mutex, running map[string]*fetchSlot, keys []string) {
	if ctx.Err() != nil {
		return
	}
	want := map[string]struct{}{}
	for _, key := range keys {
		want[key] = struct{}{}
	}
	mu.Lock()
	defer mu.Unlock()
	for key := range want {
		if s, ok := running[key]; ok {
			select {
			case <-s.done:
			default:
				continue
			}
		}
		fctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		running[key] = &fetchSlot{cancel: cancel, done: done}
		label := "fetch " + canon.ShortKey(key)
		args := []string{"fetch", "--watch", "--every", opt.every.String(), key}
		go func(label string, args []string, fctx context.Context, done chan struct{}) {
			defer close(done)
			keepAlive(fctx, label, args, opt.stdout, opt.stderr, opt.backoff)
		}(label, args, fctx, done)
	}
	for key, s := range running {
		if _, ok := want[key]; ok {
			continue
		}
		s.cancel()
		select {
		case <-s.done:
			delete(running, key)
		default:
		}
	}
}

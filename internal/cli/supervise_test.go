package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/supersuit-tech/string-can/internal/home"
	"github.com/supersuit-tech/string-can/internal/store"
)

func TestSuperviseHelp(t *testing.T) {
	var stdout bytes.Buffer
	code := RunContext(context.Background(), []string{"supervise", "--help"}, &stdout, io.Discard)
	if code != 0 || !strings.Contains(stdout.String(), "supervise") || !strings.Contains(stdout.String(), "--outbound-only") {
		t.Fatalf("exit %d usage %q", code, stdout.String())
	}
}

func TestSuperviseRejectsBadKey(t *testing.T) {
	var stderr bytes.Buffer
	code := RunContext(context.Background(), []string{"supervise", "not-a-key"}, io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "public key") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestSuperviseBackoff(t *testing.T) {
	if got := defaultSuperviseBackoff(1, 0); got != time.Second {
		t.Fatalf("first %s", got)
	}
	if got := defaultSuperviseBackoff(2, time.Second); got != 2*time.Second {
		t.Fatalf("second %s", got)
	}
	if got := defaultSuperviseBackoff(8, time.Second); got != 16*time.Second {
		t.Fatalf("cap %s", got)
	}
	if got := defaultSuperviseBackoff(4, 30*time.Second); got != time.Second {
		t.Fatalf("after healthy run %s", got)
	}
}

func TestSuperviseRestartsExitedChild(t *testing.T) {
	orig := startChild
	var mu sync.Mutex
	var n int
	startChild = func(stdout, stderr io.Writer, args ...string) (child, error) {
		mu.Lock()
		n++
		mu.Unlock()
		exit := make(chan struct{})
		close(exit)
		return &fakeChild{stop: make(chan struct{}), exit: exit}, nil
	}
	t.Cleanup(func() { startChild = orig })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSupervisor(ctx, superviseOpts{
			serveArgs: []string{"serve", "--local"},
			scan:      time.Hour,
			backoff:   func(int, time.Duration) time.Duration { return 5 * time.Millisecond },
			stdout:    io.Discard,
			stderr:    io.Discard,
		})
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := n
		mu.Unlock()
		if got >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	mu.Lock()
	got := n
	mu.Unlock()
	if got < 3 {
		t.Fatalf("restarts = %d", got)
	}
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n != got {
		t.Fatalf("restarted after stop: %d -> %d", got, n)
	}
}

func TestSuperviseStartsAndStopsFetch(t *testing.T) {
	key := strings.Repeat("ab", 32)
	var phase int
	var mu sync.Mutex
	var fetch *fakeChild
	orig := startChild
	startChild = func(stdout, stderr io.Writer, args ...string) (child, error) {
		c := &fakeChild{args: append([]string(nil), args...), stop: make(chan struct{}), stopped: make(chan struct{})}
		if len(args) > 0 && args[0] == "fetch" {
			mu.Lock()
			fetch = c
			mu.Unlock()
		}
		return c, nil
	}
	t.Cleanup(func() { startChild = orig })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runSupervisor(ctx, superviseOpts{
			serveArgs: []string{"serve"},
			auto:      true,
			every:     10 * time.Millisecond,
			scan:      10 * time.Millisecond,
			backoff:   func(int, time.Duration) time.Duration { return time.Hour },
			contacts: func() ([]string, error) {
				mu.Lock()
				defer mu.Unlock()
				if phase == 0 {
					return nil, nil
				}
				return []string{key}, nil
			},
			stdout: io.Discard,
			stderr: io.Discard,
		})
		close(done)
	}()

	mu.Lock()
	phase = 1
	mu.Unlock()
	var started *fakeChild
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		started = fetch
		mu.Unlock()
		if started != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if started == nil {
		t.Fatal("fetch was not started")
	}
	joined := strings.Join(started.args, " ")
	if !strings.Contains(joined, "--watch") || !strings.Contains(joined, key) {
		t.Fatalf("fetch args %q", joined)
	}

	mu.Lock()
	phase = 0
	mu.Unlock()
	select {
	case <-started.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("fetch was not stopped after the contact left")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestSuperviseInboundDoesNotFetch(t *testing.T) {
	orig := startChild
	var mu sync.Mutex
	var sawFetch bool
	startChild = func(stdout, stderr io.Writer, args ...string) (child, error) {
		if len(args) > 0 && args[0] == "fetch" {
			mu.Lock()
			sawFetch = true
			mu.Unlock()
		}
		return &fakeChild{stop: make(chan struct{}), stopped: make(chan struct{})}, nil
	}
	t.Cleanup(func() { startChild = orig })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSupervisor(ctx, superviseOpts{
			serveArgs: []string{"serve"},
			scan:      5 * time.Millisecond,
			contacts: func() ([]string, error) {
				return []string{strings.Repeat("cd", 32)}, nil
			},
			stdout: io.Discard,
			stderr: io.Discard,
		})
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if sawFetch {
		t.Fatal("inbound supervise started fetch without --outbound-only or keys")
	}
}

func TestSuperviseForwardsFlagsAndStickyOutbound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STRING_CAN_HOME", dir)
	if err := (home.Dir{Path: dir}).SetOutboundOnly(true); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "string-can.db"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 32)
	if err := st.UpsertContact(store.Contact{PublicKey: key, Label: "peer", BaseURL: "http://127.0.0.1:9"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	origScan := superviseScan
	superviseScan = 10 * time.Millisecond
	t.Cleanup(func() { superviseScan = origScan })

	orig := startChild
	var mu sync.Mutex
	var got [][]string
	startChild = func(stdout, stderr io.Writer, args ...string) (child, error) {
		mu.Lock()
		got = append(got, append([]string(nil), args...))
		mu.Unlock()
		return &fakeChild{stop: make(chan struct{}), stopped: make(chan struct{})}, nil
	}
	t.Cleanup(func() { startChild = orig })

	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- RunContext(ctx, []string{"supervise", "--name", "muse", "--port", "0", "--every", "20ms"}, io.Discard, io.Discard)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var serveArgs, fetchArgs []string
	for time.Now().Before(deadline) && (serveArgs == nil || fetchArgs == nil) {
		mu.Lock()
		for _, args := range got {
			if len(args) == 0 {
				continue
			}
			switch args[0] {
			case "serve":
				serveArgs = args
			case "fetch":
				fetchArgs = args
			}
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervise did not exit")
	}
	if strings.Join(serveArgs, " ") != "serve --name muse --port 0 --outbound-only" {
		t.Fatalf("serve args %q", serveArgs)
	}
	joined := strings.Join(fetchArgs, " ")
	if !strings.Contains(joined, "--watch") || !strings.Contains(joined, "--every") || !strings.Contains(joined, key) {
		t.Fatalf("fetch args %q", joined)
	}
}

func TestWatchKeys(t *testing.T) {
	dir := home.Dir{Path: t.TempDir()}
	st, err := store.Open(dir.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	withURL := strings.Repeat("ab", 32)
	noURL := strings.Repeat("cd", 32)
	if err := st.UpsertContact(store.Contact{PublicKey: withURL, Label: "up", BaseURL: "http://127.0.0.1:9"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertContact(store.Contact{PublicKey: noURL, Label: "down", PullOnly: true}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	got, err := watchKeys(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != withURL {
		t.Fatalf("auto %v", got)
	}
	got, err = watchKeys(dir, []string{withURL, noURL, strings.Repeat("ef", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != withURL {
		t.Fatalf("explicit %v", got)
	}
	empty := home.Dir{Path: t.TempDir()}
	got, err = watchKeys(empty, nil)
	if err != nil || got != nil {
		t.Fatalf("missing db %v %v", got, err)
	}
}

func TestFetchWatchAlreadyWatching(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STRING_CAN_HOME", dir)
	var stdout, stderr bytes.Buffer
	if code := RunContext(context.Background(), []string{"pubkey"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pubkey %d %s", code, stderr.String())
	}
	st, err := store.Open(home.Dir{Path: dir}.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 32)
	if err := st.UpsertContact(store.Contact{PublicKey: key, Label: "peer", BaseURL: "http://127.0.0.1:9"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	lk, err := (home.Dir{Path: dir}).FetchLock(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lk.Unlock)
	stderr.Reset()
	code := RunContext(context.Background(), []string{"fetch", "--watch", key}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "already watching") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestSuperviseFetchSurvivesServeCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group supervise is unix")
	}
	bin := buildStringCan(t)

	peerHome := t.TempDir()
	peer := exec.Command(bin, "serve", "--local", "--port", "0", "--name", "beta")
	peer.Env = envWithHome(peerHome)
	var peerOut safeBuf
	peer.Stdout = &peerOut
	peer.Stderr = &peerOut
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProc(peer) })
	peerURL := lineAfter(waitLine(t, &peerOut, "string-can listen: ", 10*time.Second), "string-can listen: ")

	museHome := t.TempDir()
	sup := exec.Command(bin, "supervise", "--outbound-only", "--name", "muse", "--port", "0", "--every", "50ms")
	sup.Env = envWithHome(museHome)
	var supOut, supErr safeBuf
	sup.Stdout = &supOut
	sup.Stderr = &supErr
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProc(sup) })
	out := waitLine(t, &supOut, "string-can public key: ", 10*time.Second)
	museKey := lineAfter(out, "string-can public key: ")

	conn := exec.Command(bin, "connect", "--local", peerURL)
	conn.Env = envWithHome(museHome)
	if cOut, err := conn.CombinedOutput(); err != nil {
		t.Fatalf("connect: %v\n%s", err, cOut)
	}

	send := exec.Command(bin, "send", museKey, "from-peer")
	send.Env = envWithHome(peerHome)
	sOut, err := send.CombinedOutput()
	if err != nil {
		t.Fatalf("send: %v\n%s", err, sOut)
	}
	if !strings.Contains(string(sOut), "(pull)") {
		t.Fatalf("expected pull send, got %s", sOut)
	}
	waitLine(t, &supOut, `"text":"from-peer"`, 10*time.Second)

	keysBefore := strings.Count(supOut.String(), "string-can public key:")
	servePID := childPID(t, sup.Process.Pid, " serve ")
	if err := syscall.Kill(servePID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(supOut.String(), "string-can public key:") > keysBefore &&
			strings.Contains(supErr.String(), "serve exited") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Count(supOut.String(), "string-can public key:") <= keysBefore {
		t.Fatalf("serve did not restart\nstderr:\n%s\nstdout:\n%s", supErr.String(), supOut.String())
	}
	waitDial(t, museHome, 3*time.Second)

	send2 := exec.Command(bin, "send", museKey, "after-restart")
	send2.Env = envWithHome(peerHome)
	if sOut, err = send2.CombinedOutput(); err != nil {
		t.Fatalf("send2: %v\n%s", err, sOut)
	}
	waitLine(t, &supOut, `"text":"after-restart"`, 10*time.Second)

	doc := exec.Command(bin, "doctor")
	doc.Env = envWithHome(museHome)
	dOut, err := doc.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, dOut)
	}
	if !strings.Contains(string(dOut), "server: running") || !strings.Contains(string(dOut), "fetch: beta watching") {
		t.Fatalf("doctor\n%s", dOut)
	}
}

func TestDoctorOutboundNoPeersYet(t *testing.T) {
	// Covered with the server harness in server tests; this checks the
	// no-peer outbound case, which must stay successful before connect.
	dir := t.TempDir()
	t.Setenv("STRING_CAN_HOME", dir)
	if err := (home.Dir{Path: dir}).SetOutboundOnly(true); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := RunContext(context.Background(), []string{"pubkey"}, &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	stdout.Reset()
	code := RunContext(context.Background(), []string{"doctor"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("doctor succeeded with no server")
	}
	if !strings.Contains(stdout.String(), "server: not running") || !strings.Contains(stdout.String(), "fetch: no peers yet") {
		t.Fatalf("doctor\n%s", stdout.String())
	}
}

type fakeChild struct {
	args    []string
	stop    chan struct{}
	exit    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func (f *fakeChild) Wait() error {
	if f.exit == nil {
		<-f.stop
		return context.Canceled
	}
	select {
	case <-f.stop:
		return context.Canceled
	case <-f.exit:
		return errorsExit
	}
}

func (f *fakeChild) Stop() {
	f.once.Do(func() {
		close(f.stop)
		if f.stopped != nil {
			close(f.stopped)
		}
	})
}

var errorsExit = errString("exit status 1")

type errString string

func (e errString) Error() string { return string(e) }

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func buildStringCan(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	bin := filepath.Join(t.TempDir(), "string-can")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func envWithHome(dir string) []string {
	var out []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "STRING_CAN_HOME=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "STRING_CAN_HOME="+dir)
}

func waitLine(t *testing.T, buf *safeBuf, sub string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s := buf.String()
		if strings.Contains(s, sub) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q\noutput:\n%s", sub, buf.String())
	return ""
}

func lineAfter(s, prefix string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest)
}

func stopProc(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func childPID(t *testing.T, ppid int, arg string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-ww", "-o", "pid=", "-o", "args=", "--ppid", strconv.Itoa(ppid)).Output()
		last = string(out)
		if err == nil {
			for _, line := range strings.Split(last, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || !strings.Contains(line, arg) {
					continue
				}
				fields := strings.Fields(line)
				pid, conv := strconv.Atoi(fields[0])
				if conv == nil {
					return pid
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no child matching %q of %d\n%s", arg, ppid, last)
	return 0
}

func readListen(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "listen.url"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func waitDial(t *testing.T, dir string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		last = readListen(t, dir)
		if dialURL(last) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listen %q is not accepting", last)
}

func dialURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", u.Host, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

package push

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryDelaySteps(t *testing.T) {
	if RetryDelay(1) != 5*time.Second || RetryDelay(0) != 5*time.Second {
		t.Fatalf("first %s", RetryDelay(1))
	}
	if RetryDelay(2) != 15*time.Second || RetryDelay(3) != 30*time.Second {
		t.Fatalf("mid %s %s", RetryDelay(2), RetryDelay(3))
	}
	if RetryDelay(4) != 60*time.Second || RetryDelay(9) != 60*time.Second {
		t.Fatalf("cap %s", RetryDelay(9))
	}
}

func TestClientTimeoutIsAmbiguous(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	err = Message(context.Background(), client, "http://"+ln.Addr().String(), "ab", "sig", []byte(`{"v":1}`))
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !IsAmbiguous(err) {
		t.Fatalf("timeout should be ambiguous: %v", err)
	}
	if IsAmbiguous(&HTTPError{Status: http.StatusForbidden, Msg: ForbiddenContact}) {
		t.Fatal("HTTP error is definite")
	}
}

func TestConnectionRefusedIsDefinite(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	err = Message(context.Background(), PeerClient(), "http://"+addr, "ab", "sig", []byte(`{"v":1}`))
	if err == nil || IsAmbiguous(err) {
		t.Fatalf("expected a definite dial failure, got %v", err)
	}
	res := Deliver(context.Background(), &http.Client{Timeout: time.Second}, "http://"+addr, "ab", "sig", []byte(`{"v":1}`), DeliverOptions{})
	if res.Kind != KindPending {
		t.Fatalf("kind %v detail %q err %v", res.Kind, res.Detail, res.Err)
	}
	if res.Detail == "" || res.Detail == UnconfirmedDetail {
		t.Fatalf("detail %q", res.Detail)
	}
}

func TestDeliverConfirmsStoredMessageAfterTimeout(t *testing.T) {
	const sig = "sig-stored"
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			got = append(got, r.Header.Get("X-String-Can-Signature"))
			mu.Unlock()
			<-r.Context().Done()
			return
		}
		mu.Lock()
		defer mu.Unlock()
		msgs := make([]RemoteMessage, len(got))
		for i, s := range got {
			msgs[i] = RemoteMessage{ID: int64(i + 1), From: "peer", Ts: 1, Text: "t", Signature: s}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Messages []RemoteMessage `json:"messages"`
		}{Messages: msgs})
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 200 * time.Millisecond}
	res := Deliver(context.Background(), client, srv.URL, "from", sig, []byte(`{"v":1}`), DeliverOptions{})
	if res.Kind != KindDelivered || !res.Found {
		t.Fatalf("kind %v found %v detail %q err %v", res.Kind, res.Found, res.Detail, res.Err)
	}
}

func TestDeliverUnconfirmedWhenThreadAlsoTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	res := Deliver(context.Background(), client, srv.URL, "from", "sig", []byte(`{"v":1}`), DeliverOptions{})
	if res.Kind != KindUnconfirmed {
		t.Fatalf("kind %v detail %q err %v", res.Kind, res.Detail, res.Err)
	}
	if res.Detail != UnconfirmedDetail {
		t.Fatalf("detail %q", res.Detail)
	}
}

func TestDeliverPendingWhenThreadLacksSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[]}`)
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	res := Deliver(context.Background(), client, srv.URL, "from", "sig", []byte(`{"v":1}`), DeliverOptions{})
	if res.Kind != KindPending || !res.Absent || res.Detail != AbsentDetail {
		t.Fatalf("kind %v absent %v detail %q", res.Kind, res.Absent, res.Detail)
	}
}

func TestDeliverVerifyFirstSkipsPostWhenPresent(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[{"id":1,"from":"a","ts":1,"text":"t","signature":"already"}]}`)
	}))
	t.Cleanup(srv.Close)
	res := Deliver(context.Background(), srv.Client(), srv.URL, "from", "already", []byte(`{"v":1}`), DeliverOptions{VerifyFirst: true})
	if res.Kind != KindDelivered || !res.Found {
		t.Fatalf("kind %v found %v err %v", res.Kind, res.Found, res.Err)
	}
	if posts.Load() != 0 {
		t.Fatalf("posted %d times", posts.Load())
	}
}

func TestDeliverForbiddenStaysPending(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
		http.Error(w, ForbiddenContact, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	res := Deliver(context.Background(), srv.Client(), srv.URL, "from", "sig", []byte(`{"v":1}`), DeliverOptions{})
	if res.Kind != KindPending || res.Detail != ForbiddenContact {
		t.Fatalf("kind %v detail %q", res.Kind, res.Detail)
	}
	if gets.Load() != 0 {
		t.Fatalf("thread was fetched on a definite HTTP error: %d", gets.Load())
	}
}

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Official test vector from the Twitter "Creating a signature" docs.
func TestOAuthSignatureVector(t *testing.T) {
	c := &credentials{
		consumerKey:    "xvz1evFS4wEEPTGEFPHBog",
		consumerSecret: "kAcSOqF21Fu85e7zjz7ZN2U4ZRhfV3WpwPAoE3Z7kBw",
		token:          "370773112-GmHxMAgYyLbNEtIKZeRNFsMKPR9EyMZeS9weJAEb",
		tokenSecret:    "LswwdoUaIvS8ltyTt5jkRh4J50vUPVVHtR2YPi5kE",
		now:            func() time.Time { return time.Unix(1318622958, 0) },
		nonce:          func() string { return "kYjzVBB8Y0ZFabxSWbWovY3uYSQ2pTgmZeNu2VS4cg" },
	}
	h := c.header("POST", "https://api.twitter.com/1.1/statuses/update.json?include_entities=true",
		map[string]string{"status": "Hello Ladies + Gentlemen, a signed OAuth request!"})
	want := `oauth_signature="hCtSmYh%2BiHYCEqBWrE7C7hYmtUk%3D"`
	if !strings.Contains(h, want) {
		t.Fatalf("wrong signature:\n%s\nwant: %s", h, want)
	}
}

func writeArchive(t *testing.T, nTweets, nLikes int) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	os.MkdirAll(data, 0o755)
	os.WriteFile(filepath.Join(data, "account.js"),
		[]byte(`window.YTD.account.part0 = [{"account":{"accountId":"42"}}]`), 0o644)

	var a, b []string
	for i := 1; i <= nTweets; i++ {
		s := `{"tweet":{"id_str":"` + strconv.Itoa(i) + `"}}`
		if i%2 == 0 {
			a = append(a, s)
		} else {
			b = append(b, s)
		}
	}
	os.WriteFile(filepath.Join(data, "tweets.js"), []byte("window.YTD.tweets.part0 = ["+strings.Join(a, ",")+"]"), 0o644)
	os.WriteFile(filepath.Join(data, "tweets-part1.js"), []byte("window.YTD.tweets.part1 = ["+strings.Join(b, ",")+"]"), 0o644)

	var l []string
	for i := 1; i <= nLikes; i++ {
		l = append(l, `{"like":{"tweetId":"`+strconv.Itoa(1000+i)+`"}}`)
	}
	os.WriteFile(filepath.Join(data, "like.js"), []byte("window.YTD.like.part0 = ["+strings.Join(l, ",")+"]"), 0o644)
	return dir
}

// Community posts live in community-tweet.js, not tweets.js: they must be included, without duplicates.
func TestTweetIDsIncludesCommunityPosts(t *testing.T) {
	archive := writeArchive(t, 3, 0)
	os.WriteFile(filepath.Join(archive, "data", "community-tweet.js"),
		[]byte(`window.YTD.community_tweet.part0 = [{"tweet":{"id_str":"500","in_reply_to_status_id_str":"9"}},{"tweet":{"id_str":"2"}}]`), 0o644)
	ids, err := tweetIDs(archive)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, id := range ids {
		got[id]++
	}
	if len(ids) != 4 || got["500"] != 1 || got["2"] != 1 {
		t.Fatalf("got %v, want 1, 2, 3 and 500 exactly once each", ids)
	}
}

// -only replies keeps original tweets, retweets and replies in your own threads.
func TestReplyIDsOnlyRepliesToOthers(t *testing.T) {
	archive := writeArchive(t, 0, 0) // account id = 42
	data := filepath.Join(archive, "data")
	os.WriteFile(filepath.Join(data, "tweets.js"), []byte(`window.YTD.tweets.part0 = [
		{"tweet":{"id_str":"1"}},
		{"tweet":{"id_str":"2","in_reply_to_status_id_str":"100","in_reply_to_user_id_str":"7"}},
		{"tweet":{"id_str":"3","in_reply_to_status_id_str":"1","in_reply_to_user_id_str":"42"}},
		{"tweet":{"id_str":"4","full_text":"RT @someone: hi"}}
	]`), 0o644)
	os.WriteFile(filepath.Join(data, "community-tweet.js"), []byte(`window.YTD.community_tweet.part0 = [
		{"tweet":{"id_str":"5","in_reply_to_status_id_str":"200","in_reply_to_user_id_str":"8"}}
	]`), 0o644)

	jobs, err := buildJobs(archive, "http://x/2", "replies")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].kind != "replies" || jobs[0].state != "tweets" {
		t.Fatalf("want a single 'replies' job sharing the tweets state, got %+v", jobs)
	}
	if got := strings.Join(jobs[0].ids, ","); got != "2,5" {
		t.Fatalf("replies = %s, want 2,5", got)
	}
}

// Fake X server: quota of 3 calls per 1 s window, per endpoint, + 429 when exceeded.
type fakeX struct {
	mu        sync.Mutex
	used      map[string]int
	windowEnd time.Time
	calls     atomic.Int64
	got429    atomic.Int64
	deleted   sync.Map
	failID    string
	status    int // if != 0, every response returns this code
}

func (f *fakeX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "OAuth ") || r.Method != http.MethodDelete {
		w.WriteHeader(400)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		w.Write([]byte(`{"title":"forced"}`))
		return
	}
	ep := "tweets"
	if strings.Contains(r.URL.Path, "/likes/") {
		ep = "likes"
	}
	f.mu.Lock()
	if time.Now().After(f.windowEnd) {
		f.used = map[string]int{}
		f.windowEnd = time.Now().Add(time.Second)
	}
	f.used[ep]++
	n, reset := f.used[ep], f.windowEnd
	f.mu.Unlock()

	w.Header().Set("x-rate-limit-reset", strconv.FormatInt(reset.Add(time.Second).Unix(), 10))
	if n > 3 {
		f.got429.Add(1)
		w.WriteHeader(429)
		return
	}
	w.Header().Set("x-rate-limit-remaining", strconv.Itoa(3-n))
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if id == f.failID {
		w.WriteHeader(400)
		w.Write([]byte(`{"detail":"nope"}`))
		return
	}
	f.deleted.Store(ep+":"+id, true)
	if ep == "tweets" {
		w.Write([]byte(`{"data":{"deleted":true}}`))
	} else {
		w.Write([]byte(`{"data":{"liked":false}}`))
	}
}

func runAll(t *testing.T, srv *httptest.Server, archive, state string, limit int) error {
	t.Helper()
	jobs, err := buildJobs(archive, srv.URL+"/2", "")
	if err != nil {
		t.Fatal(err)
	}
	ef, _ := os.OpenFile(filepath.Join(state, "errors.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	defer ef.Close()
	c := &client{http: srv.Client(), creds: &credentials{"k", "s", "t", "ts", nil, nil}}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			if err := runJob(ctx, c, j, opts{stateDir: state, limit: limit, run: true}, &errLog{f: ef}); err != nil {
				cancel(err)
			}
		}(j)
	}
	wg.Wait()
	return context.Cause(ctx)
}

func countLines(t *testing.T, p string) int {
	b, _ := os.ReadFile(p)
	return len(strings.Fields(string(b)))
}

func TestEndToEndParallelResumable(t *testing.T) {
	archive := writeArchive(t, 8, 6)
	state := t.TempDir()
	fx := &fakeX{failID: "1003"}
	srv := httptest.NewServer(fx)
	defer srv.Close()

	// 1st pass limited to 2 per type (resume test)
	if err := runAll(t, srv, archive, state, 2); err != nil {
		t.Fatal(err)
	}
	if countLines(t, filepath.Join(state, "done_tweets.txt")) != 2 {
		t.Fatal("resume: expected 2 tweets after the 1st pass")
	}

	start := time.Now()
	if err := runAll(t, srv, archive, state, 0); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, filepath.Join(state, "done_tweets.txt")); got != 8 {
		t.Fatalf("tweets processed = %d, want 8", got)
	}
	if got := countLines(t, filepath.Join(state, "done_likes.txt")); got != 5 { // 1003 fails
		t.Fatalf("likes processed = %d, want 5", got)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "errors.log")); !strings.Contains(string(b), "1003") {
		t.Fatal("the 1003 failure should be in errors.log")
	}
	// Proactive pacing should avoid almost all 429s.
	if n := fx.got429.Load(); n > 2 {
		t.Fatalf("%d 429 responses: proactive pacing is not working", n)
	}
	t.Logf("2nd pass: %s, %d calls, %d × 429", time.Since(start).Round(time.Millisecond), fx.calls.Load(), fx.got429.Load())

	// Rerun: nothing left to do (except the failed like, retried)
	before := fx.calls.Load()
	runAll(t, srv, archive, state, 0)
	if d := fx.calls.Load() - before; d != 1 {
		t.Fatalf("rerun: %d calls, want 1 (the failed like)", d)
	}
}

func TestFatalStopsBothWorkers(t *testing.T) {
	for _, code := range []int{401, 402, 403} {
		archive := writeArchive(t, 5, 5)
		state := t.TempDir()
		fx := &fakeX{status: code}
		srv := httptest.NewServer(fx)
		err := runAll(t, srv, archive, state, 0)
		srv.Close()
		var fe *fatalError
		if err == nil || !errors.As(err, &fe) {
			t.Fatalf("code %d: expected fatal error, got %v", code, err)
		}
		if n := fx.calls.Load(); n > 2 {
			t.Fatalf("code %d: %d calls, should stop immediately", code, n)
		}
	}
}

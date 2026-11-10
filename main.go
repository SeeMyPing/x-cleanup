// x-cleanup — deletes your tweets and removes your likes on X using the official archive.
//
// Tweets and likes are processed IN PARALLEL (independent API quotas), paced by
// the x-rate-limit-* headers so that no call is ever wasted.
// Default: dry run. Use -go to actually delete. Can be resumed at any time.
//
// Build:   go build -o x-cleanup .
// Usage:   x-cleanup [options] <archive-dir>     (options BEFORE the directory)
//
// Environment variables (console.x.com > your app > Keys and tokens):
//
//	X_API_KEY, X_API_SECRET, X_ACCESS_TOKEN, X_ACCESS_SECRET
//
// ⚠ Set the app to "Read and write" BEFORE generating the Access Token.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultAPI     = "https://api.x.com/2"
	pricePerDelete = 0.01
	perWindow      = 50 // deletions per 15-minute window (per endpoint)
	maxRetries     = 5
)

// ---------- archive ----------

// loadYTD reads data/<name>.js + data/<name>-part*.js ("window.YTD.x.partN = [...]").
func loadYTD(archive, name string) ([]json.RawMessage, error) {
	dataDir := filepath.Join(archive, "data")
	if st, err := os.Stat(dataDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("directory not found: %s (pass the extracted archive directory)", dataDir)
	}
	parts, _ := filepath.Glob(filepath.Join(dataDir, name+"-part*.js"))
	files := append([]string{filepath.Join(dataDir, name+".js")}, parts...)

	var items []json.RawMessage
	for _, f := range files {
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		i := strings.IndexByte(string(b), '[')
		if i < 0 {
			return nil, fmt.Errorf("%s: unexpected format", f)
		}
		var chunk []json.RawMessage
		if err := json.Unmarshal(b[i:], &chunk); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		items = append(items, chunk...)
	}
	return items, nil
}

func unquote(r json.RawMessage) string { return strings.Trim(string(r), `"`) }

func accountID(archive string) (string, error) {
	items, err := loadYTD(archive, "account")
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", errors.New("account.js not found in the archive")
	}
	var v struct {
		Account struct {
			AccountID json.RawMessage `json:"accountId"`
		} `json:"account"`
	}
	if err := json.Unmarshal(items[0], &v); err != nil {
		return "", err
	}
	return unquote(v.Account.AccountID), nil
}

func tweetIDs(archive string) ([]string, error) {
	items, err := loadYTD(archive, "tweets")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		var v struct {
			Tweet struct {
				IDStr string          `json:"id_str"`
				ID    json.RawMessage `json:"id"`
			} `json:"tweet"`
		}
		if err := json.Unmarshal(it, &v); err != nil {
			return nil, err
		}
		id := v.Tweet.IDStr
		if id == "" {
			id = unquote(v.Tweet.ID)
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func likeIDs(archive string) ([]string, error) {
	items, err := loadYTD(archive, "like")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		var v struct {
			Like struct {
				TweetID string `json:"tweetId"`
			} `json:"like"`
		}
		if err := json.Unmarshal(it, &v); err != nil {
			return nil, err
		}
		if v.Like.TweetID != "" {
			ids = append(ids, v.Like.TweetID)
		}
	}
	return ids, nil
}

// ---------- progress ----------

func loadDone(path string) map[string]bool {
	done := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return done
	}
	for _, id := range strings.Fields(string(b)) {
		done[id] = true
	}
	return done
}

type errLog struct {
	mu sync.Mutex
	f  *os.File
}

func (e *errLog) write(kind, id string, status int, body []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(body) > 300 {
		body = body[:300]
	}
	fmt.Fprintf(e.f, "%s\t%s\t%s\t%d\t%s\n", time.Now().Format(time.DateTime), kind, id, status,
		strings.ReplaceAll(string(body), "\n", " "))
}

// ---------- API ----------

type fatalError struct{ msg string }

func (f *fatalError) Error() string { return f.msg }

type client struct {
	http  *http.Client
	creds *credentials // nil in dry-run mode
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func resetTime(h http.Header) (time.Time, bool) {
	s, err := strconv.ParseInt(h.Get("x-rate-limit-reset"), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(s, 0), true
}

// del performs a signed DELETE; handles 429 / 5xx / network errors and proactive pacing.
func (c *client) del(ctx context.Context, tag, rawURL string) (int, []byte, error) {
	for attempt := 0; ; {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, rawURL, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", c.creds.header(http.MethodDelete, rawURL, nil))
		req.Header.Set("User-Agent", "x-cleanup/1.0")

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			if attempt++; attempt > maxRetries {
				return 0, nil, &fatalError{"network unavailable: " + err.Error()}
			}
			if err := sleepCtx(ctx, time.Duration(attempt)*15*time.Second); err != nil {
				return 0, nil, err
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()

		reset, hasReset := resetTime(resp.Header)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			if !hasReset {
				reset = time.Now().Add(15 * time.Minute)
			}
			wait := time.Until(reset) + 2*time.Second
			log.Printf("[%s] ⏸  rate limit reached, resuming around %s", tag, reset.Add(2*time.Second).Format("15:04"))
			if err := sleepCtx(ctx, wait); err != nil {
				return 0, nil, err
			}
			continue
		case resp.StatusCode >= 500:
			if attempt++; attempt > maxRetries {
				return resp.StatusCode, body, nil
			}
			if err := sleepCtx(ctx, time.Duration(attempt)*15*time.Second); err != nil {
				return 0, nil, err
			}
			continue
		}

		// Proactive pacing: if the window's quota is exhausted, wait for the reset
		// instead of triggering a 429.
		if rem := resp.Header.Get("x-rate-limit-remaining"); rem == "0" && hasReset {
			log.Printf("[%s] ⏸  window exhausted, resuming around %s", tag, reset.Add(2*time.Second).Format("15:04"))
			_ = sleepCtx(ctx, time.Until(reset)+2*time.Second) // cancellation is picked up on the next call
		}
		return resp.StatusCode, body, nil
	}
}

func fatalFor(status int, body []byte) error {
	b := string(body)
	if len(b) > 300 {
		b = b[:300]
	}
	switch {
	case status == http.StatusUnauthorized:
		return &fatalError{"401: invalid keys, check the 4 environment variables"}
	case status == http.StatusForbidden:
		return &fatalError{"403: access denied. Is the app set to 'Read and write'? Regenerate the Access Token AFTER changing it.\n" + b}
	case status == http.StatusPaymentRequired || strings.Contains(strings.ToLower(b), "credit"):
		return &fatalError{"API credits exhausted or missing: top up on console.x.com and run again\n" + b}
	}
	return nil
}

// ---------- processing ----------

type job struct {
	kind   string
	ids    []string
	urlFor func(id string) string
	ok     func(body []byte) bool
}

type opts struct {
	stateDir string
	limit    int
	run      bool
}

func pending(j job, o opts) (todo []string, already int) {
	done := loadDone(filepath.Join(o.stateDir, "done_"+j.kind+".txt"))
	for _, id := range j.ids {
		if !done[id] {
			todo = append(todo, id)
		}
	}
	already = len(j.ids) - len(todo)
	if o.limit > 0 && len(todo) > o.limit {
		todo = todo[:o.limit]
	}
	return todo, already
}

func runJob(ctx context.Context, c *client, j job, o opts, el *errLog) error {
	todo, already := pending(j, o)
	log.Printf("[%s] %d in archive, %d already processed, %d to do", j.kind, len(j.ids), already, len(todo))
	if len(todo) == 0 {
		return nil
	}

	f, err := os.OpenFile(filepath.Join(o.stateDir, "done_"+j.kind+".txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	var okN, errN int
	for n, id := range todo {
		status, body, err := c.del(ctx, j.kind, j.urlFor(id))
		if err != nil {
			return err
		}
		switch {
		case (status == http.StatusOK && j.ok(body)) || status == http.StatusNotFound:
			// 404 = already deleted / not found: nothing to do
			okN++
			if _, err := f.WriteString(id + "\n"); err != nil {
				return err
			}
		default:
			if fe := fatalFor(status, body); fe != nil {
				return fe
			}
			errN++
			el.write(j.kind, id, status, body)
		}
		if (n+1)%25 == 0 || n+1 == len(todo) {
			log.Printf("[%s] %d/%d  ok=%d  errors=%d", j.kind, n+1, len(todo), okN, errN)
		}
	}
	msg := fmt.Sprintf("[%s] ✔ done: ok=%d, errors=%d", j.kind, okN, errN)
	if errN > 0 {
		msg += " (see errors.log)"
	}
	log.Print(msg)
	return nil
}

func jsonField(body []byte, field string) (bool, bool) {
	var v struct {
		Data map[string]bool `json:"data"`
	}
	if json.Unmarshal(body, &v) != nil {
		return false, false
	}
	b, ok := v.Data[field]
	return b, ok
}

func buildJobs(archive, apiBase, only string) ([]job, error) {
	var jobs []job
	if only == "" || only == "tweets" {
		ids, err := tweetIDs(archive)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job{
			kind:   "tweets",
			ids:    ids,
			urlFor: func(id string) string { return apiBase + "/tweets/" + id },
			ok:     func(b []byte) bool { v, ok := jsonField(b, "deleted"); return ok && v },
		})
	}
	if only == "" || only == "likes" {
		uid, err := accountID(archive)
		if err != nil {
			return nil, err
		}
		ids, err := likeIDs(archive)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job{
			kind:   "likes",
			ids:    ids,
			urlFor: func(id string) string { return apiBase + "/users/" + uid + "/likes/" + id },
			ok:     func(b []byte) bool { v, ok := jsonField(b, "liked"); return ok && !v },
		})
	}
	return jobs, nil
}

func main() {
	log.SetFlags(log.Ltime)
	only := flag.String("only", "", "only process 'tweets' or 'likes'")
	limit := flag.Int("limit", 0, "only process N items per type (for testing)")
	run := flag.Bool("go", false, "actually delete (otherwise dry run)")
	stateDir := flag.String("state-dir", ".", "where to write done_*.txt and errors.log")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <archive-dir>\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		if flag.NArg() > 1 {
			fmt.Fprintln(os.Stderr, "\n✖ options must be placed BEFORE the archive directory")
		}
		os.Exit(2)
	}
	if *only != "" && *only != "tweets" && *only != "likes" {
		log.Fatal("✖ -only must be 'tweets' or 'likes'")
	}
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		log.Fatal(err)
	}

	apiBase := defaultAPI
	if v := os.Getenv("X_API_BASE"); v != "" { // for tests
		apiBase = strings.TrimRight(v, "/")
	}
	archive := flag.Arg(0)
	jobs, err := buildJobs(archive, apiBase, *only)
	if err != nil {
		log.Fatal("✖ ", err)
	}
	o := opts{stateDir: *stateDir, limit: *limit, run: *run}

	if !*run {
		log.Print("Dry run: nothing will be deleted. Add -go to execute.")
		var total int
		var longest time.Duration
		for _, j := range jobs {
			todo, already := pending(j, o)
			d := time.Duration(float64(len(todo))/perWindow*15) * time.Minute
			log.Printf("[%s] %d in archive, %d already processed, %d to do — ≈ $%.2f, ≈ %s",
				j.kind, len(j.ids), already, len(todo), float64(len(todo))*pricePerDelete, d.Round(time.Minute))
			total += len(todo)
			longest = max(longest, d)
		}
		log.Printf("Total: ≈ $%.2f (pricing for likes to be confirmed), ≈ %s in parallel",
			float64(total)*pricePerDelete, longest.Round(time.Minute))
		return
	}

	creds, err := credsFromEnv()
	if err != nil {
		log.Fatal("✖ ", err)
	}
	ef, err := os.OpenFile(filepath.Join(*stateDir, "errors.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer ef.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	log.Print("⚠  LIVE MODE: deletions are permanent. Ctrl+C to stop (resumable).")
	c := &client{http: &http.Client{Timeout: 30 * time.Second}, creds: creds}
	el := &errLog{f: ef}

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			if err := runJob(ctx, c, j, o, el); err != nil {
				cancel(err) // a fatal error stops both workers
			}
		}(j)
	}
	wg.Wait()

	if cause := context.Cause(ctx); cause != nil {
		var fe *fatalError
		switch {
		case errors.As(cause, &fe):
			log.Print("✖ ", fe.msg)
			os.Exit(1)
		case errors.Is(cause, context.Canceled):
			log.Print("Interrupted. Run the same command again to resume.")
			os.Exit(130)
		default:
			log.Print("✖ ", cause)
			os.Exit(1)
		}
	}
}

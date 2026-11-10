package main

// OAuth 1.0a signature (HMAC-SHA1), stdlib only — RFC 5849.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type credentials struct {
	consumerKey, consumerSecret, token, tokenSecret string

	// injectable for tests
	now   func() time.Time
	nonce func() string
}

func credsFromEnv() (*credentials, error) {
	keys := []string{"X_API_KEY", "X_API_SECRET", "X_ACCESS_TOKEN", "X_ACCESS_SECRET"}
	vals := make([]string, len(keys))
	var missing []string
	for i, k := range keys {
		if vals[i] = os.Getenv(k); vals[i] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing variables: %s", strings.Join(missing, ", "))
	}
	return &credentials{consumerKey: vals[0], consumerSecret: vals[1], token: vals[2], tokenSecret: vals[3]}, nil
}

// pct: RFC 3986 percent-encoding (only A-Z a-z 0-9 - . _ ~ are left as is).
func pct(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func randomNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// header returns the Authorization header value. extra = optional form
// parameters (query parameters from rawURL are always included).
func (c *credentials) header(method, rawURL string, extra map[string]string) string {
	now, nonce := time.Now, randomNonce
	if c.now != nil {
		now = c.now
	}
	if c.nonce != nil {
		nonce = c.nonce
	}

	oauth := map[string]string{
		"oauth_consumer_key":     c.consumerKey,
		"oauth_nonce":            nonce(),
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(now().Unix(), 10),
		"oauth_token":            c.token,
		"oauth_version":          "1.0",
	}

	u, _ := url.Parse(rawURL)
	var pairs [][2]string
	add := func(k, v string) { pairs = append(pairs, [2]string{pct(k), pct(v)}) }
	for k, v := range oauth {
		add(k, v)
	}
	for k, v := range extra {
		add(k, v)
	}
	for k, vs := range u.Query() {
		for _, v := range vs {
			add(k, v)
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	joined := make([]string, len(pairs))
	for i, p := range pairs {
		joined[i] = p[0] + "=" + p[1]
	}

	base := strings.ToUpper(method) + "&" +
		pct(u.Scheme+"://"+u.Host+u.EscapedPath()) + "&" +
		pct(strings.Join(joined, "&"))
	mac := hmac.New(sha1.New, []byte(pct(c.consumerSecret)+"&"+pct(c.tokenSecret)))
	mac.Write([]byte(base))
	oauth["oauth_signature"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))

	keys := make([]string, 0, len(oauth))
	for k := range oauth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf(`%s="%s"`, pct(k), pct(oauth[k]))
	}
	return "OAuth " + strings.Join(parts, ", ")
}

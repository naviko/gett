// Package coin looks up wallet addresses and transactions on public block
// explorers and shows a short summary. Read-only, keyless, experimental.
package coin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gett/internal/errs"
	"gett/internal/httpx"
	"gett/internal/term"
)

type kind int

const (
	address kind = iota
	transaction
)

func (k kind) String() string {
	if k == transaction {
		return "transaction"
	}
	return "address"
}

type row struct{ Label, Value string }

type request struct{ Method, URL, Body string }

type chain struct {
	Symbol    string
	Decimals  int
	Source    string
	requests  func(k kind, id string) []request
	summarize func(k kind, id string, docs []any) []row
}

var hexTx = regexp.MustCompile(`^(0x)?[0-9a-fA-F]{64}$`)

// IsChain reports whether word names a supported coin.
func IsChain(word string) bool {
	_, ok := chains[strings.ToLower(word)]
	return ok
}

// Names lists the supported coins, sorted.
func Names() []string {
	out := make([]string, 0, len(chains))
	for k := range chains {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run looks id up on the explorer for chain and prints the result: a
// summary on a terminal, the explorer's raw JSON when piped.
func Run(ctx context.Context, name, id string, client *http.Client, headers http.Header) error {
	c := chains[strings.ToLower(name)]
	k := address
	if hexTx.MatchString(id) || (c.Symbol == "SOL" && len(id) >= 80) {
		k = transaction
	}
	reqs := c.requests(k, id)
	var bodies [][]byte
	var docs []any
	for i, r := range reqs {
		var body io.Reader
		if r.Body != "" {
			body = strings.NewReader(r.Body)
		}
		req, err := httpx.NewRequest(ctx, r.Method, r.URL, body, headers)
		if err != nil {
			return err
		}
		if r.Body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			if i == 0 {
				return &errs.HTTPStatus{Code: resp.StatusCode, Status: term.Sanitize(resp.Status), URL: resp.Request.URL.Redacted()}
			}
			b = []byte("null") // secondary lookups are optional
		}
		bodies = append(bodies, b)
		docs = append(docs, parseJSON(b))
	}
	if c.Symbol == "SOL" || c.Symbol == "TRX" {
		if err := rpcError(docs[0]); err != nil {
			return err
		}
	}

	if !term.StdoutTTY() {
		os.Stdout.Write(bodies[0])
		if !bytes.HasSuffix(bodies[0], []byte("\n")) {
			os.Stdout.Write([]byte("\n"))
		}
		return nil
	}
	fmt.Printf("%s %s %s\n", strings.ToLower(name), k.String(), term.Sanitize(id))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range c.summarize(k, id, docs) {
		fmt.Fprintf(tw, "%s\t%s\n", r.Label, term.Sanitize(r.Value))
	}
	tw.Flush()
	fmt.Println("source: " + c.Source)
	return nil
}

// rpcError surfaces "not found" answers that come back as HTTP 200.
func rpcError(doc any) error {
	if e := get(doc, "error.message"); e != nil {
		return fmt.Errorf("explorer said: %s", term.Sanitize(str(e)))
	}
	if m, ok := doc.(map[string]any); ok {
		if r, has := m["result"]; has && r == nil {
			return fmt.Errorf("not found")
		}
		if ok, has := m["success"]; has && ok == false {
			return fmt.Errorf("not found")
		}
		if d, has := m["data"]; has {
			if list, isList := d.([]any); isList && len(list) == 0 {
				return fmt.Errorf("not found")
			}
		}
		if len(m) == 0 {
			return fmt.Errorf("not found")
		}
	}
	return nil
}

// --- JSON helpers -----------------------------------------------------------

func parseJSON(b []byte) any {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

// get walks "a.b.0.c" through maps and lists; nil when the path is absent.
func get(doc any, path string) any {
	cur := doc
	for _, p := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[p]
		case []any:
			var i int
			if _, err := fmt.Sscanf(p, "%d", &i); err != nil || i < 0 || i >= len(node) {
				return nil
			}
			cur = node[i]
		default:
			return nil
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "yes"
		}
		return "no"
	default:
		return fmt.Sprint(x)
	}
}

// bigint reads a JSON number or numeric string as an integer; nil if absent.
func bigint(v any) *big.Int {
	s := strings.TrimSpace(str(v))
	if s == "" {
		return nil
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		if f, _, err := big.ParseFloat(s, 10, 256, big.ToNearestEven); err == nil {
			n, _ = f.Int(nil)
			return n
		}
		return nil
	}
	return n
}

func count(v any) int {
	if l, ok := v.([]any); ok {
		return len(l)
	}
	return 0
}

// amount formats base units as coin units with trailing zeros trimmed.
func amount(v any, decimals int, symbol string) string {
	n := bigint(v)
	if n == nil {
		return "unknown"
	}
	neg := n.Sign() < 0
	n = new(big.Int).Abs(n)
	s := n.String()
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-decimals], s[len(s)-decimals:]
	frac = strings.TrimRight(frac, "0")
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out + " " + symbol
}

func sumList(v any, field string) *big.Int {
	total := new(big.Int)
	if l, ok := v.([]any); ok {
		for _, it := range l {
			if n := bigint(get(it, field)); n != nil {
				total.Add(total, n)
			}
		}
	}
	return total
}

func unixTime(v any, ms bool) string {
	n := bigint(v)
	if n == nil || n.Sign() == 0 {
		return ""
	}
	secs := n.Int64()
	if ms {
		secs /= 1000
	}
	return time.Unix(secs, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

func isoTime(v any) string {
	s := str(v)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

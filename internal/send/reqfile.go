package send

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"gett/internal/args"
	"gett/internal/errs"
)

// fileRequest is one request as stored in a JSON file. Every string may use
// ${VAR} or ${VAR:-default} for environment variables.
type fileRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Query   map[string]string `json:"query"`
	JSON    json.RawMessage   `json:"json"`
	Form    map[string]string `json:"form"`
	Files   map[string]string `json:"files"`
	Body    string            `json:"body"`
	Auth    string            `json:"auth"` // "user:pass"
}

// Load reads one request or an array of requests from path.
func Load(path string) ([]Request, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	var items []fileRequest
	if len(raw) > 0 && raw[0] == '[' {
		err = json.Unmarshal(raw, &items)
	} else {
		var one fileRequest
		err = json.Unmarshal(raw, &one)
		items = []fileRequest{one}
	}
	if err != nil {
		return nil, &errs.Usage{Msg: fmt.Sprintf("%s: %v", path, err)}
	}
	out := make([]Request, 0, len(items))
	for i, it := range items {
		r, err := it.toRequest()
		if err != nil {
			return nil, &errs.Usage{Msg: fmt.Sprintf("%s, request %d: %v", path, i+1, err)}
		}
		out = append(out, r)
	}
	return out, nil
}

func (f fileRequest) toRequest() (Request, error) {
	var missing []string
	exp := func(s string) string {
		return os.Expand(s, func(k string) string {
			name, def, hasDef := strings.Cut(k, ":-")
			if v, ok := os.LookupEnv(name); ok && (v != "" || !hasDef) {
				return v
			}
			if hasDef {
				return def
			}
			missing = append(missing, name)
			return ""
		})
	}
	r := Request{
		Method:  strings.ToUpper(exp(f.Method)),
		URL:     exp(f.URL),
		Headers: http.Header{},
		Query:   map[string]string{},
	}
	if r.URL == "" {
		return r, fmt.Errorf("no url")
	}
	for k, v := range f.Headers {
		r.Headers.Add(k, exp(v))
	}
	for k, v := range f.Query {
		r.Query[k] = exp(v)
	}
	if len(f.JSON) > 0 && string(f.JSON) != "null" {
		r.JSON = exp(string(f.JSON))
	}
	for _, k := range sortedKeys(f.Form) {
		r.Form = append(r.Form, args.KV{Key: k, Value: exp(f.Form[k])})
	}
	for _, k := range sortedKeys(f.Files) {
		r.Files = append(r.Files, args.KV{Key: k, Value: exp(f.Files[k])})
	}
	if f.Body != "" {
		r.Body = []byte(exp(f.Body))
	}
	if f.Auth != "" {
		r.User, r.Pass, _ = strings.Cut(exp(f.Auth), ":")
	}
	if len(missing) > 0 {
		return r, fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return r, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

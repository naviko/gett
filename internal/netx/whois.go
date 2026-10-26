package netx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"gett/internal/errs"
)

// Whois prints the registration record for a domain or an address, from
// RDAP, the registry protocol that replaced port-43 whois. rdap.org
// forwards to the registry responsible for the name.
func Whois(ctx context.Context, client *http.Client, target string) error {
	target = hostOf(target)
	kind := "domain"
	if net.ParseIP(target) != nil {
		kind = "ip"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://rdap.org/"+kind+"/"+target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/rdap+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("no record for %s", target)
	}
	if resp.StatusCode != http.StatusOK {
		return &errs.HTTPStatus{Code: resp.StatusCode, Status: resp.Status, URL: req.URL.String()}
	}
	var r struct {
		Name        string   `json:"ldhName"`
		Handle      string   `json:"handle"`
		NetName     string   `json:"name"`
		Start       string   `json:"startAddress"`
		End         string   `json:"endAddress"`
		Country     string   `json:"country"`
		Type        string   `json:"type"`
		Status      []string `json:"status"`
		Nameservers []struct {
			LDHName string `json:"ldhName"`
		}
		Entities []struct {
			Roles []string `json:"roles"`
			Vcard []any    `json:"vcardArray"`
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	// events carry "eventAction"/"eventDate"; map them by hand.
	var raw struct {
		Events []struct {
			Action string `json:"eventAction"`
			Date   string `json:"eventDate"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("could not read the record for %s", target)
	}
	json.Unmarshal(body, &raw)

	if kind == "domain" {
		line("%s", padRow("domain", r.Name))
	} else {
		line("%s", padRow("range", r.Start+" - "+r.End))
		if r.NetName != "" {
			line("%s", padRow("name", r.NetName))
		}
		if r.Country != "" {
			line("%s", padRow("country", r.Country))
		}
		if r.Type != "" {
			line("%s", padRow("type", strings.ToLower(r.Type)))
		}
	}
	for _, role := range []string{"registrar", "registrant"} {
		for _, e := range r.Entities {
			if contains(e.Roles, role) {
				if name := vcardName(e.Vcard); name != "" {
					line("%s", padRow(role, name))
				}
			}
		}
	}
	dates := map[string]string{}
	for _, ev := range raw.Events {
		if t, err := time.Parse(time.RFC3339Nano, ev.Date); err == nil {
			dates[ev.Action] = t.UTC().Format("2006-01-02")
		}
	}
	for _, d := range []struct{ action, label string }{{"registration", "registered"}, {"last changed", "updated"}, {"expiration", "expires"}} {
		if v := dates[d.action]; v != "" {
			line("%s", padRow(d.label, v))
		}
	}
	if len(r.Status) > 0 {
		line("%s", padRow("status", strings.Join(r.Status, ", ")))
	}
	var ns []string
	for _, n := range r.Nameservers {
		ns = append(ns, strings.ToLower(n.LDHName))
	}
	if len(ns) > 0 {
		line("%s", padRow("dns", strings.Join(ns, " ")))
	}
	return nil
}

// vcardName pulls the "fn" entry out of a jCard, the contact format RDAP uses.
func vcardName(v []any) string {
	if len(v) < 2 {
		return ""
	}
	props, ok := v[1].([]any)
	if !ok {
		return ""
	}
	for _, p := range props {
		f, ok := p.([]any)
		if !ok || len(f) < 4 {
			continue
		}
		if name, _ := f[0].(string); name == "fn" {
			if s, ok := f[3].(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

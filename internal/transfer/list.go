package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gett/internal/download"
	"gett/internal/errs"
	"gett/internal/term"
)

// saved is the last list printed, kept so that numbers mean what the user
// saw, plus the passwords that unlocked something.
type saved struct {
	Time      time.Time       `json:"time"`
	Sharers   []savedSharer   `json:"sharers"`
	Items     []savedItem     `json:"items"`
	Passwords []savedPassword `json:"passwords,omitempty"`
}

type savedSharer struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

type savedItem struct {
	N      string    `json:"n"`
	Sharer int       `json:"sharer"`
	Path   string    `json:"path"`
	Dir    bool      `json:"dir,omitempty"`
	Size   int64     `json:"size"`
	MTime  time.Time `json:"mtime"`
	Files  int       `json:"files,omitempty"`
	// Password is set on a share this list saw because of it.
	Password string `json:"password,omitempty"`
}

type savedPassword struct {
	Sharer   string `json:"sharer"`
	Password string `json:"password"`
}

func lastFile() string { return filepath.Join(Home(), "last.json") }

// loadLast returns the last list, or nil when there is none.
func loadLast() *saved {
	b, err := os.ReadFile(lastFile())
	if err != nil {
		return nil
	}
	var s saved
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

func saveLast(s *saved) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", " ")
	tmp := lastFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, lastFile())
}

// passwordsFor is what to send a sharer: the typed password and the ones
// remembered for its name.
func (s *saved) passwordsFor(name, typed string) []string {
	var out []string
	if typed != "" {
		out = append(out, typed)
	}
	if s == nil {
		return out
	}
	for _, p := range s.Passwords {
		if p.Sharer == name && p.Password != typed {
			out = append(out, p.Password)
		}
	}
	return out
}

func (s *saved) sharer(n string) (savedSharer, savedItem, bool) {
	for _, it := range s.Items {
		if it.N == n {
			return s.Sharers[it.Sharer], it, true
		}
	}
	return savedSharer{}, savedItem{}, false
}

// found is one sharer's answer to a listing.
type found struct {
	peer  Peer
	items []item
	kids  map[string][]item // children of each shared folder
}

// List prints what is shared on the local network, numbered. A password
// also lists the shares it unlocks.
func List(ctx context.Context, password string, w io.Writer) error {
	prev := loadLast()
	peers, err := finder.Find(ctx, "")
	if err != nil {
		return err
	}
	client := newClient()
	results := make([]*found, len(peers))
	var wg sync.WaitGroup
	for i, pr := range peers {
		if pr.API != api {
			term.Info("%s runs a different gett version; update the older one", pr.Name)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &peer{Name: pr.Name, Addr: pr.Addr, Port: pr.Port, Passwords: prev.passwordsFor(pr.Name, password), client: client}
			f, err := ask(ctx, p, pr)
			if err != nil {
				term.Info("%s", errs.Message(err))
				return
			}
			results[i] = f
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s := &saved{Time: time.Now()}
	unlocked := map[savedPassword]bool{}
	top := 0
	for _, f := range results {
		if f == nil {
			continue
		}
		s.Sharers = append(s.Sharers, savedSharer{Name: f.peer.Name, ID: f.peer.ID, Addr: f.peer.Addr, Port: f.peer.Port})
		si := len(s.Sharers) - 1
		for _, it := range f.items {
			top++
			n := strconv.Itoa(top)
			if it.Password != "" {
				unlocked[savedPassword{f.peer.Name, it.Password}] = true
			}
			s.Items = append(s.Items, savedItem{N: n, Sharer: si, Path: it.Name, Dir: it.Dir, Size: it.Size, MTime: it.MTime, Files: it.Files, Password: it.Password})
			for j, k := range f.kids[it.Name] {
				s.Items = append(s.Items, savedItem{N: n + "." + strconv.Itoa(j+1), Sharer: si, Path: it.Name + "/" + k.Name, Dir: k.Dir, Size: k.Size, MTime: k.MTime, Files: k.Files})
			}
		}
	}
	// Remember a password that unlocked something; forget one that no
	// longer does on a sharer that is still there.
	present := map[string]bool{}
	for _, sh := range s.Sharers {
		present[sh.Name] = true
	}
	if prev != nil {
		for _, p := range prev.Passwords {
			if !present[p.Sharer] || unlocked[p] {
				s.Passwords = append(s.Passwords, p)
			}
		}
	}
	for p := range unlocked {
		if !containsPassword(s.Passwords, p) {
			s.Passwords = append(s.Passwords, p)
		}
	}
	if err := saveLast(s); err != nil {
		return &errs.Write{Err: err}
	}
	printList(w, s, "")
	typedWorked := false
	for p := range unlocked {
		typedWorked = typedWorked || p.Password == password
	}
	switch {
	case password != "" && !typedWorked:
		term.Info("no shares found for that password")
	case len(s.Items) == 0:
		term.Info("no shares found")
	}
	return nil
}

func containsPassword(ps []savedPassword, p savedPassword) bool {
	for _, q := range ps {
		if q == p {
			return true
		}
	}
	return false
}

// ask fetches a sharer's shares and the children of each shared folder.
func ask(ctx context.Context, p *peer, pr Peer) (*found, error) {
	items, err := p.ls(ctx, "")
	if err != nil {
		return nil, err
	}
	// From here on only the passwords that worked go along, so a wrong one
	// costs one pause, not one per folder.
	p.Passwords = p.Passwords[:0]
	for _, it := range items {
		if it.Password != "" && !slices.Contains(p.Passwords, it.Password) {
			p.Passwords = append(p.Passwords, it.Password)
		}
	}
	f := &found{peer: pr, items: items, kids: map[string][]item{}}
	for _, it := range items {
		if !it.Dir {
			continue
		}
		kids, err := p.ls(ctx, it.Name)
		if err != nil {
			return nil, err
		}
		f.kids[it.Name] = kids
	}
	return f, nil
}

// Ls prints what is inside item number of the last list and adds those
// children to it.
func Ls(ctx context.Context, number string, w io.Writer) error {
	s := loadLast()
	if s == nil {
		return &errs.Usage{Msg: "run gett transfer first to see what is shared"}
	}
	sh, it, ok := s.sharer(number)
	if !ok {
		return &errs.Usage{Msg: "no item " + number + " in the last list"}
	}
	if !it.Dir {
		return &errs.Usage{Msg: number + " is a file"}
	}
	p, err := reach(ctx, sh, s.passwordsFor(sh.Name, ""))
	if err != nil {
		return err
	}
	kids, err := p.ls(ctx, it.Path)
	if err != nil {
		if status(err) == 404 {
			return &errs.Range{Msg: fmt.Sprintf("%s no longer shares %s; run gett transfer again", sh.Name, it.Path)}
		}
		return err
	}
	// Replace what was known below this item.
	var items []savedItem
	for _, x := range s.Items {
		if !strings.HasPrefix(x.N, number+".") {
			items = append(items, x)
		}
		if x.N == number {
			for j, k := range kids {
				items = append(items, savedItem{N: number + "." + strconv.Itoa(j+1), Sharer: x.Sharer, Path: it.Path + "/" + k.Name, Dir: k.Dir, Size: k.Size, MTime: k.MTime, Files: k.Files})
			}
		}
	}
	s.Items = items
	if err := saveLast(s); err != nil {
		return &errs.Write{Err: err}
	}
	printList(w, s, number+".")
	return nil
}

// reach finds a sharer from the last list again, by id first and by name
// second, and returns a peer to talk to. The saved address is a last resort.
func reach(ctx context.Context, sh savedSharer, passwords []string) (*peer, error) {
	peers, err := finder.Find(ctx, sh.ID)
	if err != nil {
		return nil, err
	}
	pr := Peer{Name: sh.Name, Addr: sh.Addr, Port: sh.Port}
	for _, cand := range peers {
		if cand.ID == sh.ID || cand.Name == sh.Name {
			pr = cand
			break
		}
	}
	if pr.API != 0 && pr.API != api {
		return nil, fmt.Errorf("%s runs a different gett version; update the older one", pr.Name)
	}
	return &peer{Name: sh.Name, Addr: pr.Addr, Port: pr.Port, Passwords: passwords, client: newClient()}, nil
}

// printList writes the items whose numbers start with prefix. On a
// terminal each sharer's block starts with a line naming it, then the
// items, and the footer shows what to type next. Piped, every line is
// tab-separated and carries the sharer itself.
func printList(w io.Writer, s *saved, prefix string) {
	tsv := !term.StdoutTTY()
	sharer := -1
	var pull, inside string
	shown := map[string]bool{}
	for _, it := range s.Items {
		if i := strings.LastIndex(it.N, "."); i > 0 {
			shown[it.N[:i]] = true // its parent's contents are on screen
		}
	}
	for _, it := range s.Items {
		if !strings.HasPrefix(it.N, prefix) {
			continue
		}
		sh := s.Sharers[it.Sharer]
		name := term.Sanitize(it.Path)
		if it.Dir {
			name += "/"
		}
		if tsv {
			count := ""
			if it.Dir {
				count = strconv.Itoa(it.Files)
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", it.N, name, it.Size, count, sh.Name, it.Password)
			continue
		}
		if it.Sharer != sharer {
			if sharer >= 0 {
				fmt.Fprintln(w)
			}
			sharer = it.Sharer
			head := "shared by: " + sh.Name
			if pw := s.passwordOf(it.Sharer, prefix); pw != "" {
				head += "   password:" + pw
			}
			fmt.Fprintln(w, head)
		}
		files := ""
		if it.Dir {
			files = plural(it.Files, "file")
		}
		line := fmt.Sprintf("%-6s %-30s %10s  %s", it.N, name, download.Human(it.Size), files)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
		if pull == "" {
			pull = it.N
		}
		if inside == "" && it.Dir && !shown[it.N] {
			inside = it.N
		}
	}
	if tsv || pull == "" {
		return
	}
	term.Info("")
	term.Info("ex: %-31s%s", "gett transfer "+pull, "download item "+pull+" into this folder")
	term.Info("    %-31s%s", "gett transfer "+pull+" <folder>/", "into <folder>/")
	if inside != "" {
		term.Info("    %-31s%s", "gett transfer ls "+inside, "list inside "+inside)
	}
}

// passwordOf is the password that showed this sharer's items, if any.
func (s *saved) passwordOf(sharer int, prefix string) string {
	for _, it := range s.Items {
		if it.Sharer == sharer && strings.HasPrefix(it.N, prefix) && it.Password != "" {
			return it.Password
		}
	}
	return ""
}

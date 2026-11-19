// Package xgett reads and writes .xgett files: any number of files and
// folders in one file, packed and unpacked on most of the machine's cores.
//
// A file is a short header followed by blocks. The contents are laid end to
// end as one stream (the list of entries first, then every file's bytes) and
// the stream is cut into blocks of equal size. Each block is compressed and
// sealed on its own, so blocks can be made and opened in any order.
package xgett

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

// Ext is the extension of gett's own archives.
const Ext = ".xgett"

const (
	magic     = "\x8aXGETT"
	version   = 1
	headerLen = 56
	sealedLen = 40 // the part of the header the key check covers
	saltLen   = 16
	tagLen    = 16
	keyLen    = 32

	flagPassword = 1 // the archive was made with a password

	minBlockLog = 12 // 4 KiB
	maxBlockLog = 26 // 64 MiB

	stored     = 0 // a block kept as it is, because packing did not shrink it
	compressed = 1

	// frameExtra is what a block gains on its way to disk, besides its
	// length: the method byte and the seal.
	frameExtra = 1 + tagLen
)

// The cost of turning a password into a key. It belongs to the format
// version and is never read from a file, so a file cannot choose how much
// memory opening it takes.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
)

// blockLog is the block size new archives get, as a power of two. Tests
// shrink it to cross block borders with small files.
var blockLog = 22

// testWorkers overrides Workers in benchmarks.
var testWorkers int

// Why an archive did not open. Anything else Open returns means the file is
// damaged, and the error says how. The two password errors are only for an
// archive made with a password; ErrNoKey is for any other that does not open.
var (
	ErrNeedsPassword = errors.New("needs a password")
	ErrWrongPassword = errors.New("wrong password")
	ErrNoKey         = errors.New("unable to extract")
	ErrNewer         = errors.New("made by a newer gett")
)

// Workers is how many goroutines pack or unpack at once: four cores in five.
func Workers() int {
	if testWorkers > 0 {
		return testWorkers
	}
	n := runtime.GOMAXPROCS(0) * 8 / 10
	if n < 1 {
		n = 1
	}
	return n
}

// Sniff reports whether head, a file's first bytes, begins a .xgett.
func Sniff(head []byte) bool {
	return len(head) >= len(magic) && string(head[:len(magic)]) == magic
}

// header is the cleartext start of a file.
//
//	0   6  magic
//	6   1  version
//	7   1  flags
//	8   1  block size, log2
//	9   7  zero
//	16  8  number of blocks
//	24  16 salt
//	40  16 key check: the seal of nothing, with bytes 0-39 as associated data
type header struct {
	flags    byte
	blockLog byte
	blocks   uint64
	salt     [saltLen]byte
}

// bytes lays the header out with the key check left empty.
func (h *header) bytes() []byte {
	b := make([]byte, headerLen)
	copy(b, magic)
	b[6] = version
	b[7] = h.flags
	b[8] = h.blockLog
	binary.LittleEndian.PutUint64(b[16:], h.blocks)
	copy(b[24:], h.salt[:])
	return b
}

func parseHeader(b []byte) (*header, error) {
	if len(b) < headerLen || !Sniff(b) {
		return nil, errors.New("it does not start like a .xgett")
	}
	if b[6] != version || b[7]&^flagPassword != 0 || !bytes.Equal(b[9:16], make([]byte, 7)) {
		return nil, ErrNewer
	}
	h := &header{flags: b[7], blockLog: b[8], blocks: binary.LittleEndian.Uint64(b[16:])}
	if h.blockLog < minBlockLog || h.blockLog > maxBlockLog {
		return nil, errors.New("its block size is impossible")
	}
	if h.blocks == 0 || h.blocks > math.MaxInt64>>h.blockLog {
		return nil, errors.New("its block count is impossible")
	}
	copy(h.salt[:], b[24:40])
	return h, nil
}

// newAEAD is the seal on every block and on the header: AES-256-GCM, which
// the processors gett mostly runs on have instructions for. On this format's
// blocks it measured six times faster than ChaCha20-Poly1305 (BenchmarkSeal).
func newAEAD(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // only for a key of the wrong length
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return a
}

// checkNonce seals the header. No block can have it: block nonces start with
// four zero bytes.
var checkNonce = bytes.Repeat([]byte{0xff}, 12)

// nonce is block k's: the key is new for every archive, so a counter never
// repeats under one key, and a block moved to another place does not open.
func nonce(k uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], k)
	return n
}

// plainKey is the key of an archive made without a password.
func plainKey(text string, salt []byte) []byte {
	k, err := hkdf.Key(sha256.New, []byte(text), salt, "xgett v1", keyLen)
	if err != nil {
		panic(err) // only for an impossible key length
	}
	return k
}

func passwordKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, keyLen)
}

// label is the text plainKey is given for an archive.
func label(file string) string { return normalise(stemOf(file)) }

func stemOf(file string) string {
	base := filepath.Base(file)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// normalise keeps the letters and digits of any script, in capitals and in
// one Unicode form.
func normalise(s string) string {
	s = norm.NFC.String(strings.ToUpper(norm.NFC.String(s)))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "XGETT"
	}
	return b.String()
}

var numbered = regexp.MustCompile(`\s*\(\d+\)$`)

// labels are the ones to try for an archive, in order.
func labels(file string) []string {
	stem := stemOf(file)
	out := []string{normalise(stem)}
	for numbered.MatchString(stem) {
		stem = numbered.ReplaceAllString(stem, "")
		out = appendNew(out, normalise(stem))
	}
	return out
}

// wordLabels are the ones a typed word can stand for.
func wordLabels(word string) []string {
	return appendNew([]string{normalise(stemOf(word))}, normalise(word))
}

func appendNew(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}

// Kind is what an entry is.
type Kind byte

const (
	File Kind = iota
	Dir
	Link
)

// Entry is one file, folder or symlink in an archive. Path uses slashes and
// is whatever the archive says: check it before using it on disk.
type Entry struct {
	Kind   Kind
	Exec   bool      // files: someone may run it
	Mtime  time.Time // zero when unknown
	Size   int64     // files
	Path   string
	Target string // links

	off int64 // files: where the bytes start in the stream
}

const execBit = 4

func encodeIndex(es []Entry) []byte {
	b := binary.AppendUvarint(nil, uint64(len(es)))
	for i := range es {
		e := &es[i]
		k := byte(e.Kind)
		if e.Exec {
			k |= execBit
		}
		b = append(b, k)
		var t int64
		if !e.Mtime.IsZero() {
			t = e.Mtime.UnixNano()
		}
		b = binary.AppendVarint(b, t)
		if e.Kind == File {
			b = binary.AppendUvarint(b, uint64(e.Size))
		}
		b = binary.AppendUvarint(b, uint64(len(e.Path)))
		b = append(b, e.Path...)
		if e.Kind == Link {
			b = binary.AppendUvarint(b, uint64(len(e.Target)))
			b = append(b, e.Target...)
		}
	}
	return b
}

func decodeIndex(b []byte) ([]Entry, error) {
	bad := errors.New("its list of contents cannot be read")
	n, w := binary.Uvarint(b)
	if w <= 0 || n > uint64(len(b))/3 { // an entry takes at least three bytes
		return nil, bad
	}
	b = b[w:]
	str := func() (string, bool) {
		n, w := binary.Uvarint(b)
		if w <= 0 || n > uint64(len(b)-w) {
			return "", false
		}
		s := string(b[w : w+int(n)])
		b = b[w+int(n):]
		return s, true
	}
	es := make([]Entry, 0, min(n, 1<<16)) // grown by what is really there, not by what the count claims
	for ; n > 0; n-- {
		if len(b) == 0 {
			return nil, bad
		}
		k := b[0]
		b = b[1:]
		if k&^(3|execBit) != 0 || k&3 > byte(Link) {
			return nil, bad
		}
		e := Entry{Kind: Kind(k & 3), Exec: k&execBit != 0}
		t, w := binary.Varint(b)
		if w <= 0 {
			return nil, bad
		}
		b = b[w:]
		if t != 0 {
			e.Mtime = time.Unix(0, t)
		}
		if e.Kind == File {
			size, w := binary.Uvarint(b)
			if w <= 0 || size > math.MaxInt64 {
				return nil, bad
			}
			b = b[w:]
			e.Size = int64(size)
		}
		var ok bool
		if e.Path, ok = str(); !ok {
			return nil, bad
		}
		if e.Kind == Link {
			if e.Target, ok = str(); !ok {
				return nil, bad
			}
		}
		es = append(es, e)
	}
	if len(b) != 0 {
		return nil, bad
	}
	return es, nil
}

// span is one file's place in the stream. Empty files have none.
type span struct {
	idx       int
	off, size int64
}

// layout gives every file its place, one after the other from data on, and
// returns the length of the whole stream.
func layout(es []Entry, data int64) (total int64, spans []span, err error) {
	off := data
	for i := range es {
		e := &es[i]
		if e.Kind != File {
			continue
		}
		if e.Size < 0 || off+e.Size < off {
			return 0, nil, errors.New("the sizes of its contents do not add up")
		}
		e.off = off
		if e.Size > 0 {
			spans = append(spans, span{i, off, e.Size})
		}
		off += e.Size
	}
	return off, spans, nil
}

// Change says how a size changed: "52% smaller", "109% larger". It is empty
// when there was nothing to compare with.
func Change(from, to int64) string {
	if from <= 0 {
		return ""
	}
	pct := func(d int64) int64 { return int64(math.Round(float64(d) * 100 / float64(from))) }
	switch {
	case to == from:
		return "same size"
	case to < from:
		return fmt.Sprintf("%d%% smaller", pct(from-to))
	}
	return fmt.Sprintf("%d%% larger", pct(to-from))
}

// Took prints a duration to a tenth of a second, or to a millisecond when it
// was shorter than a second.
func Took(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// Tail ends a finish line with how the size changed and how long it took.
func Tail(from, to int64, d time.Duration) string {
	s := ""
	if c := Change(from, to); c != "" {
		s = ", " + c
	}
	return s + ", in " + Took(d)
}

package xgett

import (
	"bufio"
	"context"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// Archive is an open .xgett: its entries are known, its file bytes are not
// read yet. Each or Stream reads them, once.
type Archive struct {
	Entries []Entry
	Size    int64 // the bytes of all its files together
	Packed  int64 // the size of the .xgett itself

	r      *bufio.Reader
	aead   cipher.AEAD
	dec    *zstd.Decoder
	b      int64    // block size
	blocks uint64   // how many there are
	total  int64    // length of the stream
	data   int64    // where the file bytes start in it
	spans  []span   // the files with bytes, in stream order
	lead   [][]byte // the first blocks, opened already to read the entries
}

// Open reads an archive's header and its list of entries. name is the
// archive's file name and key the word given to open it, empty for none. An
// archive with a password answers ErrNeedsPassword when key is empty and
// ErrWrongPassword when it is wrong. Any other that does not open answers
// ErrNoKey.
func Open(f *os.File, name, key string) (*Archive, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	hb := make([]byte, headerLen)
	if _, err := f.ReadAt(hb, 0); err != nil {
		return nil, errors.New("it is too short")
	}
	h, err := parseHeader(hb)
	if err != nil {
		return nil, err
	}
	body := st.Size() - headerLen
	if h.blocks > uint64(body)/(4+frameExtra+1) { // every block takes at least that much room
		return nil, errors.New("it is shorter than its block count says")
	}

	var aead cipher.AEAD
	try := func(k []byte) bool {
		a := newAEAD(k)
		if _, err := a.Open(nil, checkNonce, hb[sealedLen:], hb[:sealedLen]); err != nil {
			return false
		}
		aead = a
		return true
	}
	if h.flags&flagPassword != 0 {
		if key == "" {
			return nil, ErrNeedsPassword
		}
		if !try(passwordKey(key, h.salt[:])) {
			return nil, ErrWrongPassword
		}
	} else {
		tries := labels(name)
		if key != "" {
			tries = append(tries, wordLabels(key)...)
		}
		for _, l := range tries {
			if try(plainKey(l, h.salt[:])) {
				break
			}
		}
		if aead == nil {
			return nil, ErrNoKey
		}
	}

	a := &Archive{
		Packed: st.Size(),
		r:      bufio.NewReaderSize(io.NewSectionReader(f, headerLen, body), 1<<20),
		aead:   aead,
		b:      int64(1) << h.blockLog,
		blocks: h.blocks,
	}
	// One decoder for every worker. A block never unpacks to more than the
	// block size, so that is all the decoder may ever produce.
	a.dec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(Workers()), zstd.WithDecoderMaxMemory(uint64(a.b)))
	if err != nil {
		return nil, err
	}
	if err := a.readIndex(); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// Close frees what Open set up. The file stays open; it is the caller's.
func (a *Archive) Close() { a.dec.Close() }

// readIndex opens blocks from the start until the list of entries is
// complete, and keeps them: what follows the list in them is file bytes.
func (a *Archive) readIndex() error {
	var head []byte
	need := int64(8)
	for k := uint64(0); int64(len(head)) < need; k++ {
		if k == a.blocks {
			return errors.New("its list of contents is cut short")
		}
		fr, err := a.readFrame(nil)
		if err != nil {
			return err
		}
		plain, err := a.openBlock(k, fr, nil)
		if err != nil {
			return err
		}
		a.lead = append(a.lead, plain)
		if k == 0 {
			head = plain[:len(plain):len(plain)] // no copy for the usual case of one block; a second one appends to a new array
		} else {
			head = append(head, plain...)
		}
		if k == 0 {
			if len(head) < 8 {
				return errors.New("its first block is too short")
			}
			n := binary.LittleEndian.Uint64(head)
			if n > uint64(a.b)*a.blocks-8 {
				return errors.New("its list of contents is longer than the file")
			}
			need = 8 + int64(n)
		}
	}
	a.data = need
	entries, err := decodeIndex(head[8:need])
	if err != nil {
		return err
	}
	if a.total, a.spans, err = layout(entries, a.data); err != nil {
		return err
	}
	if uint64((a.total+a.b-1)/a.b) != a.blocks {
		return errors.New("its contents do not fill its blocks")
	}
	for k, p := range a.lead {
		if int64(len(p)) != a.want(uint64(k)) {
			return fmt.Errorf("block %d has the wrong length", k)
		}
	}
	a.Entries = entries
	a.Size = a.total - a.data
	return nil
}

// want is the length block k must have: all are full but the last.
func (a *Archive) want(k uint64) int64 {
	if k == a.blocks-1 {
		return a.total - int64(k)*a.b
	}
	return a.b
}

// Whole reports whether file i arrives in one piece: it has bytes and they
// all lie in one block.
func (a *Archive) Whole(i int) bool {
	e := &a.Entries[i]
	return e.Kind == File && e.Size > 0 && e.off/a.b == (e.off+e.Size-1)/a.b
}

// readFrame reads the next block as it lies on disk, into buf when one is
// given. Frames follow each other without an index, so they are read in
// order, by one goroutine.
func (a *Archive) readFrame(buf []byte) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(a.r, l[:]); err != nil {
		return nil, errors.New("it ends early")
	}
	n := int64(binary.LittleEndian.Uint32(l[:]))
	if n < frameExtra+1 || n > a.b+frameExtra {
		return nil, errors.New("a block has an impossible length")
	}
	if buf == nil {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(a.r, buf); err != nil {
		return nil, errors.New("it ends early")
	}
	return buf, nil
}

// openBlock turns frame k into the bytes it holds, in place when the block
// was stored as it is and in scratch when it was packed.
func (a *Archive) openBlock(k uint64, fr, scratch []byte) ([]byte, error) {
	p, err := a.aead.Open(fr[:0], nonce(k), fr, nil)
	if err != nil {
		return nil, fmt.Errorf("block %d does not open", k)
	}
	switch p[0] {
	case stored:
		return p[1:], nil
	case compressed:
		plain, err := a.dec.DecodeAll(p[1:], scratch[:0])
		if err != nil {
			return nil, fmt.Errorf("block %d does not unpack", k)
		}
		return plain, nil
	}
	return nil, fmt.Errorf("block %d is packed in a way this gett does not know", k)
}

// block is one block on its way through the workers.
type block struct {
	k              uint64
	frame, scratch []byte // from the pool, nil for a block opened earlier
	plain          []byte
}

// run reads every block in order, opens them on the workers, and calls
// inWorker there and inOrder afterwards, one block at a time from the first
// to the last. Either may be nil.
func (a *Archive) run(ctx context.Context, inWorker, inOrder func(b *block) error) error {
	w := Workers()
	bufs := newPool(4*w+4, int(a.b)+frameExtra)
	next := func(k uint64) (*block, error) {
		b := &block{k: k}
		if k < uint64(len(a.lead)) {
			b.plain = a.lead[k]
		} else {
			fr, err := a.readFrame(bufs.get())
			if err != nil {
				return nil, err
			}
			b.frame = fr
		}
		if k == a.blocks-1 {
			if _, err := a.r.ReadByte(); err != io.EOF {
				return nil, errors.New("something follows its last block")
			}
		}
		return b, nil
	}
	work := func(b *block) (*block, error) {
		if b.frame != nil {
			b.scratch = bufs.get()
			plain, err := a.openBlock(b.k, b.frame, b.scratch)
			if err != nil {
				return nil, err
			}
			if int64(len(plain)) != a.want(b.k) {
				return nil, fmt.Errorf("block %d has the wrong length", b.k)
			}
			b.plain = plain
		}
		if inWorker != nil {
			if err := inWorker(b); err != nil {
				return nil, err
			}
		}
		return b, nil
	}
	emit := func(b *block) error {
		if inOrder != nil {
			if err := inOrder(b); err != nil {
				return err
			}
		}
		if b.frame != nil {
			bufs.put(b.frame)
			bufs.put(b.scratch)
		}
		return nil
	}
	return ordered(ctx, w, a.blocks, next, work, emit)
}

// Each calls fn with every piece of every file: i is the entry, off where the
// piece belongs in the file, and p the bytes, valid until fn returns. A file
// that lies in one block comes as one piece; see Whole. fn runs on several
// goroutines at once, and pieces arrive in no particular order.
func (a *Archive) Each(ctx context.Context, fn func(i int, off int64, p []byte) error) error {
	return a.run(ctx, func(b *block) error {
		pos := int64(b.k) * a.b
		end := pos + int64(len(b.plain))
		i := sort.Search(len(a.spans), func(j int) bool { return a.spans[j].off+a.spans[j].size > pos })
		for ; i < len(a.spans) && a.spans[i].off < end; i++ {
			s := a.spans[i]
			from, to := max(s.off, pos), min(s.off+s.size, end)
			if err := fn(s.idx, from-s.off, b.plain[from-pos:to-pos]); err != nil {
				return err
			}
		}
		return nil
	}, nil)
}

// Stream writes the bytes of every file to w, one after the other in the
// order of the entries.
func (a *Archive) Stream(ctx context.Context, w io.Writer) error {
	return a.run(ctx, nil, func(b *block) error {
		p := b.plain
		if pos := int64(b.k) * a.b; pos < a.data {
			p = p[min(a.data-pos, int64(len(p))):]
		}
		_, err := w.Write(p)
		return err
	})
}

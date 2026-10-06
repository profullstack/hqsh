// Package ring keeps the newest numbered output chunks, up to a byte budget,
// so a reconnecting client can resume exactly where it left off.
package ring

import "sync"

// Chunk is one numbered piece of output.
type Chunk struct {
	Seq  uint64
	Data []byte
}

// Buffer is safe for concurrent use.
type Buffer struct {
	mu     sync.Mutex
	limit  int
	size   int
	chunks []Chunk
	next   uint64 // seq the next Append gets; the first is 1
}

// New makes a buffer holding at most limit bytes of output.
func New(limit int) *Buffer {
	return &Buffer{limit: limit, next: 1}
}

// Append numbers and stores a copy of data, dropping the oldest chunks past
// the budget (always keeping the newest). Returns the chunk's seq.
func (b *Buffer) Append(data []byte) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := Chunk{Seq: b.next, Data: append([]byte(nil), data...)}
	b.next++
	b.chunks = append(b.chunks, c)
	b.size += len(c.Data)
	for b.size > b.limit && len(b.chunks) > 1 {
		b.size -= len(b.chunks[0].Data)
		b.chunks = b.chunks[1:]
	}
	return c.Seq
}

// Since returns the chunks after lastSeq, oldest first. gap is true when some
// of them were already dropped, so the client cannot be brought fully up to
// date and should redraw instead.
func (b *Buffer) Since(lastSeq uint64) (chunks []Chunk, gap bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.chunks) == 0 {
		return nil, lastSeq+1 < b.next
	}
	first := b.chunks[0].Seq
	gap = lastSeq+1 < first
	for _, c := range b.chunks {
		if c.Seq > lastSeq {
			chunks = append(chunks, c)
		}
	}
	return chunks, gap
}

// Trim drops chunks the client acknowledged (seq <= acked). Optional: the
// budget alone bounds memory; trimming just frees it sooner.
func (b *Buffer) Trim(acked uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := 0
	for i < len(b.chunks) && b.chunks[i].Seq <= acked {
		b.size -= len(b.chunks[i].Data)
		i++
	}
	b.chunks = b.chunks[i:]
}

// Last is the seq of the newest chunk (0 when none yet).
func (b *Buffer) Last() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.next - 1
}

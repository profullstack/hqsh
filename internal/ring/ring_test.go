package ring

import (
	"math"
	"testing"
)

func TestResumeReplaysExactlyWhatWasMissed(t *testing.T) {
	b := New(1 << 20)
	for _, s := range []string{"a", "b", "c", "d"} {
		b.Append([]byte(s))
	}
	got, gap := b.Since(2) // the client printed a and b
	if gap || len(got) != 2 || string(got[0].Data) != "c" || string(got[1].Data) != "d" {
		t.Fatalf("since 2: %+v gap=%v", got, gap)
	}
	if got, gap := b.Since(4); gap || len(got) != 0 {
		t.Fatalf("up to date: %+v gap=%v", got, gap)
	}
}

func TestOverflowReportsAGap(t *testing.T) {
	b := New(4)
	for _, s := range []string{"aa", "bb", "cc"} { // "aa" no longer fits
		b.Append([]byte(s))
	}
	got, gap := b.Since(0)
	if !gap || len(got) != 2 || got[0].Seq != 2 {
		t.Fatalf("since 0: %+v gap=%v", got, gap)
	}
	if _, gap := b.Since(1); gap {
		t.Fatal("a client that printed seq 1 missed nothing")
	}
}

func TestTheNewestChunkSurvivesEvenWhenOversized(t *testing.T) {
	b := New(2)
	b.Append([]byte("an image far larger than the budget"))
	if got, _ := b.Since(0); len(got) != 1 {
		t.Fatalf("newest chunk dropped: %+v", got)
	}
}

func TestTrimAndLast(t *testing.T) {
	b := New(1 << 20)
	b.Append([]byte("x"))
	b.Append([]byte("y"))
	b.Trim(1)
	if got, _ := b.Since(0); len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("after trim: %+v", got)
	}
	if b.Last() != 2 {
		t.Fatalf("last: %d", b.Last())
	}
	if b.First() != 2 {
		t.Fatalf("first: %d", b.First())
	}
	b.Trim(2)
	if b.First() != 3 {
		t.Fatalf("first when empty is the next seq, got %d", b.First())
	}
}

func TestBehind(t *testing.T) {
	b := New(6)
	b.Append([]byte("aa"))  // 1
	b.Append([]byte("bbb")) // 2
	b.Append([]byte("c"))   // 3
	for seq, want := range map[uint64]int{0: 6, 1: 4, 2: 1, 3: 0, 9: 0} {
		if got := b.Behind(seq); got != want {
			t.Errorf("Behind(%d) = %d, want %d", seq, got, want)
		}
	}
	b.Append([]byte("dd")) // 4; "aa" falls off the 6-byte budget
	if got := b.Behind(0); got != math.MaxInt {
		t.Errorf("a reader that missed dropped output: Behind(0) = %d", got)
	}
	if got := b.Behind(1); got != 6 {
		t.Errorf("Behind(1) = %d, want 6", got)
	}
}

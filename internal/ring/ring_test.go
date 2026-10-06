package ring

import "testing"

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
}

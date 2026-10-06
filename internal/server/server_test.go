package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/profullstack/hqsh/internal/ring"
)

func TestListJSONIsAnArray(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteList(&buf, nil, true); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "[]\n" {
		t.Fatalf("empty list: %q", got)
	}
	buf.Reset()
	in := []SessionInfo{{Name: "main", Attached: true, Clients: 2}, {Name: "work"}}
	if err := WriteList(&buf, in, true); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != `[{"name":"main","attached":true,"clients":2},{"name":"work","attached":false,"clients":0}]`+"\n" {
		t.Fatalf("json: %q", got)
	}
	var back []SessionInfo
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil || len(back) != 2 || !back[0].Attached || back[0].Clients != 2 {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestMinSize(t *testing.T) {
	for _, tc := range []struct {
		sizes      []winsize
		cols, rows uint16
		ok         bool
	}{
		{nil, 0, 0, false},
		{[]winsize{{120, 40}}, 120, 40, true},
		// Smallest cols and smallest rows, each on its own (tmux's default).
		{[]winsize{{200, 30}, {100, 50}}, 100, 30, true},
		{[]winsize{{80, 24}, {80, 24}, {300, 90}}, 80, 24, true},
		// A client that reported no size does not shrink the others to 0.
		{[]winsize{{0, 0}, {132, 43}, {90, 0}}, 132, 43, true},
		{[]winsize{{0, 0}}, 0, 0, false},
	} {
		cols, rows, ok := minSize(tc.sizes)
		if cols != tc.cols || rows != tc.rows || ok != tc.ok {
			t.Errorf("minSize(%v) = %d %d %v, want %d %d %v", tc.sizes, cols, rows, ok, tc.cols, tc.rows, tc.ok)
		}
	}
}

func TestMustWaitPacesToTheFastestClient(t *testing.T) {
	if mustWait(nil, 10) {
		t.Fatal("nobody attached: the shell runs free")
	}
	if !mustWait([]int{11}, 10) {
		t.Fatal("one client past the window: backpressure")
	}
	if mustWait([]int{10}, 10) {
		t.Fatal("at the window is fine")
	}
	if mustWait([]int{1 << 30, 3}, 10) {
		t.Fatal("a lagging client must not stall a caught-up one")
	}
}

func TestMinAck(t *testing.T) {
	if _, ok := minAck(nil); ok {
		t.Fatal("no clients must not trim")
	}
	if s, ok := minAck([]uint64{7}); !ok || s != 7 {
		t.Fatalf("one client: %d %v", s, ok)
	}
	if s, ok := minAck([]uint64{90, 12, 40}); !ok || s != 12 {
		t.Fatalf("the lagging client decides: %d %v", s, ok)
	}
	if s, ok := minAck([]uint64{5, 0}); !ok || s != 0 {
		t.Fatalf("a client that acked nothing keeps everything: %d %v", s, ok)
	}
}

// Trimming to the minimum ACK keeps what the lagging client still needs.
func TestAckMinTrimKeepsTheLaggingClientsOutput(t *testing.T) {
	b := ring.New(1 << 20)
	for i := 0; i < 10; i++ {
		b.Append([]byte{byte('a' + i)})
	}
	fast, slow := uint64(10), uint64(4)
	upTo, _ := minAck([]uint64{fast, slow})
	b.Trim(upTo)
	got, gap := b.Since(slow)
	if gap || len(got) != 6 || got[0].Seq != 5 {
		t.Fatalf("slow client resumes from 5: %+v gap=%v", got, gap)
	}
	if got, gap := b.Since(fast); gap || len(got) != 0 {
		t.Fatalf("fast client is up to date: %+v gap=%v", got, gap)
	}
	// The byte budget still wins over a client that stops acking.
	small := ring.New(3)
	for i := 0; i < 10; i++ {
		small.Append([]byte{'x'})
	}
	if _, gap := small.Since(slow); !gap {
		t.Fatal("a client behind the budget must get the gap path")
	}
}

func TestListText(t *testing.T) {
	var buf bytes.Buffer
	WriteList(&buf, []SessionInfo{{Name: "main", Attached: true}, {Name: "work"}}, false)
	if got := buf.String(); got != "main\tattached\nwork\tdetached\n" {
		t.Fatalf("text: %q", got)
	}
	buf.Reset()
	WriteList(&buf, []SessionInfo{{Name: "pair", Attached: true, Clients: 3}}, false)
	if got := buf.String(); got != "pair\tattached (3 clients)\n" {
		t.Fatalf("text, several clients: %q", got)
	}
	buf.Reset()
	WriteList(&buf, nil, false)
	if got := buf.String(); got != "no sessions\n" {
		t.Fatalf("empty: %q", got)
	}
}

func TestValidSession(t *testing.T) {
	for _, ok := range []string{"main", "work-2", "a_b", "x.y"} {
		if err := ValidSession(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", ".hidden", "a\x00b"} {
		if ValidSession(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

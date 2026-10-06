package server

import (
	"bytes"
	"encoding/json"
	"testing"
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
	in := []SessionInfo{{Name: "main", Attached: true}, {Name: "work"}}
	if err := WriteList(&buf, in, true); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != `[{"name":"main","attached":true},{"name":"work","attached":false}]`+"\n" {
		t.Fatalf("json: %q", got)
	}
	var back []SessionInfo
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil || len(back) != 2 || !back[0].Attached {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestListText(t *testing.T) {
	var buf bytes.Buffer
	WriteList(&buf, []SessionInfo{{Name: "main", Attached: true}, {Name: "work"}}, false)
	if got := buf.String(); got != "main\tattached\nwork\tdetached\n" {
		t.Fatalf("text: %q", got)
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

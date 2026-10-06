package client

import (
	"errors"
	"strings"
	"testing"
)

const tsJSON = `{
  "BackendState": "Running",
  "Peer": {
    "a": {"HostName": "dev2", "DNSName": "dev2.tail1234.ts.net.", "TailscaleIPs": ["fd7a:115c::1", "100.64.0.2"], "Online": true, "OS": "linux"},
    "b": {"HostName": "laptop", "DNSName": "laptop.tail1234.ts.net.", "TailscaleIPs": ["100.64.0.3"], "Online": false, "OS": "macOS"},
    "c": {"HostName": "Vienna-Box", "DNSName": "vienna-box.tail1234.ts.net.", "TailscaleIPs": ["100.64.0.4"], "Online": true, "OS": "linux"}
  }
}`

func fakeTailscale(t *testing.T, json string, err error, resolved map[string]string) {
	t.Helper()
	oldJSON, oldResolve := tailscaleJSON, sshResolve
	t.Cleanup(func() { tailscaleJSON, sshResolve = oldJSON, oldResolve })
	tailscaleJSON = func() ([]byte, error) { return []byte(json), err }
	sshResolve = func(host string) string { return resolved[host] }
}

func TestMatchPeer(t *testing.T) {
	fakeTailscale(t, tsJSON, nil, nil)
	peers, ok := TailscalePeers()
	if !ok || len(peers) != 2 {
		t.Fatalf("online peers: %v %v", peers, ok)
	}
	for _, c := range []struct {
		host, resolved, want string
	}{
		{"dev2", "", "dev2"},                     // machine name
		{"anthony@dev2", "", "dev2"},             // with a user
		{"dev2.tail1234.ts.net", "", "dev2"},     // MagicDNS
		{"100.64.0.2", "", "dev2"},               // tailnet address
		{"prod", "dev2.tail1234.ts.net", "dev2"}, // ssh alias resolving to MagicDNS
		{"vienna-box", "", "Vienna-Box"},         // case
		{"dev2.profullstack.com", "", ""},        // a public name is not the peer
		{"d2", "dev2.profullstack.com", ""},      // ...nor an alias resolving to one
		{"laptop", "", ""},                       // offline
		{"nothing", "", ""},
	} {
		p, ok := MatchPeer(c.host, c.resolved, peers)
		got := ""
		if ok {
			got = p.HostName
		}
		if got != c.want {
			t.Errorf("MatchPeer(%q, %q) = %q, want %q", c.host, c.resolved, got, c.want)
		}
	}
	if a := PeerAddr(peers[0]); !strings.HasPrefix(a, "100.") && peers[0].HostName == "dev2" {
		t.Errorf("PeerAddr prefers IPv4, got %s", a)
	}
}

func TestTailnetRoute(t *testing.T) {
	fakeTailscale(t, tsJSON, nil, map[string]string{"dev2": "dev2"})
	addr, alias, err := tailnetRoute("dev2", TailscaleAuto)
	if err != nil || addr != "100.64.0.2" || alias != "dev2" {
		t.Fatalf("auto, a peer: %q %q %v", addr, alias, err)
	}
	if addr, _, err := tailnetRoute("dev2", TailscaleOff); addr != "" || err != nil {
		t.Fatalf("off: %q %v", addr, err)
	}
	if addr, _, err := tailnetRoute("elsewhere", TailscaleAuto); addr != "" || err != nil {
		t.Fatalf("auto, not a peer: %q %v", addr, err)
	}
	if _, _, err := tailnetRoute("elsewhere", TailscaleOn); err == nil {
		t.Fatal("on, not a peer: want an error")
	}
	fakeTailscale(t, "", errors.New("not installed"), nil)
	if addr, _, err := tailnetRoute("dev2", TailscaleAuto); addr != "" || err != nil {
		t.Fatalf("auto without tailscale: %q %v", addr, err)
	}
	if _, _, err := tailnetRoute("dev2", TailscaleOn); err == nil {
		t.Fatal("on without tailscale: want an error")
	}
	fakeTailscale(t, `{"BackendState":"Stopped","Peer":{}}`, nil, nil)
	if _, ok := TailscalePeers(); ok {
		t.Fatal("a stopped Tailscale is not running")
	}
}

func TestSSHCommandOverTheTailnet(t *testing.T) {
	got := strings.Join(SSHCommand(Options{Host: "dev2", Via: "100.64.0.2", ViaKeyAlias: "dev2.profullstack.com", SSHArgs: []string{"-p", "2202"}}), " ")
	want := "ssh -T -o ServerAliveInterval=10 -o HostName=100.64.0.2 -o HostKeyAlias=dev2.profullstack.com -p 2202 dev2 hqsh server attach main"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestRouteFallsBackAndAlternates(t *testing.T) {
	r := &runner{tsAddr: "100.64.0.2", tsAlias: "dev2"}
	r.route(false, 0)
	if r.o.Via != "100.64.0.2" {
		t.Fatal("first connection should try the tailnet")
	}
	r.tsBroken = true
	r.route(false, 0)
	if r.o.Via != "" {
		t.Fatal("after the tailnet failed the first connection, go direct")
	}
	r.tsBroken = false
	var via []string
	for a := 0; a < 4; a++ {
		r.route(true, a)
		via = append(via, r.o.Via)
	}
	if strings.Join(via, ",") != "100.64.0.2,,100.64.0.2," {
		t.Fatalf("reconnects should alternate, got %v", via)
	}
	r.tsRequired = true
	r.route(true, 1)
	if r.o.Via == "" {
		t.Fatal("--tailscale on always uses the tailnet")
	}
}

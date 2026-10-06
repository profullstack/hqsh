package client

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// Tailscale, when it is running here and the host is one of its online
// peers, gives a path that survives network changes better than the public
// one: hqsh then points ssh at the peer's tailnet address. Only the address
// changes (-o HostName); the user, port and keys still come from the ssh
// config, and HostKeyAlias keeps the host's known_hosts entry in use.
//
//	auto  use the tailnet when the host is a peer (the default)
//	on    require it: fail if the host is not an online peer
//	off   never
const (
	TailscaleAuto = "auto"
	TailscaleOn   = "on"
	TailscaleOff  = "off"
)

// TailscalePeer is the part of `tailscale status --json` hqsh uses.
type TailscalePeer struct {
	HostName     string
	DNSName      string
	TailscaleIPs []string
	Online       bool
	OS           string
}

type tailscaleStatus struct {
	BackendState string
	Peer         map[string]TailscalePeer
}

// Seams for tests.
var (
	tailscaleJSON = func() ([]byte, error) {
		return exec.Command("tailscale", "status", "--json").Output()
	}
	sshResolve = func(host string) string {
		out, err := exec.Command("ssh", "-G", host).Output()
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, " "); ok && k == "hostname" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
)

// TailscalePeers returns the online peers, or ok=false when Tailscale is not
// installed or not running.
func TailscalePeers() (peers []TailscalePeer, ok bool) {
	out, err := tailscaleJSON()
	if err != nil {
		return nil, false
	}
	var st tailscaleStatus
	if json.Unmarshal(out, &st) != nil || st.BackendState != "Running" {
		return nil, false
	}
	for _, p := range st.Peer {
		if p.Online && len(p.TailscaleIPs) > 0 {
			peers = append(peers, p)
		}
	}
	return peers, true
}

func label(name string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	first, _, _ := strings.Cut(name, ".")
	return first
}

// MatchPeer finds the peer that host (an ssh destination: alias, name or
// address, optionally user@) refers to. resolved is what ssh would connect
// to for it (`ssh -G`'s hostname), or "".
func MatchPeer(host, resolved string, peers []TailscalePeer) (TailscalePeer, bool) {
	if _, h, ok := strings.Cut(host, "@"); ok {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	resolved = strings.ToLower(strings.TrimSuffix(resolved, "."))
	for _, p := range peers {
		dns := strings.ToLower(strings.TrimSuffix(p.DNSName, "."))
		for _, cand := range []string{host, resolved} {
			if cand == "" {
				continue
			}
			// The peer's MagicDNS name, its machine name, or one of its
			// tailnet addresses.
			if cand == dns || (cand == strings.ToLower(p.HostName) && !strings.Contains(cand, ".")) ||
				(cand == label(dns) && !strings.Contains(cand, ".")) {
				return p, true
			}
			for _, ip := range p.TailscaleIPs {
				if cand == strings.ToLower(ip) {
					return p, true
				}
			}
		}
	}
	return TailscalePeer{}, false
}

// PeerAddr is the peer's IPv4 tailnet address if it has one, else its first.
func PeerAddr(p TailscalePeer) string {
	for _, ip := range p.TailscaleIPs {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			return ip
		}
	}
	return p.TailscaleIPs[0]
}

// tailnetRoute decides the tailnet address for o.Host under mode. keyAlias
// is the name whose known_hosts entry the connection should keep using.
func tailnetRoute(host, mode string) (addr, keyAlias string, err error) {
	if mode == TailscaleOff {
		return "", "", nil
	}
	peers, running := TailscalePeers()
	if !running {
		if mode == TailscaleOn {
			return "", "", fmt.Errorf("hqsh: --tailscale on, but Tailscale is not running here")
		}
		return "", "", nil
	}
	resolved := sshResolve(host)
	p, ok := MatchPeer(host, resolved, peers)
	if !ok {
		if mode == TailscaleOn {
			return "", "", fmt.Errorf("hqsh: --tailscale on, but %s is not an online peer on this tailnet", host)
		}
		return "", "", nil
	}
	keyAlias = resolved
	if keyAlias == "" {
		keyAlias = host
		if _, h, ok := strings.Cut(host, "@"); ok {
			keyAlias = h
		}
	}
	return PeerAddr(p), keyAlias, nil
}

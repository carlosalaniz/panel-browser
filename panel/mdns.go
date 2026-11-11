// Finding renderers on the LAN with DNS-SD over mDNS (RFC 6762/6763), without avahi on the Pi.
//
// The query goes out from an ordinary UDP port, so responders (avahi on a Harbor machine, mDNSResponder
// on a Mac) answer straight back to it ("legacy unicast", RFC 6762 §6.7) and nothing has to bind 5353.
package main

import (
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const serviceType = "_desk-panel._tcp.local."

// renderer is one announced renderer.
type renderer struct {
	Name string // instance name, e.g. "Panel Browser" or "Panel Browser #2"
	Host string // e.g. "harbor.local."
	IP   net.IP
	Port int
	TXT  map[string]string
}

// url is the panel stream's WebSocket address, or "" when the renderer is not usable yet.
func (r renderer) url() string {
	if r.IP == nil || r.Port == 0 {
		return ""
	}
	if r.TXT["scheme"] == "https" {
		return "" // secure-only Harbor: the panel would need Harbor's certificate authority (not supported yet)
	}
	path := r.TXT["path"]
	if !strings.HasPrefix(path, "/") {
		path = "/panel"
	}
	return "ws://" + net.JoinHostPort(r.IP.String(), strconv.Itoa(r.Port)) + path
}

func query(conn *net.UDPConn, name string, typ dnsmessage.Type) {
	n, err := dnsmessage.NewName(name)
	if err != nil {
		return
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: n, Type: typ, Class: dnsmessage.ClassINET})
	msg, err := b.Finish()
	if err != nil {
		return
	}
	conn.WriteToUDP(msg, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
}

// txtMap reads DNS-SD key=value strings.
func txtMap(items []string) map[string]string {
	m := map[string]string{}
	for _, s := range items {
		k, v, _ := strings.Cut(s, "=")
		if k = strings.ToLower(k); k != "" {
			if _, seen := m[k]; !seen {
				m[k] = v
			}
		}
	}
	return m
}

// instanceName turns "Panel\ Browser._desk-panel._tcp.local." into "Panel Browser".
func instanceName(full string) string {
	label := strings.TrimSuffix(full, "."+serviceType)
	var b strings.Builder
	for i := 0; i < len(label); i++ {
		c := label[i]
		if c == '\\' && i+3 < len(label) && isDigit(label[i+1]) && isDigit(label[i+2]) && isDigit(label[i+3]) {
			n, _ := strconv.Atoi(label[i+1 : i+4])
			b.WriteByte(byte(n))
			i += 3
		} else if c == '\\' && i+1 < len(label) {
			b.WriteByte(label[i+1])
			i++
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// findRenderers asks the LAN for renderers and collects answers for `wait`.
func findRenderers(wait time.Duration) ([]renderer, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	instances := map[string]bool{}
	type srv struct {
		target string
		port   int
	}
	srvs := map[string]srv{}
	txts := map[string]map[string]string{}
	addrs := map[string]net.IP{}

	query(conn, serviceType, dnsmessage.TypePTR)
	deadline := time.Now().Add(wait)
	nextAsk := time.Now().Add(300 * time.Millisecond)
	buf := make([]byte, 9000)
	for time.Now().Before(deadline) {
		// Ask again for whatever is still missing: responders may skip the extra records.
		if time.Now().After(nextAsk) {
			nextAsk = time.Now().Add(400 * time.Millisecond)
			if len(instances) == 0 {
				query(conn, serviceType, dnsmessage.TypePTR)
			}
			for in := range instances {
				s, ok := srvs[in]
				if !ok {
					query(conn, in, dnsmessage.TypeSRV)
				} else if addrs[s.target] == nil {
					query(conn, s.target, dnsmessage.TypeA)
				}
				if _, ok := txts[in]; !ok {
					query(conn, in, dnsmessage.TypeTXT)
				}
			}
		}
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var p dnsmessage.Parser
		if _, err := p.Start(buf[:n]); err != nil {
			continue
		}
		p.SkipAllQuestions()
		var rrs []dnsmessage.Resource
		for _, section := range []func() ([]dnsmessage.Resource, error){p.AllAnswers, p.AllAuthorities, p.AllAdditionals} {
			r, err := section()
			if err != nil {
				break
			}
			rrs = append(rrs, r...)
		}
		for _, rr := range rrs {
			owner := strings.ToLower(rr.Header.Name.String())
			switch body := rr.Body.(type) {
			case *dnsmessage.PTRResource:
				if owner == serviceType {
					instances[body.PTR.String()] = true
				}
			case *dnsmessage.SRVResource:
				srvs[rr.Header.Name.String()] = srv{strings.ToLower(body.Target.String()), int(body.Port)}
			case *dnsmessage.TXTResource:
				txts[rr.Header.Name.String()] = txtMap(body.TXT)
			case *dnsmessage.AResource:
				addrs[owner] = net.IP(body.A[:])
			}
		}
		// Done early once every instance is complete.
		if len(instances) > 0 {
			complete := true
			for in := range instances {
				s, ok := srvs[in]
				_, hasTxt := txts[in]
				if !ok || !hasTxt || addrs[s.target] == nil {
					complete = false
				}
			}
			if complete && time.Until(deadline) > 200*time.Millisecond {
				deadline = time.Now().Add(200 * time.Millisecond) // a little longer for other responders
			}
		}
	}

	var out []renderer
	for in := range instances {
		s := srvs[in]
		r := renderer{Name: instanceName(in), Host: s.target, Port: s.port, IP: addrs[s.target], TXT: txts[in]}
		if r.TXT == nil {
			r.TXT = map[string]string{}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

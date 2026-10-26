package egress

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func tcpDNS(t *testing.T, answer func(dnsmessage.Message) dnsmessage.Message) (string, *atomic.Int32) {
	t.Helper()
	return tcpDNSAt(t, "127.0.0.1:0", answer)
}

func tcpDNSAt(t *testing.T, address string, answer func(dnsmessage.Message) dnsmessage.Message) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer c.Close()
				calls.Add(1)
				var size [2]byte
				if _, err := io.ReadFull(c, size[:]); err != nil {
					return
				}
				data := make([]byte, int(binary.BigEndian.Uint16(size[:])))
				if _, err := io.ReadFull(c, data); err != nil {
					return
				}
				var q dnsmessage.Message
				if q.Unpack(data) != nil {
					return
				}
				a := answer(q)
				wire, err := a.Pack()
				if err != nil {
					return
				}
				binary.BigEndian.PutUint16(size[:], uint16(len(wire)))
				_, _ = c.Write(append(size[:], wire...))
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done; workers.Wait() })
	return listener.Addr().String(), &calls
}

func TestTCPResolverCannotResolveBusinessTargetToItself(t *testing.T) {
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var host netip.Addr
	for _, raw := range interfaces {
		prefix, err := netip.ParsePrefix(raw.String())
		if err == nil && prefix.Addr().Is4() && prefix.Addr().IsGlobalUnicast() && !prefix.Addr().IsLoopback() {
			host = prefix.Addr()
			break
		}
	}
	if !host.IsValid() {
		t.Fatal("resolver destination test requires a non-loopback IPv4 interface")
	}
	endpoint, calls := tcpDNSAt(t, net.JoinHostPort(host.String(), "0"), func(q dnsmessage.Message) dnsmessage.Message {
		a := dnsAnswer(q, false)
		if q.Questions[0].Type == dnsmessage.TypeA {
			a.Answers[0].Body = &dnsmessage.AResource{A: host.As4()}
		}
		return a
	})
	raw := strings.Replace(enterpriseFixture, `"targets"`, `"resolver":{"transport":"tcp","address":"`+endpoint+`"},"targets"`, 1)
	raw = strings.Replace(raw, `,"staticAddresses":["10.60.0.10"]`, "", 1)
	raw = strings.Replace(raw, `"allowedCidrs":["10.60.0.0/16"],`, "", 1)
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(t.Context(), "model.example.invalid:443"); err != ErrForbidden || calls.Load() != 2 {
		t.Fatalf("resolver address reached business admission: %v, queries %d", err, calls.Load())
	}
}

func dnsAnswer(q dnsmessage.Message, ipv6 bool) dnsmessage.Message {
	a := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true}, Questions: q.Questions}
	question := q.Questions[0]
	if question.Type == dnsmessage.TypeA {
		a.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AResource{A: [4]byte{10, 60, 0, 10}}}}
	} else if ipv6 {
		a.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("fd12::1").As16()}}}
	}
	return a
}

func TestTCPDNSFamiliesCNAMEAndFailureBoundaries(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(dnsmessage.Message) dnsmessage.Message
		fail   bool
	}{
		{"both", func(q dnsmessage.Message) dnsmessage.Message { return dnsAnswer(q, true) }, false},
		{"nodata", func(q dnsmessage.Message) dnsmessage.Message { return dnsAnswer(q, false) }, false},
		{"family-failure", func(q dnsmessage.Message) dnsmessage.Message {
			a := dnsAnswer(q, false)
			if q.Questions[0].Type == dnsmessage.TypeAAAA {
				a.RCode = dnsmessage.RCodeServerFailure
			}
			return a
		}, true},
		{"wrong-id", func(q dnsmessage.Message) dnsmessage.Message { a := dnsAnswer(q, true); a.ID++; return a }, true},
		{"truncated", func(q dnsmessage.Message) dnsmessage.Message { a := dnsAnswer(q, true); a.Truncated = true; return a }, true},
		{"additional-only", func(q dnsmessage.Message) dnsmessage.Message {
			a := dnsAnswer(q, true)
			a.Additionals = a.Answers
			a.Answers = nil
			return a
		}, true},
		{"cname-loop", func(q dnsmessage.Message) dnsmessage.Message {
			a := dnsAnswer(q, false)
			a.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}, Body: &dnsmessage.CNAMEResource{CNAME: q.Questions[0].Name}}}
			return a
		}, true},
		{"cname-follow", func(q dnsmessage.Message) dnsmessage.Message {
			a := dnsAnswer(q, false)
			if q.Questions[0].Name.String() == "model.example.invalid." {
				alias, _ := dnsmessage.NewName("canonical.example.invalid.")
				a.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}, Body: &dnsmessage.CNAMEResource{CNAME: alias}}}
			}
			return a
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			address, calls := tcpDNS(t, test.change)
			got, err := lookupTCP(context.Background(), address, "model.example.invalid")
			if (err != nil) != test.fail {
				t.Fatalf("addresses=%v err=%v", got, err)
			}
			if calls.Load() > 18 {
				t.Fatal("unbounded queries")
			}
			if !test.fail && got[0] != netip.MustParseAddr("10.60.0.10") {
				t.Fatal(got)
			}
		})
	}
}

func TestPolicyTCPRejectsMixedAnswersAndUnlistedHostsBeforeLookup(t *testing.T) {
	address, calls := tcpDNS(t, func(q dnsmessage.Message) dnsmessage.Message {
		a := dnsAnswer(q, true)
		if q.Questions[0].Type == dnsmessage.TypeAAAA {
			a.Answers[0].Body = &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("::1").As16()}
		}
		return a
	})
	raw := strings.Replace(enterpriseFixture, `"targets"`, `"resolver":{"transport":"tcp","address":"`+address+`"},"targets"`, 1)
	raw = strings.Replace(raw, `,"staticAddresses":["10.60.0.10"]`, "", 1)
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Resolve(context.Background(), "absent.example.invalid:443"); err == nil || calls.Load() != 0 {
		t.Fatal("unlisted host queried")
	}
	if _, err = p.Resolve(context.Background(), "model.example.invalid:443"); err != ErrForbidden {
		t.Fatalf("mixed answers accepted %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = lookupTCP(ctx, address, "model.example.invalid"); err == nil {
		t.Fatal("cancel ignored")
	}
	want := []netip.Addr{netip.MustParseAddr("10.60.0.10")}
	static, err := Parse([]byte(enterpriseFixture))
	if err != nil {
		t.Fatal(err)
	}
	got, err := static.Resolve(context.Background(), "model.example.invalid:443")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v %v", got, err)
	}
}

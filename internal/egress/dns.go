package egress

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// One lookup has a five-second budget, at most eight CNAME hops per family and 256
// records per message. Only answer records associated with the question can yield addresses.
func lookupTCP(ctx context.Context, resolver, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var addresses []netip.Addr
	for _, kind := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		result, err := lookupFamily(ctx, resolver, host, kind)
		if err != nil {
			return nil, ErrResolution
		}
		addresses = append(addresses, result...)
	}
	if len(addresses) == 0 {
		return nil, ErrResolution
	}
	return addresses, nil
}

func lookupFamily(ctx context.Context, resolver, host string, kind dnsmessage.Type) ([]netip.Addr, error) {
	name := strings.ToLower(strings.TrimSuffix(host, ".")) + "."
	seen := map[string]bool{}
	for len(seen) < 9 {
		if seen[name] {
			return nil, ErrResolution
		}
		seen[name] = true
		qname, err := dnsmessage.NewName(name)
		if err != nil {
			return nil, ErrResolution
		}
		answer, err := exchangeTCP(ctx, resolver, &dnsmessage.Question{Name: qname, Type: kind, Class: dnsmessage.ClassINET})
		if err != nil {
			return nil, ErrResolution
		}
		// Walk aliases already present in the response before issuing another query.
		for {
			var next string
			var result []netip.Addr
			for index := range answer.Answers {
				resource := &answer.Answers[index]
				if resource.Header.Class != dnsmessage.ClassINET {
					return nil, ErrResolution
				}
				if !strings.EqualFold(resource.Header.Name.String(), name) {
					continue
				}
				switch body := resource.Body.(type) {
				case *dnsmessage.CNAMEResource:
					alias := strings.ToLower(body.CNAME.String())
					if next != "" && next != alias {
						return nil, ErrResolution
					}
					next = alias
				case *dnsmessage.AResource:
					if kind != dnsmessage.TypeA {
						return nil, ErrResolution
					}
					result = append(result, netip.AddrFrom4(body.A))
				case *dnsmessage.AAAAResource:
					if kind != dnsmessage.TypeAAAA {
						return nil, ErrResolution
					}
					result = append(result, netip.AddrFrom16(body.AAAA))
				}
			}
			if next == "" {
				return result, nil
			}
			if len(result) != 0 || seen[next] || len(seen) >= 9 {
				return nil, ErrResolution
			}
			name = next
			hasAnswer := false
			for index := range answer.Answers {
				resource := &answer.Answers[index]
				if strings.EqualFold(resource.Header.Name.String(), name) {
					hasAnswer = true
				}
			}
			if !hasAnswer {
				break
			}
			seen[name] = true
		}
	}
	return nil, ErrResolution
}

func exchangeTCP(ctx context.Context, resolver string, question *dnsmessage.Question) (*dnsmessage.Message, error) {
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, ErrResolution
	}
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true}, Questions: []dnsmessage.Question{*question}}
	wire, err := query.Pack()
	if err != nil {
		return nil, ErrResolution
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", resolver)
	if err != nil {
		return nil, ErrResolution
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, ErrResolution
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if len(wire) > 65535 {
		return nil, ErrResolution
	}
	frame := make([]byte, 2+len(wire))
	frame[0] = byte((len(wire) >> 8) & 0xff)
	frame[1] = byte(len(wire) & 0xff)
	copy(frame[2:], wire)
	if _, err := io.Copy(conn, bytes.NewReader(frame)); err != nil {
		return nil, ErrResolution
	}
	var length [2]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		return nil, ErrResolution
	}
	data := make([]byte, int(binary.BigEndian.Uint16(length[:])))
	if len(data) < 12 {
		return nil, ErrResolution
	}
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, ErrResolution
	}
	// Reject record bombs before the decoder allocates their resource bodies.
	count := int(binary.BigEndian.Uint16(data[6:8])) + int(binary.BigEndian.Uint16(data[8:10])) + int(binary.BigEndian.Uint16(data[10:12]))
	if count > 256 {
		return nil, ErrResolution
	}
	var answer dnsmessage.Message
	if answer.Unpack(data) != nil || !answer.Response || answer.Truncated || answer.OpCode != 0 || answer.ID != query.ID || answer.RCode != dnsmessage.RCodeSuccess || len(answer.Questions) != 1 || answer.Questions[0] != *question {
		return nil, ErrResolution
	}
	return &answer, nil
}

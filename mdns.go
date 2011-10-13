package zeroconf

// convenience routines

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	dns "github.com/miekg/godns"
)

type Host struct {
	Name   string
	Domain string
	Addrs  []net.IP
}

type Service struct {
	*Host
	*Type
	Port uint16
}

type proto int

func (p proto) String() string {
	if p == tcp {
		return "_tcp"
	}
	return "_udp"
}

const (
	tcp proto = iota
	udp
)

type Type struct {
	name string
	proto
}

var (
	Ssh = &Type{"_ssh", tcp}
)

func (s *Service) fqdn() string {
	return fmt.Sprintf("%s.%s", s.Name, s.Domain)
}

func (s *Service) service() string {
	return fmt.Sprintf("%s.%s.%s", s.Type.name, s.Type.proto.String(), s.Domain)
}

func (s *Service) serviceFqdn() string {
	return s.Name + "." + s.service()
}

func Publish(z Zone, s *Service) {
	for _, addr := range s.Addrs {
		a := dns.NewRR(dns.TypeA).(*dns.RR_A)
		a.Hdr.Name = s.fqdn()
		a.Hdr.Class = dns.ClassINET
		a.Hdr.Ttl = 3600
		a.A = addr
		PublishRR(z, a)
	}

	ptr := dns.NewRR(dns.TypePTR).(*dns.RR_PTR)
	ptr.Hdr.Name = s.service()
	ptr.Hdr.Class = dns.ClassINET
	ptr.Hdr.Ttl = 3600
	ptr.Ptr = s.serviceFqdn()
	PublishRR(z, ptr)

	srv := dns.NewRR(dns.TypeSRV).(*dns.RR_SRV)
	srv.Hdr.Name = s.serviceFqdn()
	srv.Hdr.Class = dns.ClassINET
	srv.Hdr.Ttl = 3600
	srv.Port = s.Port
	srv.Target = s.fqdn()
	PublishRR(z, srv)

	txt := dns.NewRR(dns.TypeTXT).(*dns.RR_TXT)
	txt.Hdr.Name = s.serviceFqdn()
	txt.Hdr.Class = dns.ClassINET
	txt.Hdr.Ttl = 3600
	PublishRR(z, txt)
}

func PublishRR(z Zone, rr dns.RR) {
	z.Add(&Entry{
		Publish: true,
		RR:      rr,
	})
}

type Entry struct {
	Expires int64 // the timestamp when this record will expire in nanoseconds
	Publish bool  // whether this entry should be broadcast in response to an mDNS question
	RR      dns.RR
	Source  *net.UDPAddr
}

func (e *Entry) fqdn() string {
	return e.RR.Header().Name
}

func (e *Entry) Domain() string {
	return "local." // TODO
}

func (e *Entry) Name() string {
	return strings.Split(e.fqdn(), ".")[0]
}

func (e *Entry) Type() string {
	return e.fqdn()[len(e.Name()+".") : len(e.fqdn())-len(e.Domain())]
}

type Query struct {
	Question dns.Question
	Result   chan *Entry
}

type entries []*Entry

func (e entries) contains(entry *Entry) bool {
	for _, ee := range e {
		if equals(ee.RR, entry.RR) {
			return true
		}
	}
	return false
}

type zone struct {
	Domain        string
	entries       map[string]entries
	add           chan *Entry // add entries to zone
	query         chan *Query // query exsting entries in zone
	subscribe     chan *Query // subscribe to new entries added to zone
	subscriptions []*Query
}

type Zone interface {
	Query(dns.Question) []*Entry
	QueryAdditional(dns.Question) ([]*Entry, []*Entry)
	Subscribe(uint16) chan *Entry
	Add(*Entry)
}

func NewLocalZone() Zone {
	z := &zone{
		Domain:    "local.",
		entries:   make(map[string]entries),
		add:       make(chan *Entry, 16),
		query:     make(chan *Query, 16),
		subscribe: make(chan *Query, 16),
	}
	go z.mainloop()
	if err := z.listen(IPv4MCASTADDR); err != nil {
		log.Fatal("Failed to listen: ", err)
	}
	if err := z.listen(IPv6MCASTADDR); err != nil {
		log.Fatal("Failed to listen: ", err)
	}
	return z
}

func (z *zone) mainloop() {
	for {
		select {
		case entry := <-z.add:
			z.add0(entry)
		case q := <-z.query:
			z.query0(q)
		case q := <-z.subscribe:
			z.subscriptions = append(z.subscriptions, q)
		}
	}
}

func (z *zone) Add(e *Entry) {
	z.add <- e
}

func (z *zone) Subscribe(t uint16) chan *Entry {
	res := make(chan *Entry, 16)
	z.subscribe <- &Query{
		dns.Question{
			"",
			t,
			dns.ClassINET,
		},
		res,
	}
	return res
}

func (z *zone) Query(q dns.Question) (entries []*Entry) {
	res := make(chan *Entry, 16)
	z.query <- &Query{q, res}
	for e := range res {
		entries = append(entries, e)
	}
	return entries
}

func (z *zone) QueryAdditional(q dns.Question) ([]*Entry, []*Entry) {
	return z.Query(q), nil
}

func (z *zone) add0(entry *Entry) {
	if !z.entries[entry.fqdn()].contains(entry) {
		z.entries[entry.fqdn()] = append(z.entries[entry.fqdn()], entry)
		z.publish(entry)
	}
}

func (z *zone) publish(entry *Entry) {
	for _, c := range z.subscriptions {
		if c.matches(entry) {
			c.Result <- entry
		}
	}
}

func (z *zone) query0(query *Query) {
	for _, entry := range z.entries[query.Question.Name] {
		if query.matches(entry) {
			query.Result <- entry
		}
	}
	close(query.Result)
}

func (q *Query) matches(entry *Entry) bool {
	return q.Question.Qtype == dns.TypeANY || q.Question.Qtype == entry.RR.Header().Rrtype
}

func equals(this, that dns.RR) bool {
	if _, ok := this.(*dns.RR_ANY); ok {
		return true // *RR_ANY matches anything
	}
	if _, ok := that.(*dns.RR_ANY); ok {
		return true // *RR_ANY matches all
	}
	return false
}

const (
	seconds = 1e9
)

var (
	IPv4MCASTADDR = &net.UDPAddr{
		IP:   net.ParseIP("224.0.0.251"),
		Port: 5353,
	}

	IPv6MCASTADDR = &net.UDPAddr{
		IP:   net.ParseIP("ff02::fb"),
		Port: 5353,
	}
)

type connector struct {
	*net.UDPAddr
	*net.UDPConn
	Zone
}

func (z *zone) listen(addr *net.UDPAddr) os.Error {
	conn, err := openSocket(addr)
	if err != nil {
		return err
	}
	if err := conn.JoinGroup(nil, addr.IP); err != nil {
		return err
	}
	c := &connector{
		UDPAddr: addr,
		UDPConn: conn,
		Zone:    z,
	}
	go c.mainloop()
	return nil
}

func openSocket(addr *net.UDPAddr) (*net.UDPConn, os.Error) {
	switch addr.IP.To4() {
	case nil:
		return net.ListenUDP("udp6", &net.UDPAddr{
			IP:   net.IPv6zero,
			Port: addr.Port,
		})
	default:
		return net.ListenUDP("udp4", &net.UDPAddr{
			IP:   net.IPv4zero,
			Port: addr.Port,
		})
	}
	panic("unreachable")
}

func (c *connector) mainloop() {
	type incoming struct {
		*dns.Msg
		*net.UDPAddr
	}
	in := make(chan incoming, 32)
	go func() {
		for {
			msg, addr, err := c.readMessage()
			if err != nil {
				log.Fatalf("Cound not read from %s: %s", c.UDPConn, err)
			}
			in <- incoming{msg, addr}
		}
	}()

	for {
		select {
		case msg := <-in:
			if msg.IsQuestion() {
				r := new(dns.Msg)
				r.MsgHdr.Response = true
				results, additionals := c.query(msg.Question)
				for _, result := range results {
					if result.Publish {
						r.Answer = append(r.Answer, result.RR)
					}
				}
				for _, additional := range additionals {
					if additional.Publish {
						r.Extra = append(r.Extra, additional.RR)
					}
				}
				if len(r.Answer) > 0 {
					r.Extra = c.findAdditional(r.Answer)
					fmt.Println(r)
					if err := c.writeMessage(r); err != nil {
						log.Fatalf("Cannot send: %s", err)
					}

				}
			} else {
				for _, rr := range msg.Answer {
					c.Add(&Entry{
						Expires: time.Nanoseconds() + int64(rr.Header().Ttl*seconds),
						Publish: false,
						RR:      rr,
						Source:  msg.UDPAddr,
					})
				}
			}
		}
	}
}

func (c *connector) findAdditional(rr []dns.RR) []dns.RR {
	return []dns.RR{}
}

func (c *connector) query(qs []dns.Question) (results []*Entry, additionals []*Entry) {
	for _, q := range qs {
		result, additional := c.QueryAdditional(q)
		results = append(results, result...)
		additionals = append(additionals, additional...)
	}
	return
}

func (c *connector) writeMessage(msg *dns.Msg) (err os.Error) {
	if buf, ok := msg.Pack(); ok {
		_, err = c.WriteToUDP(buf, c.UDPAddr)
	}
	return
}

func (c *connector) readMessage() (*dns.Msg, *net.UDPAddr, os.Error) {
	buf := make([]byte, 1500)
	read, addr, err := c.ReadFromUDP(buf)
	if err != nil {
		return nil, nil, err
	}
	if msg := new(dns.Msg); msg.Unpack(buf[:read]) {
		return msg, addr, nil
	}
	return nil, addr, os.NewError("Unable to unpack buffer")
}
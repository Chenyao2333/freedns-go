package freedns

import (
	"strings"
	"time"

	goc "github.com/louchenyao/golang-cache"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
	"github.com/tuna/freedns-go/chinaip"
)

type Config struct {
	FastDNS   string
	CleanDNS  string
	Listen    string
	CacheSize int
}

type Server struct {
	config Config

	udp_server *dns.Server
	tcp_server *dns.Server

	chinaDom *goc.Cache
	cache    *goc.Cache
}

type Error string

var log = logrus.New()

func (e Error) Error() string {
	return string(e)
}

// append the 53 port number after the ip, if the ip does not has ip infomation.
// It only works for IPv4 addresses, since it's a little hard to check if a port
// is in the IPv6 string representation.
func append_default_port(ip string) string {
	if strings.Contains(ip, ".") && !strings.Contains(ip, ":") {
		return ip + ":53"
	}
	return ip
}

func NewServer(cfg Config) (*Server, error) {
	s := &Server{}

	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1"
	}
	cfg.Listen = append_default_port(cfg.Listen)
	cfg.FastDNS = append_default_port(cfg.FastDNS)
	cfg.CleanDNS = append_default_port(cfg.CleanDNS)
	s.config = cfg

	s.udp_server = &dns.Server{
		Addr: s.config.Listen,
		Net:  "udp",
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
			s.handle(w, req, "udp")
		}),
	}

	s.tcp_server = &dns.Server{
		Addr: s.config.Listen,
		Net:  "tcp",
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
			s.handle(w, req, "tcp")
		}),
	}

	var err error
	s.chinaDom, err = goc.NewCache("lru", cfg.CacheSize)
	if err != nil {
		log.Fatalln(err)
	}

	s.cache, err = goc.NewCache("lru", cfg.CacheSize)
	if err != nil {
		log.Fatalln(err)
	}

	return s, nil
}

// Run tcp and udp server.
func (s *Server) Run() error {
	errChan := make(chan error, 2)

	go func() {
		err := s.tcp_server.ListenAndServe()
		errChan <- err
	}()

	go func() {
		err := s.udp_server.ListenAndServe()
		errChan <- err
	}()

	select {
	case err := <-errChan:
		s.tcp_server.Shutdown()
		s.udp_server.Shutdown()
		return err
	}
}

func (s *Server) Shutdown() {
	s.tcp_server.Shutdown()
	s.udp_server.Shutdown()
}

func (s *Server) handle(w dns.ResponseWriter, req *dns.Msg, net string) {
	res := &dns.Msg{}
	hit := false
	var err error

	if len(req.Question) < 1 {
		res.SetRcode(req, dns.RcodeBadName)
		w.WriteMsg(res)
		log.WithFields(logrus.Fields{
			"op":     "handle_request",
			"domain": "[request without questions]",
			"status": dns.RcodeToString[res.Rcode],
		}).Warn()
		return
	}

	qname := req.Question[0].Name
	upstream := ""
	if res, hit = s.LookupHosts(req); hit {
		upstream = "hosts"
	} else if res, hit = s.LookupCache(req, net); hit {
		upstream = "cache"
	} else {
		upstream = "net"
		res, upstream, err = s.LookupNet(req, net)
		if err != nil {
			log.Error(err)
		}
		if res == nil {
			res = &dns.Msg{}
			res.SetRcode(req, dns.RcodeServerFailure)
		}
	}
	res.SetRcode(req, res.Rcode)
	w.WriteMsg(res)

	// logging
	l := log.WithFields(logrus.Fields{
		"op":       "handle_request",
		"domain":   qname,
		"type":     dns.TypeToString[req.Question[0].Qtype],
		"upstream": upstream,
		"status":   dns.RcodeToString[res.Rcode],
	})
	if res.Rcode == dns.RcodeSuccess {
		l.Info()
	} else {
		l.Warn()
	}
}

// LookupNet resolve the the dns request through net.
// The first return value is answer,iff it's nil means failed in resolving.
// Due to implementation, now the error will always be nil,
// but don't do this assumpation in your code.
func (s *Server) LookupNet(req *dns.Msg, net string) (*dns.Msg, string, error) {
	fastCh := make(chan *dns.Msg, 10)
	cleanCh := make(chan *dns.Msg, 10)

	Q := func(ch chan *dns.Msg, useClean bool) {
		upstream := s.config.FastDNS
		if useClean {
			upstream = s.config.CleanDNS
		}

		res, err := resolve(req, upstream, net)

		if err != nil {
			log.WithFields(logrus.Fields{
				"op":       "Resolve",
				"upstream": upstream,
				"domain":   req.Question[0].Name,
			}).Error(err)
		}

		if res == nil {
			ch <- nil
			return
		}

		// if it's fastDNS upstream and maybe polluted, just return serverFailure
		if !useClean && (res.Rcode != dns.RcodeSuccess || err != nil || s.maybePolluted(res)) {
			ch <- nil
			return
		}

		ch <- res
	}

	go Q(cleanCh, true)
	go Q(fastCh, false)

	// ensure ch must will receive nil after timeout
	go func() {
		time.Sleep(2 * time.Second)
		fastCh <- nil
		cleanCh <- nil
	}()

	// first try to resolve by fastDNS
	res := <-fastCh
	if res != nil {
		s.setCache(res, net)
		return res, s.config.FastDNS, nil
	}

	// if fastDNS failed, just return result of cleanDNS
	res = <-cleanCh
	if res == nil {
		res = &dns.Msg{}
		res.SetRcode(req, dns.RcodeServerFailure)
	}
	if res.Rcode == dns.RcodeSuccess {
		s.setCache(res, net)
	}
	return res, s.config.CleanDNS, nil
}

func resolve(req *dns.Msg, upstream string, net string) (*dns.Msg, error) {
	r := req.Copy()
	r.Id = dns.Id()

	c := &dns.Client{Net: net}

	res, _, err := c.Exchange(r, upstream)

	return res, err
}

func (s *Server) maybePolluted(res *dns.Msg) bool {
	// not contain any valid response
	if len(res.Answer)+len(res.Ns)+len(res.Extra) == 0 {
		return true
	}

	// contain A; If it's none China IP, it maybe polluted
	if containA(res) {
		china := containChinaIP(res)
		s.chinaDom.Set(res.Question[0].Name, china)
		return !china
	}

	// not sure, but it's not China domain
	china, ok := s.chinaDom.Get(res.Question[0].Name)
	if ok {
		return !china.(bool)
	}

	// otherwith, it's trustable response
	return false
}

func containA(res *dns.Msg) bool {
	var rrs []dns.RR

	rrs = append(rrs, res.Answer...)
	rrs = append(rrs, res.Ns...)
	rrs = append(rrs, res.Extra...)

	for i := 0; i < len(rrs); i++ {
		_, ok := rrs[i].(*dns.A)
		if ok {
			return true
		}
	}
	return false
}

// containChinaIP judge answers whether contains IP belong to China.
func containChinaIP(res *dns.Msg) bool {
	var rrs []dns.RR

	rrs = append(rrs, res.Answer...)
	rrs = append(rrs, res.Ns...)
	rrs = append(rrs, res.Extra...)

	for i := 0; i < len(rrs); i++ {
		rr, ok := rrs[i].(*dns.A)
		if ok {
			ip := rr.A.String()
			if chinaip.IsChinaIP(ip) {
				return true
			}
		}
	}
	return false
}

func genCacheKey(r *dns.Msg, net string) string {
	q := r.Question[0]
	s := q.Name + "_" + dns.TypeToString[q.Qtype]
	if r.RecursionDesired {
		s += "_1"
	} else {
		s += "_0"
	}
	s += "_" + net
	return s
}

type cacheEntry struct {
	putin time.Time
	reply *dns.Msg
}

func subTTL(res *dns.Msg, delta int) bool {
	needUpdate := false
	S := func(rr []dns.RR) {
		for i := 0; i < len(rr); i++ {
			newTTL := int(rr[i].Header().Ttl)
			newTTL -= delta

			if newTTL <= 0 {
				newTTL = 3
				needUpdate = true
			}

			rr[i].Header().Ttl = uint32(newTTL)
		}
	}

	S(res.Answer)
	S(res.Ns)
	S(res.Extra)

	return needUpdate
}

func (s *Server) LookupCache(req *dns.Msg, net string) (*dns.Msg, bool) {
	key := genCacheKey(req, net)
	ci, ok := s.cache.Get(key)

	if ok {
		c := ci.(cacheEntry)
		delta := time.Now().Sub(c.putin).Seconds()

		r := c.reply.Copy()
		needUpdate := subTTL(r, int(delta))
		if needUpdate {
			go func() {
				res, upstream, _ := s.LookupNet(req, net)

				l := log.WithFields(logrus.Fields{
					"op":       "LookupCache-LookupNet",
					"domain":   req.Question[0].Name,
					"type":     dns.TypeToString[req.Question[0].Qtype],
					"upstream": upstream,
					"status":   dns.RcodeToString[res.Rcode],
				})

				if res.Rcode != dns.RcodeSuccess {
					l.Warn()
				} else {
					l.Info()
				}
			}()
		}

		return r, true
	}

	return nil, false
}

func (s *Server) setCache(res *dns.Msg, net string) {
	key := genCacheKey(res, net)

	s.cache.Set(key, cacheEntry{
		putin: time.Now(),
		reply: res,
	})
}

func (s *Server) LookupHosts(req *dns.Msg) (*dns.Msg, bool) {
	// TODO: implement needed
	return nil, false
}

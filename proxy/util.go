package proxy

import (
	"github.com/coyove/goflyway/pkg/logg"

	"crypto/tls"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	socksVersion5   = byte(0x05)
	socksAddrIPv4   = 1
	socksAddrDomain = 3
	socksAddrIPv6   = 4
	socksReadErr    = "cannot read buffer: "
	socksVersionErr = "invalid socks version (SOCKS5 only)"
)

const (
	doConnect = 1
	doForward = 1 << 1
	doRSV5    = 1 << 2
	doRSV1    = 1 << 3
	doDNS     = 1 << 4
	doRSV2    = 1 << 5
	doRSV3    = 1 << 6
	doRSV4    = 1 << 7
)

const (
	timeoutUDP          = 30
	timeoutTCP          = 60
	invalidRequestRetry = 2
)

var (
	okHTTP          = []byte{'H', 'T', 'T', 'P', '/', '1', '.', '1', ' ', '2', '0', '0', ' ', 'O', 'K', '\r', '\n', '\r', '\n'}
	okSOCKS         = []byte{socksVersion5, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	udpHeaderIPv4   = []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	udpHeaderIPv6   = []byte{0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	dummyHeaders    = []string{"Accept-Language", "User-Agent", "Referer", "Cache-Control", "Accept-Encoding", "Connection"}
	tlsSkip         = &tls.Config{InsecureSkipVerify: true}
	hasPort         = regexp.MustCompile(`:\d+$`)
	isHTTPSSchema   = regexp.MustCompile(`^https:\/\/`)
	base32Encoding  = base32.NewEncoding("0123456789abcdefghiklmnoprstuwxy")
	base32Encoding2 = base32.NewEncoding("abcd&fghijklmnopqrstuvwxyz+-_./e")
)

func (proxy *ProxyClient) addToDummies(req *http.Request) {
	for _, field := range dummyHeaders {
		if x := req.Header.Get(field); x != "" {
			proxy.dummies.Add(field, x)
		}
	}
}

func (proxy *ProxyClient) genHost() string {
	const tlds = ".com.net.org"
	if proxy.DummyDomain == "" {
		i := proxy.Rand.Intn(3) * 4
		return genWord(proxy.Cipher, true) + tlds[i:i+4]
	}

	return proxy.DummyDomain
}

func (proxy *ProxyClient) encryptAndTransport(req *http.Request, auth string) (*http.Response, []byte, error) {
	rkey, rkeybuf := proxy.Cipher.NewIV(doForward, nil, auth)
	req.Header.Add(proxy.rkeyHeader, rkey)

	proxy.addToDummies(req)

	if proxy.DummyDomain2 != "" {
		req.Header.Add("X-Forwarded-Url", "http://"+proxy.genHost()+"/"+proxy.Cipher.EncryptCompress(req.URL.String(), rkeybuf...))
		req.Host = proxy.DummyDomain2
		req.URL, _ = url.Parse("http://" + proxy.DummyDomain2)
	} else {
		req.Host = proxy.genHost()
		req.URL, _ = url.Parse("http://" + req.Host + "/" + proxy.Cipher.EncryptCompress(req.URL.String(), rkeybuf...))
	}

	cookies := []string{}
	for _, c := range req.Cookies() {
		c.Value = proxy.Cipher.EncryptString(c.Value, rkeybuf...)
		cookies = append(cookies, c.String())
	}

	req.Header.Set("Cookie", strings.Join(cookies, ";"))
	if origin := req.Header.Get("Origin"); origin != "" {
		req.Header.Set("Origin", proxy.EncryptString(origin, rkeybuf...)+".com")
	}

	if referer := req.Header.Get("Referer"); referer != "" {
		req.Header.Set("Referer", proxy.EncryptString(referer, rkeybuf...))
	}

	req.Body = proxy.Cipher.IO.NewReadCloser(req.Body, rkeybuf)
	// logg.D(req.Header)
	resp, err := proxy.tp.RoundTrip(req)
	return resp, rkeybuf, err
}

func stripURI(uri string) string {
	if len(uri) < 1 {
		return uri
	}

	if uri[0] != '/' {
		idx := strings.Index(uri[8:], "/")
		if idx > -1 {
			uri = uri[idx+1+8:]
		} else {
			logg.W("unexpected uri: ", uri)
		}
	} else {
		uri = uri[1:]
	}

	return uri
}

func (proxy *ProxyUpstream) decryptRequest(req *http.Request, options byte, rkeybuf []byte) bool {
	var err error
	req.URL, err = url.Parse(proxy.Cipher.DecryptDecompress(stripURI(req.RequestURI), rkeybuf...))
	if err != nil {
		logg.E(err)
		return false
	}

	req.Host = req.URL.Host

	cookies := []string{}
	for _, c := range req.Cookies() {
		c.Value = proxy.Cipher.DecryptString(c.Value, rkeybuf...)
		cookies = append(cookies, c.String())
	}
	req.Header.Set("Cookie", strings.Join(cookies, ";"))

	if origin := req.Header.Get("Origin"); origin != "" {
		req.Header.Set("Origin", proxy.DecryptString(origin[:len(origin)-4], rkeybuf...))
	}

	if referer := req.Header.Get("Referer"); referer != "" {
		req.Header.Set("Referer", proxy.DecryptString(referer, rkeybuf...))
	}

	for k := range req.Header {
		if k[:3] == "Cf-" || (len(k) > 12 && strings.ToLower(k[:12]) == "x-forwarded-") {
			// ignore all cloudflare headers
			// this is needed when you use cf as the frontend:
			// gofw client -> cloudflare -> gofw server -> target host using cloudflare

			// delete all x-forwarded-... headers
			// some websites won't allow them
			req.Header.Del(k)
		}
	}

	req.Body = proxy.Cipher.IO.NewReadCloser(req.Body, rkeybuf)
	return true
}

func copyHeaders(dst, src http.Header, gc *Cipher, enc bool, rkeybuf []byte) {
	for k := range dst {
		dst.Del(k)
	}

	for k, vs := range src {
	READ:
		for _, v := range vs {
			cip := func(ei, di int) {
				if rkeybuf != nil {
					if enc {
						v = v[:ei] + "=" + gc.EncryptString(v[ei+1:di], rkeybuf...) + ";" + v[di+1:]
					} else {
						v = v[:ei] + "=" + gc.DecryptString(v[ei+1:di], rkeybuf...) + ";" + v[di+1:]
					}
				}

				// rkeybuf is nil, so do nothing
			}

			switch strings.ToLower(k) {
			case "set-cookie":
				ei, di := strings.Index(v, "="), strings.Index(v, ";")

				if ei > -1 && di > ei {
					cip(ei, di)
				}

				ei = strings.Index(v, "main=") // [Dd]omain
				if ei > -1 {
					for di = ei + 5; di < len(v); di++ {
						if v[di] == ';' {
							cip(ei+4, di)
							break
						}
					}
				}
			case "content-encoding", "content-type":
				if enc {
					dst.Add("X-"+k, v)
					continue READ
				} else if rkeybuf != nil {
					continue READ
				}

				// rkeybuf is nil and we are in decrypt mode
				// aka plain copy mode, so fall to the bottom
			case "x-content-encoding", "x-content-type":
				if !enc {
					dst.Add(k[2:], v)
					continue READ
				}
			}

			dst.Add(k, v)
		}
	}
}

func (proxy *ProxyClient) basicAuth(token string) string {
	parts := strings.Split(token, " ")
	if len(parts) != 2 {
		return ""
	}

	pa, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		return ""
	}

	if s := string(pa); s == proxy.UserAuth {
		return s
	}

	return ""
}

func tryClose(b io.ReadCloser) {
	if err := b.Close(); err != nil {
		logg.W("can't close: ", err)
	}
}

func splitHostPort(host string) (string, string) {
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		idx2 := strings.LastIndex(host, "]")
		if idx2 < idx {
			return strings.ToLower(host[:idx]), host[idx:]
		}

		// ipv6 without port
	}

	return strings.ToLower(host), ""
}

func isTrustedToken(mark string, rkeybuf []byte) int {
	logg.D("test token: ", rkeybuf)

	if string(rkeybuf[:len(mark)]) != mark {
		return 0
	}

	sent := int64(binary.BigEndian.Uint32(rkeybuf[12:]))
	if time.Now().Unix()-sent >= 10 {
		// token becomes invalid after 10 seconds
		return -1
	}

	return 1
}

func genTrustedToken(mark, auth string, gc *Cipher) string {
	buf := make([]byte, ivLen)

	copy(buf, []byte(mark))
	binary.BigEndian.PutUint32(buf[ivLen-4:], uint32(time.Now().Unix()))

	k, _ := gc.NewIV(0, buf, auth)
	return k
}

func base32Encode(buf []byte, alpha bool) string {
	var str string
	if alpha {
		str = base32Encoding.EncodeToString(buf)
	} else {
		str = base32Encoding2.EncodeToString(buf)
	}
	idx := strings.Index(str, "=")

	if idx == -1 {
		return str
	}

	return str[:idx]
}

func base32Decode(text string, alpha bool) ([]byte, error) {
	const paddings = "======"

	if m := len(text) % 8; m > 1 {
		text = text + paddings[:8-m]
	}

	if alpha {
		return base32Encoding.DecodeString(text)
	}

	return base32Encoding2.DecodeString(text)
}

func genWord(gc *Cipher, random bool) string {
	const (
		vowels = "aeiou"
		cons   = "bcdfghlmnprst"
	)

	ret := make([]byte, 16)
	i, ln := 0, 0

	if random {
		ret[0] = (vowels + cons)[gc.Rand.Intn(18)]
		i, ln = 1, gc.Rand.Intn(6)+3
	} else {
		gc.Block.Encrypt(ret, gc.Key)
		ret[0] = (vowels + cons)[ret[0]/15]
		i, ln = 1, int(ret[15]/85)+6
	}

	link := func(prev string, this string, thisidx byte) {
		if strings.ContainsRune(prev, rune(ret[i-1])) {
			if random {
				ret[i] = this[gc.Rand.Intn(len(this))]
			} else {
				ret[i] = this[ret[i]/thisidx]
			}

			i++
		}
	}

	for i < ln {
		link(vowels, cons, 20)
		link(cons, vowels, 52)
		link(vowels, cons, 20)
		link(cons, vowels+"tr", 37)
	}

	if !random {
		ret[0] -= 32
	}

	return string(ret[:ln])
}

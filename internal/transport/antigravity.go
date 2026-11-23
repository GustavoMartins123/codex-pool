package transport

import (
	"crypto/tls"
	"net/http"
)

func CloneAntigravityHTTP11(base *http.Transport) *http.Transport {
	if base == nil {
		return nil
	}
	clone := base.Clone()
	clone.ForceAttemptHTTP2 = false
	clone.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
	}
	clone.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return clone
}

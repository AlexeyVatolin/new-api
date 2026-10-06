package service

import (
	"errors"
	"net/http"
	"sync"

	"github.com/QuantumNous/new-api/relaykit/dto"
)

var rawHTTPClients sync.Map

// RawHTTPClient reuses the channel proxy/policy, but does not consume redirects
// or silently decode gzip. A separate cached transport keeps other relays intact.
func RawHTTPClient(proxy string, settings dto.ChannelSettings) (*http.Client, error) {
	base, err := GetHttpClientWithProxySettings(proxy, settings)
	if err != nil {
		return nil, err
	}
	if cached, ok := rawHTTPClients.Load(base); ok {
		return cached.(*http.Client), nil
	}
	transport, ok := base.Transport.(*http.Transport)
	if !ok {
		return nil, errors.New("raw transport is unavailable")
	}
	clone := transport.Clone()
	clone.DisableCompression = true
	client := &http.Client{Transport: clone, Timeout: base.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cached, loaded := rawHTTPClients.LoadOrStore(base, client)
	if loaded {
		clone.CloseIdleConnections()
	}
	return cached.(*http.Client), nil
}

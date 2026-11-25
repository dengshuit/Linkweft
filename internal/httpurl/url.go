package httpurl

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Parse returns nil unless raw is an absolute HTTP(S) URL with a valid host and port.
func Parse(raw string) *url.URL {
	address, err := url.Parse(raw)
	if err != nil || (address.Scheme != "http" && address.Scheme != "https") {
		return nil
	}
	hostname := address.Hostname()
	if hostname == "" {
		return nil
	}
	if strings.HasPrefix(address.Host, "[") {
		ip, err := netip.ParseAddr(hostname)
		if err != nil || !ip.Is6() {
			return nil
		}
	} else if strings.ContainsAny(hostname, "[]:") {
		return nil
	}
	if port := address.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil
		}
	}
	return address
}

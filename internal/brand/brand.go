// Package brand holds the public product identity the platform advertises in
// response headers, for both the API and proxied apps.
package brand

import "net/http"

// Name is the public product name the platform advertises in the Server and
// X-Powered-By headers, overriding whatever the API or a proxied app would
// otherwise report.
const Name = "Dynaserve"

// Stamp sets the branding headers (Server, X-Powered-By, X-Region) on h.
func Stamp(h http.Header, region string) {
	h.Set("X-Powered-By", Name)
	h.Set("Server", Name)
	h.Set("X-Region", region)
}

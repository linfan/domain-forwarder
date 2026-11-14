package proxy

import "net/http"

// applyRequestEdits applies the configured request edits in place. It runs
// after add_proxy_headers and before forwarding, so an added header overwrites
// a same-name header from the client or from add_proxy_headers.
func applyRequestEdits(r *http.Request, addHeaders map[string]string) {
	for name, value := range addHeaders {
		r.Header.Set(name, value)
	}
}

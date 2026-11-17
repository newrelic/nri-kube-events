package objects

import (
	"fmt"
	"net/http"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ReadOnlyTransport wraps rt so that every request other than GET fails before it leaves
// the machine. The command-line tools that read a cluster only list and watch; this makes a
// write impossible even if a code path tried one.
func ReadOnlyTransport(rt http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, fmt.Errorf("read-only client: refused %s %s", r.Method, r.URL.Path)
		}
		return rt.RoundTrip(r)
	})
}

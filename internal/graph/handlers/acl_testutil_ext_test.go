package handlers_test

import (
	"io"
	"net/http"
	"net/http/httptest"
)

// adminRequest builds a request carrying the admin header: the unfiltered
// view that tests of non-ACL behaviour use.
func adminRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("X-Graph-Admin", "1")
	return r
}

// setAsker names a filtered asker on r, dropping any admin header.
func setAsker(r *http.Request, ref string) {
	r.Header.Del("X-Graph-Admin")
	r.Header.Set("X-Asker-User", ref)
}

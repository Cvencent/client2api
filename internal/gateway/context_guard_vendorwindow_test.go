package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// TestGuardTrimsAgainstTheVendorsOwnAdvertisedWindow is the case that matters in
// production and the one a seeded learned limit would otherwise hide.
//
// Nothing has been learned here.  The only thing that tells the gateway how big
// this model is, is the number the module publishes in its catalogue Extra --
// which is also the number /v1/models serves and the number the client sizes its
// packing from.  If the guard cannot see it, it trims nothing and every
// oversized request is still refused.
func TestGuardTrimsAgainstTheVendorsOwnAdvertisedWindow(t *testing.T) {
	// The vendor advertises 100k and enforces it.  No learned limit exists.
	c := &catalogueClient{name: "workbuddy", window: 100000}
	s := newGuardServer(t, c, "")
	if got := s.learnedWindows.get("workbuddy", "cn:space-bunny"); got != 0 {
		t.Fatalf("this test must start with nothing learned, got %d", got)
	}

	body := strings.Replace(oversizedBody(80, 4000), `"bunny/cn:space-bunny"`, `"workbuddy/cn:space-bunny"`, 1)
	rr := chatHandler(t, s, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("the guard must trim against the vendor's own window, got %d: %s",
			rr.Code, rr.Body.String())
	}
}

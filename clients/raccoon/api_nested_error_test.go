package raccoon

import (
	"net/http"
	"strings"
	"testing"
)

// The vendor answers some refusals with a NESTED error object
// (`{"error":{"message":...}}`) instead of the universal envelope. Before this
// test the message was dropped and every such failure surfaced as the useless
// `raccoon: HTTP 400: Bad Request`, which made a schema refusal indistinguishable
// from a context refusal.
func TestDecodeEnvelopeReadsNestedErrorMessage(t *testing.T) {
	raw := []byte(`{"error":{"code":"400","message":"Invalid schema for function 'x': \"^[^=\\0]+$\" is not a \"regex\"","param":null,"type":"invalid_request_error"}}`)

	_, err := decodeEnvelope(http.StatusBadRequest, raw)
	if err == nil {
		t.Fatal("a nested error payload must produce an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Invalid schema for function 'x'") {
		t.Fatalf("error = %q, want it to carry the vendor's nested message", msg)
	}
	if strings.Contains(msg, "Bad Request") {
		t.Fatalf("error = %q, must not fall back to the bare HTTP status text", msg)
	}
}

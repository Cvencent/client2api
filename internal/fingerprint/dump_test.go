package fingerprint

import (
	"encoding/hex"
	"testing"
)

func TestDumpBunHelloHex(t *testing.T) {
	body := captureClientHello(t, Options{Profile: ProfileBun, Protocol: ProtocolH1})
	t.Logf("full hello hex:\n%s", hex.Dump(body))
}

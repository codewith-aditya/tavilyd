package extract

import (
	"testing"

	utls "github.com/refraction-networking/utls"
)

// TestChromeHTTP1Spec guards the ALPN rewrite: the Chrome preset advertises h2,
// which breaks the HTTP/1.1 parser used by tier 1 (servers answered with h2
// frames before this fix).
func TestChromeHTTP1Spec(t *testing.T) {
	spec, err := chromeHTTP1Spec()
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	var got []string
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			got = alpn.AlpnProtocols
		}
	}
	if len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("ALPN protocols = %v, want [http/1.1]", got)
	}
}

package shell

import (
	"os/exec"
	"testing"
)

func TestQuoteRoundTrip(t *testing.T) {
	for _, s := range []string{"plain", "with space", "it's", `$(id) "x" \n`, ""} {
		out, err := exec.Command("bash", "-c", "printf %s "+Quote(s)).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != s {
			t.Errorf("Quote(%q) round-tripped to %q", s, out)
		}
	}
}

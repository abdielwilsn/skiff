package dotenv

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := `# comment
; also a comment
APP_NAME=My App
  export APP_ENV=production
APP_URL = https://x.test
QUOTED="a \"b\" \$HOME\nnext"
SINGLE='raw $HOME \n'
MULTI="line one
line two"
CONT=abc\
def
not a line
1BAD=x
EMPTY=
TRAIL=value   
`
	got, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{"APP_NAME", "My App"}, {"APP_ENV", "production"}, {"APP_URL", "https://x.test"},
		{"QUOTED", "a \"b\" $HOME\nnext"}, {"SINGLE", `raw $HOME \n`}, {"MULTI", "line one\nline two"},
		{"CONT", "abcdef"}, {"EMPTY", ""}, {"TRAIL", "value"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %q, want %q", i, got[i], want[i])
		}
	}
}

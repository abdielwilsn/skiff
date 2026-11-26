// Package dotenv parses .env files the way systemd's EnvironmentFile= does,
// so apps see the same variables they did when systemd read the file.
package dotenv

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse returns the KEY=VALUE assignments in r, in order. Blank lines and
// lines starting with # or ; are skipped, an "export " prefix is allowed,
// single quotes are literal, double quotes understand \" \\ \$ \` and \n, and
// a quoted value may span lines. Unquoted values are trimmed, and a trailing
// backslash continues them on the next line. Invalid lines are ignored.
func Parse(r io.Reader) ([][2]string, error) {
	var out [][2]string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimLeft(lines[i], " \t")
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		rest := strings.TrimLeft(line[eq+1:], " \t")
		if !keyRe.MatchString(key) {
			continue
		}
		var val string
		switch {
		case strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, `'`):
			q := rest[0]
			body := rest[1:]
			for {
				if end := closingQuote(body, q); end >= 0 {
					body = body[:end]
					break
				}
				if i+1 >= len(lines) { // unterminated: take what's there
					break
				}
				i++
				body += "\n" + lines[i]
			}
			if q == '"' {
				body = unescape(body)
			}
			val = body
		default:
			for strings.HasSuffix(rest, `\`) && i+1 < len(lines) {
				i++
				rest = strings.TrimSuffix(rest, `\`) + lines[i]
			}
			val = strings.TrimRight(rest, " \t")
		}
		out = append(out, [2]string{key, val})
	}
	return out, nil
}

// closingQuote finds the closing quote in s, skipping \" in double quotes.
func closingQuote(s string, q byte) int {
	for i := 0; i < len(s); i++ {
		if q == '"' && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q {
			return i
		}
	}
	return -1
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\\', '$', '`':
				b.WriteByte(s[i+1])
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

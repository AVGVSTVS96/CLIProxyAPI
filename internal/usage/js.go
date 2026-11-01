package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Record ids hash JSON.stringify output and parse timestamps the way Bun does, so these
// helpers reproduce JavaScript's string, number and date behavior exactly.

func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }

// jsLength is a string's length in UTF-16 code units.
func jsLength(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func jsString(b *strings.Builder, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[r>>4])
				b.WriteByte(hexDigits[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// jsNumber formats a float64 as Number.prototype.toString does.
func jsNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exp, _ := strings.Cut(e, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	k := len(digits)
	n, _ := strconv.Atoi(exp)
	n++
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	expSign := "+"
	if n-1 < 0 {
		expSign = "-"
	}
	frac := ""
	if k > 1 {
		frac = "." + digits[1:]
	}
	return sign + digits[:1] + frac + "e" + expSign + strconv.Itoa(abs(n-1))
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// jsonArray renders JSON.stringify of an array of strings, numbers and ints.
func jsonArray(values ...any) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		switch v := v.(type) {
		case string:
			jsString(&b, v)
		case int:
			b.WriteString(strconv.Itoa(v))
		case int64:
			b.WriteString(strconv.FormatInt(v, 10))
		case float64:
			b.WriteString(jsNumber(v))
		}
	}
	b.WriteByte(']')
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// isoMillis renders Date.prototype.toISOString, or "" where JavaScript's Date is invalid
// or the value is not positive (the parsers' own rule).
func isoMillis(ms float64) string {
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms <= 0 || ms > 8.64e15 {
		return ""
	}
	t := time.UnixMilli(int64(ms)).UTC()
	year := t.Year()
	rest := t.Format("-01-02T15:04:05.000Z")
	if year >= 0 && year <= 9999 {
		return pad(year, 4) + rest
	}
	sign := "+"
	if year < 0 {
		sign, year = "-", -year
	}
	return sign + pad(year, 6) + rest
}

func pad(n, width int) string {
	s := strconv.Itoa(n)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// parseDate is Bun's Date.parse for ISO-like strings: fractional seconds truncate to
// milliseconds, out-of-range days roll over, hour 24 is allowed, and a date-time without
// an offset is local time. Anything else is NaN.
func parseDate(s string) float64 {
	p := dateParser{s: s}
	if ms, ok := p.parse(); ok {
		return ms
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixMilli())
		}
	}
	return math.NaN()
}

type dateParser struct {
	s string
	i int
}

func (p *dateParser) digits(n int) (int, bool) {
	if p.i+n > len(p.s) {
		return 0, false
	}
	v := 0
	for _, c := range []byte(p.s[p.i : p.i+n]) {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	p.i += n
	return v, true
}

func (p *dateParser) peek(chars string) bool {
	return p.i < len(p.s) && strings.IndexByte(chars, p.s[p.i]) >= 0
}

func (p *dateParser) parse() (float64, bool) {
	year, ok := 0, false
	if p.peek("+-") {
		neg := p.s[p.i] == '-'
		p.i++
		if year, ok = p.digits(6); !ok {
			return 0, false
		}
		if neg {
			year = -year
		}
	} else if year, ok = p.digits(4); !ok {
		return 0, false
	}
	month, day := 1, 1
	if p.peek("-") {
		p.i++
		if month, ok = p.digits(2); !ok {
			return 0, false
		}
		if p.peek("-") {
			p.i++
			if day, ok = p.digits(2); !ok {
				return 0, false
			}
		}
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return 0, false
	}
	hour, minute, second, millis := 0, 0, 0, 0
	hasTime := p.peek("Tt ")
	local := false
	if hasTime {
		p.i++
		if hour, ok = p.digits(2); !ok || !p.peek(":") {
			return 0, false
		}
		p.i++
		if minute, ok = p.digits(2); !ok {
			return 0, false
		}
		if p.peek(":") {
			p.i++
			if second, ok = p.digits(2); !ok {
				return 0, false
			}
			if p.peek(".") {
				p.i++
				start := p.i
				for p.peek("0123456789") {
					p.i++
				}
				frac := p.s[start:p.i]
				if frac == "" {
					return 0, false
				}
				frac = (frac + "00")[:3]
				millis, _ = strconv.Atoi(frac)
			}
		}
		if hour > 24 || minute > 59 || second > 59 || (hour == 24 && (minute > 0 || second > 0 || millis > 0)) {
			return 0, false
		}
		local = p.i == len(p.s)
	}
	offset := 0
	if !local && p.i < len(p.s) {
		if !hasTime {
			return 0, false
		}
		switch {
		case p.peek("Zz"):
			p.i++
		case p.peek("+-"):
			sign := 1
			if p.s[p.i] == '-' {
				sign = -1
			}
			p.i++
			oh, ok := p.digits(2)
			if !ok {
				return 0, false
			}
			if p.peek(":") {
				p.i++
			}
			om, ok := p.digits(2)
			if !ok || oh > 23 || om > 59 {
				return 0, false
			}
			offset = sign * (oh*60 + om)
		default:
			return 0, false
		}
		if p.i != len(p.s) {
			return 0, false
		}
	}
	loc := time.UTC
	if local {
		loc = time.Local
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, millis*int(time.Millisecond), loc)
	return float64(t.UnixMilli() - int64(offset)*60_000), true
}

// validUTF8 replaces invalid bytes with U+FFFD, as TextDecoder does when reading logs.
func validUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	out := make([]byte, 0, len(b)+8)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out = append(out, "�"...)
		} else {
			out = append(out, b[:size]...)
		}
		b = b[size:]
	}
	return out
}

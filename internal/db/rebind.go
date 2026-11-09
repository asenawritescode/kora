package db

import (
	"strings"
)

// Rebind converts a dialect-neutral '?'-placeholder query into the
// placeholder style required by the underlying driver. MySQL and LibSQL use
// '?' natively; PostgreSQL (lib/pq) requires ordered '$n' markers.
//
// This is the transitional bridge for packages that compose raw SQL while
// DB-001 migrates the ORM to dialect.Placeholder() everywhere. Quoted string
// literals containing '?' are preserved.
func Rebind(d QueryDialect, query string) string {
	if _, isPG := d.(*PostgresDialect); !isPG {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inSingle := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			if inSingle && i+1 < len(query) && query[i+1] == '\'' {
				// An escaped quote inside a string doesn't end the literal.
				b.WriteByte(c)
				b.WriteByte('\'')
				i++
				continue
			}
			inSingle = !inSingle
			b.WriteByte(c)
		case c == '?' && !inSingle:
			n++
			b.WriteByte('$')
			b.WriteString(itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

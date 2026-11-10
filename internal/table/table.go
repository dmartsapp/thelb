// Package table renders rows the way a SQL client shows a result: a header, a
// separator, and the rows - without the count a SQL client adds, which says
// nothing the rows do not.
package table

import (
	"strings"
	"unicode/utf8"
)

// Render returns headers and rows as a table in psql's style. The columns listed
// in right are right-aligned (numbers); every other column is left-aligned, and
// headers always are. A row shorter than the headers is padded with empty cells.
func Render(headers []string, rows [][]string, right ...int) string {
	alignRight := make(map[int]bool, len(right))
	for _, i := range right {
		alignRight[i] = true
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range rows {
		for i := range headers {
			if i < len(row) {
				widths[i] = max(widths[i], utf8.RuneCountInString(row[i]))
			}
		}
	}

	var b strings.Builder
	line := func(cells []string, header bool) {
		parts := make([]string, len(headers))
		for i := range headers {
			v := ""
			if i < len(cells) {
				v = cells[i]
			}
			pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(v))
			if alignRight[i] && !header {
				parts[i] = pad + v
			} else {
				parts[i] = v + pad
			}
		}
		b.WriteString(strings.TrimRight(" "+strings.Join(parts, " | "), " "))
		b.WriteByte('\n')
	}

	line(headers, true)
	separators := make([]string, len(headers))
	for i, w := range widths {
		separators[i] = strings.Repeat("-", w+2)
	}
	b.WriteString(strings.Join(separators, "+"))
	b.WriteByte('\n')
	for _, row := range rows {
		line(row, false)
	}
	return b.String()
}

package table

import "testing"

func TestRenderLooksLikePsql(t *testing.T) {
	got := Render([]string{"service", "vip", "port", "weight"}, [][]string{
		{"nginx", "10.10.1.20", "tcp/80", "3"},
		{"db", "10.0.0.50", "tcp/5432", "12"},
	}, 3)
	want := ` service | vip        | port     | weight
---------+------------+----------+--------
 nginx   | 10.10.1.20 | tcp/80   |      3
 db      | 10.0.0.50  | tcp/5432 |     12
(2 rows)
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRenderOneAndNoRows(t *testing.T) {
	if got, want := Render([]string{"a"}, [][]string{{"x"}}), " a\n---\n x\n(1 row)\n"; got != want {
		t.Errorf("one row: got %q, want %q", got, want)
	}
	if got, want := Render([]string{"node", "role"}, nil), " node | role\n------+------\n(0 rows)\n"; got != want {
		t.Errorf("no rows: got %q, want %q", got, want)
	}
}

func TestRenderPadsShortRows(t *testing.T) {
	got := Render([]string{"a", "b"}, [][]string{{"only"}})
	want := " a    | b\n------+---\n only |\n(1 row)\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

package data

import "testing"

func TestParseImportedTable(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{
			"restore file completed",
			"[INFO] [restore.go:196] [\"restore file completed\"] [table=`db`.`t1`] [file=db.t1.0.csv] [imported=67]",
			"db.t1", true,
		},
		{
			"another table",
			"[INFO] [restore.go:196] [\"restore file completed\"] [table=`mydb`.`orders`] [file=mydb.orders.000.csv] [imported=1000]",
			"mydb.orders", true,
		},
		{"other info line", `[INFO] [restore.go:71] ["the whole procedure start"]`, "", false},
		{"no table tag", `[INFO] ["restore file completed"]`, "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		got, ok := parseImportedTable(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: parseImportedTable(%q) = (%q, %v), want (%q, %v)",
				c.name, c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestImportTableCounterDistinct(t *testing.T) {
	// Real-shaped lightning lines: t1 has two chunk files (counted once),
	// t2 and t3 one each — the same table may also appear with a different
	// file name before its restore completes.
	lines := []string{
		"[INFO] [restore.go:196] [\"restore file completed\"] [table=`db`.`t1`] [file=db.t1.0.csv] [imported=67]",
		"[INFO] [restore.go:196] [\"restore file completed\"] [table=`db`.`t2`] [file=db.t2.0.csv] [imported=3]",
		"[INFO] [restore.go:196] [\"restore file completed\"] [table=`db`.`t1`] [file=db.t1.1.csv] [imported=50]",
		"[INFO] [restore.go:196] [\"restore file completed\"] [table=`db`.`t3`] [file=db.t3.csv] [imported=9]",
		"[INFO] [restore.go:196] [\"restore file completed\"] [table=`db`.`t2`] [file=db.t2.0.csv] [imported=3]",
	}

	var c importTableCounter
	wantNew := []bool{true, true, false, true, false}
	wantN := []int{1, 2, 2, 3, 3}
	for i, line := range lines {
		table, ok := parseImportedTable(line)
		if !ok {
			t.Fatalf("line %d: expected parseable table", i)
		}
		n, isNew := c.add(table)
		if isNew != wantNew[i] || n != wantN[i] {
			t.Errorf("line %d (%s): add = (n=%d, isNew=%v), want (n=%d, isNew=%v)",
				i, table, n, isNew, wantN[i], wantNew[i])
		}
	}
	if c.n != 3 {
		t.Errorf("final distinct count = %d, want 3", c.n)
	}
}

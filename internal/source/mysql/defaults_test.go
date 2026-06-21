package mysql

import "testing"

func TestQuoteDefault(t *testing.T) {
	cases := []struct{ dt, def, want string }{
		{"char", "CNY", "'CNY'"},
		{"varchar", "active", "'active'"},
		{"enum", "small", "'small'"},
		{"set", "a,b", "'a,b'"},
		{"json", "{}", "'{}'"},
		{"text", "hi there", "'hi there'"},
		{"varchar", "O'Brien", "'O''Brien'"}, // escape internal quote
		{"int", "42", "42"},
		{"bigint", "-1", "-1"},
		{"decimal", "0.00", "0.00"},
		{"timestamp", "CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP"},
		{"datetime", "CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP"},
		{"datetime", "2020-01-01 00:00:00", "'2020-01-01 00:00:00'"}, // date literal → quote
		{"date", "1970-01-01", "'1970-01-01'"},
		{"char", "NULL", "NULL"},
		{"", "", ""},
		{"char", "", ""},
	}
	for _, c := range cases {
		if got := quoteDefault(c.dt, c.def); got != c.want {
			t.Errorf("quoteDefault(%q, %q) = %q, want %q", c.dt, c.def, got, c.want)
		}
	}
}

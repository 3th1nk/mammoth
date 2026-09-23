package render

import "testing"

func TestSyslogHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://m/render/tok", "m"},
		{"http://10.0.0.1:8080/render/tok", "10.0.0.1"}, // port stripped — the arg carries none
		{"", ""},
		{"::bad url::", ""},
	}
	for _, c := range cases {
		if got := SyslogHost(c.in); got != c.want {
			t.Errorf("SyslogHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

package policy

import (
	"slices"
	"testing"
)

func TestFilterCookies(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"codex's jar and a session cookie", []string{"__oailb=lb1; _cfuvid=u1; session=s3cr3t"}, []string{"__oailb=lb1; _cfuvid=u1"}},
		{"only a session cookie", []string{"session=s3cr3t"}, nil},
		{"no header", nil, nil},
		{"an empty value", []string{""}, nil},
		{"separators only", []string{";; ;"}, nil},
		{"several header lines", []string{"__cf_bm=a", "x=1; cf_clearance=c"}, []string{"__cf_bm=a; cf_clearance=c"}},
		{"challenge cookies", []string{"cf_chl_rc_m=1; cf_chl_2=x; cf_chlx=y"}, []string{"cf_chl_rc_m=1; cf_chl_2=x"}},
		{"names are case-sensitive", []string{"__CF_BM=a; __Oailb=b"}, nil},
		{"whitespace around pairs", []string{"  __cf_bm=a  ;\t__oailb=b\t"}, []string{"__cf_bm=a; __oailb=b"}},
		{"whitespace in the name", []string{"__cf_bm =a; cf_chl_ x=1"}, nil},
		{"no equals sign", []string{"__cf_bm; __oailb=b"}, []string{"__oailb=b"}},
		{"a control character in the value", []string{"__cf_bm=a\x01b; __oailb=b"}, []string{"__oailb=b"}},
		{"an empty value kept", []string{"__cf_bm="}, []string{"__cf_bm="}},
		{"a quoted value kept", []string{`__cf_bm="a=b"`}, []string{`__cf_bm="a=b"`}},
		{"a value with spaces inside kept", []string{"cf_ob_info=502:8a b:SJC"}, []string{"cf_ob_info=502:8a b:SJC"}},
		{"order kept", []string{"cf_use_ob=0; __cflb=1; s=2; __cfruid=3"}, []string{"cf_use_ob=0; __cflb=1; __cfruid=3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FilterCookies(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("FilterCookies(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestInfraCookies(t *testing.T) {
	want := []string{
		"__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "__oailb", "_cfuvid",
		"cf_clearance", "cf_ob_info", "cf_use_ob",
	}
	if got := InfraCookies(); !slices.Equal(got, want) {
		t.Errorf("InfraCookies() = %v, want %v", got, want)
	}
	for _, name := range want {
		if got := FilterCookies([]string{"session=x; " + name + "=v"}); !slices.Equal(got, []string{name + "=v"}) {
			t.Errorf("%s: FilterCookies = %q", name, got)
		}
	}
	for _, name := range []string{"session", "__Secure-next-auth.session-token", "oai-did", "cf_chl", "_cf_bm", "__cf_bm2", ""} {
		if IsInfraCookie(name) {
			t.Errorf("IsInfraCookie(%q) = true", name)
		}
	}
	l := InfraCookies()
	l[0] = "session"
	if IsInfraCookie("session") {
		t.Error("InfraCookies returned the list itself, not a copy")
	}
}

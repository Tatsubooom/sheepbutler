package source

import "testing"

func TestParse(t *testing.T) {
	got, err := Parse(" 大学=12ABC345@ms.dendai.ac.jp ; 個人=someone@gmail.com;")
	if err != nil {
		t.Fatal(err)
	}
	want := []Source{{"大学", "12abc345@ms.dendai.ac.jp"}, {"個人", "someone@gmail.com"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}

	for _, bad := range []string{"大学", "=a@b.c", "大学=not-an-address", "a=x@y.z;b=X@Y.Z", "a=ab@gmail.com;b=a.b@gmail.com"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) はエラーになるべき", bad)
		}
	}
}

func TestDetect(t *testing.T) {
	srcs := []Source{{"大学", "12abc345@ms.dendai.ac.jp"}, {"個人", "someone@gmail.com"}}
	tests := []struct {
		name    string
		headers map[string][]string
		want    string
	}{
		{"To に元の宛先", map[string][]string{"To": {"山田 <12ABC345@ms.dendai.ac.jp>"}}, "大学"},
		{"X-Forwarded-To を優先", map[string][]string{
			"X-Forwarded-To": {"someone@gmail.com"},
			"To":             {"12abc345@ms.dendai.ac.jp"},
		}, "個人"},
		{"BCC でも Received の for で判別", map[string][]string{
			"To":       {"undisclosed-recipients:;"},
			"Received": {"from mx.example.com by mx.google.com\r\n for <someone@gmail.com>; Sun, 4 Oct 2026"},
		}, "個人"},
		{"Gmail はドットと + を無視", map[string][]string{"To": {"Some.One+news@googlemail.com"}}, "個人"},
		{"Gmail 以外はドットを区別", map[string][]string{"To": {"12abc.345@ms.dendai.ac.jp"}}, Unknown},
		{"一致しない", map[string][]string{"To": {"other@example.com"}}, Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect(srcs, tt.headers); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

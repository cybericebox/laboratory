package operator

import "testing"

func TestResolveLabBaseNetwork(t *testing.T) {
	for name, tc := range map[string]struct {
		in      string
		want    string
		wantErr bool
	}{
		"unset: the default": {want: "10.128.0.0/9"},
		"a /8":               {in: "10.0.0.0/8", want: "10.0.0.0/8"},
		"a /18":              {in: "10.0.0.0/18", want: "10.0.0.0/18"},
		"too small":          {in: "10.0.0.0/20", wantErr: true},
		"too big":            {in: "10.0.0.0/7", wantErr: true},
		"not an address":     {in: "ten", wantErr: true},
		"ipv6":               {in: "fd00::/8", wantErr: true},
	} {
		got, err := ResolveLabBaseNetwork(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: %q, %v; want %q (error %v)", name, got, err, tc.want, tc.wantErr)
		}
	}
}

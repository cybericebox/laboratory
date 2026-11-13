package operator

import "testing"

func TestResolveLabBaseNetwork(t *testing.T) {
	for name, tc := range map[string]struct {
		lab, vpn, inet string
		want           string
		wantErr        bool
	}{
		"nothing set: the default":           {want: "10.128.0.0/9"},
		"new setting":                        {lab: "10.0.0.0/8", want: "10.0.0.0/8"},
		"new setting wins over old":          {lab: "10.0.0.0/9", vpn: "10.128.0.0/10", inet: "10.192.0.0/10", want: "10.0.0.0/9"},
		"old defaults are the halves of /9":  {vpn: "10.128.0.0/10", inet: "10.192.0.0/10", want: "10.128.0.0/9"},
		"old halves in another network":      {vpn: "10.64.0.0/11", inet: "10.96.0.0/11", want: "10.64.0.0/10"},
		"old, inet half first: clients move": {vpn: "10.192.0.0/10", inet: "10.128.0.0/10", wantErr: true},
		"old, not neighbours":                {vpn: "10.128.0.0/10", inet: "10.64.0.0/10", wantErr: true},
		"old, different sizes":               {vpn: "10.128.0.0/10", inet: "10.192.0.0/11", wantErr: true},
		"old, only one set":                  {vpn: "10.128.0.0/10", wantErr: true},
		"old kustomize pair is no halves":    {vpn: "10.8.0.0/10", inet: "10.9.0.0/10", wantErr: true},
		"too small":                          {lab: "10.0.0.0/20", wantErr: true},
		"too big":                            {lab: "10.0.0.0/7", wantErr: true},
		"not an address":                     {lab: "ten", wantErr: true},
		"ipv6":                               {lab: "fd00::/8", wantErr: true},
	} {
		got, err := ResolveLabBaseNetwork(tc.lab, tc.vpn, tc.inet)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: %q, %v; want %q (error %v)", name, got, err, tc.want, tc.wantErr)
		}
	}
}

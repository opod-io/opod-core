package main

import "testing"

// PLAN T10.22: OPOD_BIND_ADDR is what the worker listens on when the address it
// advertises is not one this host owns; unset keeps the advertised address.
func TestBindAddress(t *testing.T) {
	for _, tc := range []struct {
		name, advertised, bind string
		listen, host           string
		wantErr                bool
	}{
		{"unset keeps the advertised address", "203.0.113.7:8081", "", "203.0.113.7:8081", "", false},
		{"host and port", "203.0.113.7:18081", "10.0.0.5:8081", "10.0.0.5:8081", "10.0.0.5", false},
		{"host only takes the advertised port", "203.0.113.7:8081", "10.0.0.5", "10.0.0.5:8081", "10.0.0.5", false},
		{"an explicit wildcard is taken (the caller warns)", "203.0.113.7:8081", "0.0.0.0", "0.0.0.0:8081", "0.0.0.0", false},
		{"IPv6", "[2001:db8::7]:8081", "[2001:db8::5]", "[2001:db8::5]:8081", "2001:db8::5", false},
		{"no host", "203.0.113.7:8081", ":8081", "", "", true},
		{"no port anywhere", "203.0.113.7", "10.0.0.5", "", "", true},
	} {
		listen, host, err := bindAddress(tc.advertised, tc.bind)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err %v, want error %v", tc.name, err, tc.wantErr)
			continue
		}
		if listen != tc.listen || host != tc.host {
			t.Errorf("%s: listen %q host %q, want %q %q", tc.name, listen, host, tc.listen, tc.host)
		}
	}
}

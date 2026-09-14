package commons

import "testing"

func TestParseManagementServiceEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
		wantErr  bool
	}{
		{name: "empty disables management service", endpoint: "", want: ""},
		{name: "http endpoint", endpoint: "http://127.0.0.1:13021", want: "127.0.0.1:13021"},
		{name: "omitted scheme", endpoint: "127.0.0.1:13021", want: "127.0.0.1:13021"},
		{name: "all interfaces", endpoint: ":13021", want: ":13021"},
		{name: "IPv6", endpoint: "[::1]:13021", want: "[::1]:13021"},
		{name: "HTTPS is rejected", endpoint: "https://127.0.0.1:13021", wantErr: true},
		{name: "missing port", endpoint: "127.0.0.1", wantErr: true},
		{name: "path is rejected", endpoint: "http://127.0.0.1:13021/api", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseManagementServiceEndpoint(test.endpoint)
			if test.wantErr {
				if err == nil {
					t.Fatal("ParseManagementServiceEndpoint unexpectedly succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseManagementServiceEndpoint: %v", err)
			}
			if got != test.want {
				t.Fatalf("endpoint = %q, want %q", got, test.want)
			}
		})
	}
}

package main

import "testing"

func TestGatewayAuthenticationMode(t *testing.T) {
	for _, tt := range []struct {
		name, token string
		public      bool
		want        string
		wantError   bool
	}{
		{"protected", "gateway-secret", false, "gateway-secret", false},
		{"missing-token-fails-closed", "", false, "", true},
		{"backend-auth", "", true, "", false},
		{"backend-auth-ignores-stale-token", "gateway-secret", true, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := configuredToken(tt.token, tt.public)
			if (err != nil) != tt.wantError || got != tt.want {
				t.Fatalf("got (%q, %v), want token %q and error %v", got, err, tt.want, tt.wantError)
			}
		})
	}
}

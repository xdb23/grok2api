package provider

import "testing"

func TestIsDefinitiveAccountBlockBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "blocked-user code", body: `{"error":{"code":"blocked-user","message":"denied"}}`, want: true},
		{name: "user is blocked message", body: `{"message":"User is blocked"}`, want: true},
		{name: "plain text", body: `account blocked-user permanently`, want: true},
		{name: "generic 403", body: `{"code":"permission-denied","message":"Access denied"}`, want: false},
		{name: "anti-bot noise", body: `{"code":7,"message":"challenge"}`, want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsDefinitiveAccountBlockBody([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %v want %v body=%s", got, tc.want, tc.body)
			}
		})
	}
}

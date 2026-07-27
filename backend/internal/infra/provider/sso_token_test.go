package provider

import "testing"

func TestSanitizeSSOToken(t *testing.T) {
	if got := SanitizeSSOToken("sso=eyJhbGciOiJub25lIn0.eyJzdWIiOiJhIn0.sig; sso-rw=drop"); got != "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhIn0.sig" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateSSOTokenAcceptsJWT(t *testing.T) {
	token := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhY2NvdW50LW9uZSJ9.signaturevalue"
	if err := ValidateSSOToken(token); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSSOTokenRejectsJSONGarbage(t *testing.T) {
	for _, token := range []string{
		`"name": "xaief831eaf4d58@r03.reg.xdb23.tech",`,
		`"sso_token": "eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uX2lkIjoiMWE0ZjIxYSJ9.sig`,
		"token-one",
		"eyJonlyheader",
		"",
	} {
		if err := ValidateSSOToken(SanitizeSSOToken(token)); err == nil {
			t.Fatalf("accepted %q", token)
		}
	}
}

package redact

import (
	"strings"
	"testing"
)

// The cases are upstream's crates/kiri/src/diag/redact.rs tests, with
// Fabo's share token prefix added.

// r is a redactor for a macOS-style home with one signed-in token.
func r() *Redactor {
	return New("/Users/ada/", []string{"s3cret-signed-in-token", "short"})
}

// check fails when Patterns(in) is not want.
func check(t *testing.T, in, want string) {
	t.Helper()
	if got := Patterns(in); got != want {
		t.Errorf("Patterns(%q)\n got %q\nwant %q", in, got, want)
	}
}

// unchanged fails when Patterns changes in.
func unchanged(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		check(t, text, text)
	}
}

// TestHomeBecomesTildeButNotALongerName keeps another user's folder.
func TestHomeBecomesTildeButNotALongerName(t *testing.T) {
	out := r().Apply("open /Users/ada/.fabo/logs and /Users/adam/x and /Users/ada")
	if out != "open ~/.fabo/logs and /Users/adam/x and ~" {
		t.Fatal(out)
	}
}

// TestKnownSecretsGoWhereverTheyAre takes out exact secrets, not short ones.
func TestKnownSecretsGoWhereverTheyAre(t *testing.T) {
	if got := r().Apply("sent s3cret-signed-in-token twice: s3cret-signed-in-token"); got != "sent [redacted] twice: [redacted]" {
		t.Fatal(got)
	}
	if got := r().Apply("a short note"); got != "a short note" {
		t.Fatal(got)
	}
}

// TestBearerTokens takes out bearer tokens but not the word.
func TestBearerTokens(t *testing.T) {
	check(t, "Authorization: Bearer eyJhbGciOi.J9x-y_z", "Authorization: Bearer [redacted]")
	check(t, "authorization: bearer abcdefgh1234", "authorization: bearer [redacted]")
	unchanged(t, "the bearer of bad news", "Bearer [redacted]")
}

// TestJSONValuesOfSecretKeys handles plain and escaped JSON.
func TestJSONValuesOfSecretKeys(t *testing.T) {
	check(t, `{"login":"ada","token":"ks_abc","password" : "p\"w","access_token":"x","auths":{"r":{"auth":"YWRhOnB3"}}}`,
		`{"login":"ada","token":"[redacted]","password" : "[redacted]","access_token":"[redacted]","auths":{"r":{"auth":"[redacted]"}}}`)
	check(t, `body {\"secret\":\"hunter2\",\"n\":1}`, `body {\"secret\":\"[redacted]\",\"n\":1}`)
	unchanged(t, `{"msg":"token","n":"1"}`)
}

// TestQueryValuesAndAssignments covers key=value forms.
func TestQueryValuesAndAssignments(t *testing.T) {
	check(t, "GET /cb?code=1&token=abc123&x=2 HTTP/1.1", "GET /cb?code=1&token=[redacted]&x=2 HTTP/1.1")
	check(t, "https://x/?password=hunter2", "https://x/?password=[redacted]")
	check(t, "-e DB_PASSWORD=hunter2 -e MODE=dev", "-e DB_PASSWORD=[redacted] -e MODE=dev")
	check(t, "--password='two words' next", "--password='[redacted]' next")
	check(t, `FABO_SHARE_TOKEN="abc def"`, `FABO_SHARE_TOKEN="[redacted]"`)
	unchanged(t, "if token == other", "a=b token=")
}

// TestShareTokensAnywhere takes out Fabo's and Kiri's share tokens.
func TestShareTokensAnywhere(t *testing.T) {
	check(t, "signed in with ks_0123456789abcdefXYZ, ok", "signed in with ks_[redacted], ok")
	check(t, "signed in with ft_0123456789abcdefXYZ, ok", "signed in with ft_[redacted], ok")
	unchanged(t, "ks_x and thanks_0123456789abcdef", "left_0123456789abcdef")
}

// TestPasswordsInURLs takes out user info but leaves other @s.
func TestPasswordsInURLs(t *testing.T) {
	check(t, "DATABASE_URL=postgres://app:hunter2@db.internal/prod", "DATABASE_URL=postgres://[redacted]@db.internal/prod")
	check(t, "proxy failed: http://bob:p@ss@proxy.corp:3128, retrying", "proxy failed: http://[redacted]@proxy.corp:3128, retrying")
	check(t, "https://ghp_x@github.com/a/b.git", "https://[redacted]@github.com/a/b.git")
	unchanged(t, "https://example.com/a@b", "git@github.com:a/b.git", "http://localhost:8080/?who=a@b.c", "see https://github.com/OpenFabrica/Fabo.")
}

// TestAuthorizationValues takes out any scheme's value.
func TestAuthorizationValues(t *testing.T) {
	check(t, "Authorization: Basic Ym9iOmh1bnRlcjI=", "Authorization: Basic [redacted]")
	check(t, "proxy-authorization: Basic Ym9i, next", "proxy-authorization: Basic [redacted], next")
	check(t, "Authorization: abc123", "Authorization: [redacted]")
	check(t, `{"Authorization":"Basic Ym9i"}`, `{"Authorization":"[redacted]"}`)
	unchanged(t, "basic authentication failed")
}

// TestPrivateKeyBlocks takes out private keys but not certificates.
func TestPrivateKeyBlocks(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END RSA PRIVATE KEY-----"
	check(t, "before "+key+" after", "before [redacted] after")
	check(t, "x -----BEGIN PRIVATE KEY----- MIIEvQIBADANBg", "x [redacted]")
	unchanged(t, "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----")
}

// TestSecretFlags takes out flag values but not the next flag.
func TestSecretFlags(t *testing.T) {
	check(t, "--password hunter2 --verbose", "--password [redacted] --verbose")
	check(t, "login --token 'two words' x", "login --token '[redacted]' x")
	check(t, "--api-key abc", "--api-key [redacted]")
	unchanged(t, "--password --verbose", "--password", "--name web", "pre--token x")
}

// TestMoreSecretKeys checks which key names count as secret.
func TestMoreSecretKeys(t *testing.T) {
	for _, key := range []string{"PGPASS", "MYSQL_PASS", "API-KEY", "X.API.KEY", "SSH_PRIVATE_KEY", "AWS_ACCESS_KEY_ID", "GPG_PASSPHRASE"} {
		check(t, key+"=abc123", key+"=[redacted]")
	}
	unchanged(t, "bypass=1", "compass=north", "passed=3", "MONKEY=1")
}

// TestTokensThatSayWhatTheyAre takes out prefixed tokens.
func TestTokensThatSayWhatTheyAre(t *testing.T) {
	check(t, "pushed with ghp_abcdefghijklmnopqrstuvwxyz0123456789 ok", "pushed with ghp_[redacted] ok")
	check(t, "github"+"_pat_11ABCDEFG0abcdefghij_klmn", "github"+"_pat_[redacted]")
	check(t, "glpat-abcdef123456xyz", "glpat-[redacted]")
	check(t, "dckr_pat_abcdefghijklmnop", "dckr_pat_[redacted]")
	check(t, "key AKIAIOSFODNN7EXAMPLE", "key AKIA[redacted]")
	unchanged(t, "ghp_short and aghp_abcdefghijklmnop")
}

// TestRedactingTwiceChangesNothing keeps redaction idempotent.
func TestRedactingTwiceChangesNothing(t *testing.T) {
	text := `Bearer abcdefghijkl token=abc {"token":"x"} ks_0123456789abcdef /Users/ada/x postgres://a:b@h/d Authorization: Basic Ym9i --password hunter2 --token "two words" ghp_abcdefghijklmnop -----BEGIN PRIVATE KEY-----k-----END PRIVATE KEY----- end`
	once := r().Apply(text)
	if twice := r().Apply(once); twice != once {
		t.Fatalf("twice %q\nonce %q", twice, once)
	}
	if strings.Contains(once, "abcdefghijkl") || strings.Contains(once, "0123456789abcdef") || strings.Contains(once, "/Users/ada") {
		t.Fatal(once)
	}
}

// TestWindowsHomeInEveryForm matches each way Windows and WSL write it.
func TestWindowsHomeInEveryForm(t *testing.T) {
	r := New(`C:\Users\Alex`, nil)
	for _, c := range []struct{ in, want string }{
		{`opened C:\Users\Alex\proj`, `opened ~\proj`},
		{`{"path":"C:\\Users\\Alex\\proj"}`, `{"path":"~\\proj"}`},
		{"C:/Users/Alex/proj", "~/proj"},
		{`c:\users\ALEX\proj`, `~\proj`},
		{"/mnt/c/Users/Alex/proj", "~/proj"},
		{"/home/alex/.docker/config.json", "~/.docker/config.json"},
	} {
		if got := r.Apply(c.in); got != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
	if got := r.Apply(`C:\Users\Alexandra\x`); got != `C:\Users\Alexandra\x` {
		t.Fatal(got)
	}
	if got := New(`C:\`, nil).Apply(`C:\x`); got != `C:\x` {
		t.Fatal(got)
	}
}

// TestColonAndSpacedValues covers key: value and key = value.
func TestColonAndSpacedValues(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"password: hunter2", "password: [redacted]"},
		{"token : abc123 next", "token : [redacted] next"},
		{`token: "two words"`, `token: "[redacted]"`},
		{"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG", "aws_secret_access_key = [redacted]"},
		{`Config { password: "x", port: 5432 }`, `Config { password: "[redacted]", port: 5432 }`},
		{"X-Api-Key: abcdef0123456789", "X-Api-Key: [redacted]"},
		{"Cookie: session=abc; theme=dark\nnext", "Cookie: [redacted]\nnext"},
		{"Set-Cookie: sg_session=abc; Path=/", "Set-Cookie: [redacted]"},
	} {
		check(t, c.in, c.want)
	}
	unchanged(t, "error: could not connect", "at 12:30: done", "see https://example.com/token", "token refresh failed", "port = 5432", "password:\nnext")
}

// TestASecretInsideAnotherValue looks inside values that are not secret.
func TestASecretInsideAnotherValue(t *testing.T) {
	check(t, "env=[DB_PASSWORD=hunter2 STRIPE=1]", "env=[DB_PASSWORD=[redacted] STRIPE=1]")
	check(t, "args=--password=hunter2", "args=--password=[redacted]")
}

// TestEmailAddresses takes out addresses, not git remotes or digests.
func TestEmailAddresses(t *testing.T) {
	check(t, "logged in as ada.lovelace@example.org.", "logged in as [redacted].")
	unchanged(t, "git@github.com:a/b.git", "nginx@sha256:abcd", "a @ b", "x@localhost")
}

// TestJWTsWhole takes out a JWT whole.
func TestJWTsWhole(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmn"
	check(t, "got "+jwt+" ok", "got [redacted] ok")
	unchanged(t, "eyJustAWord and eyJx.y")
}

// TestMoreTokensThatSayWhatTheyAre covers the other prefixes.
func TestMoreTokensThatSayWhatTheyAre(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"sk_live_abcdefghijklmnopqrstu", "sk_live_[redacted]"},
		{"xoxb-123456789012-abcdefghijkl", "xoxb-[redacted]"},
		{"sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "sk-ant-[redacted]"},
		{"sk-proj-abcdefghijklmnopqrstuvwxyz012345", "sk-proj-[redacted]"},
		{"AIzaSyA1234567890abcdefghijklmnop", "AIza[redacted]"},
		{"npm_abcdefghijklmnopqrstuvwxyz0123456789", "npm_[redacted]"},
	} {
		check(t, c.in, c.want)
	}
}

// TestRedactingTheNewFormsTwiceChangesNothing keeps the later forms idempotent.
func TestRedactingTheNewFormsTwiceChangesNothing(t *testing.T) {
	text := `password: hunter2 Cookie: a=b eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmn sk_live_abcdefghijklmnop env=[DB_PASSWORD=x ada@example.org C:/Users/Ada/x`
	r := New(`C:\Users\Ada`, nil)
	once := r.Apply(text)
	if r.Apply(once) != once {
		t.Fatal(once)
	}
	if strings.Contains(once, "hunter2") || strings.Contains(once, "ada@") || strings.Contains(once, "Users/Ada") {
		t.Fatal(once)
	}
}

// TestLeavesOtherTextAndUnicodeAlone keeps ordinary text, multi-byte too.
func TestLeavesOtherTextAndUnicodeAlone(t *testing.T) {
	unchanged(t, "VM booted in 112 ms → 4 CPUs, café=ok, a = b", "İstanbul token İ=1 ÄÖÜ")
	// Non-ASCII letters around a secret keep their bytes.
	check(t, "café password=hunter2 → ok", "café password=[redacted] → ok")
}

// Package redact takes out of logs and reports what must not leave the
// computer: the home folder's path (it names the person), in every form
// Windows writes it, bearer and other Authorization values, the values of
// token, password, secret and cookie keys in JSON, query strings,
// KEY=value, `key: value` and `key = value` pairs and `--flag value`,
// passwords in URLs, private keys, JWTs, e-mail addresses, and tokens that
// say what they are (ft_…, ghp_…, sk_live_…, xoxb-…, sk-ant-…).
//
// Patterns, not parsing: it runs over any text (log lines, panic
// messages), so it errs towards taking out too much. A port of upstream
// Kiri's crates/kiri/src/diag/redact.rs; every function works on byte
// offsets, and lower-casing is ASCII-only so offsets in the lowered copy
// match the original (strings.ToLower may change a string's length).
package redact

import "strings"

// Mark replaces whatever is taken out.
const Mark = "[redacted]"

// homeForm is one way the home folder may be written, and whether to
// match it ignoring case (Windows paths).
type homeForm struct {
	text    string
	anyCase bool
}

// Redactor takes out the home folder, known secrets and the patterns.
type Redactor struct {
	homes   []homeForm
	secrets []string
}

// New returns a Redactor for home (empty for none) and exact secrets to
// take out wherever they appear; secrets shorter than 8 bytes are ignored,
// as matching them blindly would take out ordinary words.
func New(home string, secrets []string) *Redactor {
	r := &Redactor{homes: homeForms(home)}
	for _, s := range secrets {
		if len(s) >= 8 {
			r.secrets = append(r.secrets, s)
		}
	}
	return r
}

// Apply takes the secrets, the home folder (written as ~) and the
// patterns out of text.
func (r *Redactor) Apply(text string) string {
	s := text
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, Mark)
	}
	for _, h := range r.homes {
		s = replaceHome(s, h.text, h.anyCase)
	}
	return Patterns(s)
}

// Patterns applies the patterns alone (no home, no known secrets).
func Patterns(s string) string {
	s = privateKeys(s)
	s = urlUserinfo(s)
	s = bearer(s)
	s = authorization(s)
	s = jsonValues(s, `"`)
	// The same inside a JSON string: {\"token\":\"…\"}.
	s = jsonValues(s, `\"`)
	s = assignments(s)
	s = spacedValues(s)
	s = flags(s)
	s = jwts(s)
	s = emails(s)
	for _, prefix := range tokenPrefixes {
		s = prefixedTokens(s, prefix)
	}
	return s
}

// lowerASCII lower-cases ASCII letters only, keeping every byte in place.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// isAlnum reports an ASCII letter or digit.
func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// isWord reports a byte of an identifier.
func isWord(b byte) bool { return isAlnum(b) || b == '_' }

// keyByte reports a byte of a key: an identifier, `-` or `.`.
func keyByte(c byte) bool { return isWord(c) || c == '-' || c == '.' }

// tokenByte reports a byte of a bearer or authorization value.
func tokenByte(c byte) bool { return isAlnum(c) || strings.IndexByte("-._~+/=", c) >= 0 }

// isSpace reports what Rust's char::is_whitespace does for ASCII, and
// treats any non-ASCII byte as not a space.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// indexFunc is the first i at or after 0 in s where f(s[i]) holds, or -1.
func indexFunc(s string, f func(byte) bool) int {
	for i := 0; i < len(s); i++ {
		if f(s[i]) {
			return i
		}
	}
	return -1
}

// sensitive reports a key whose value is secret: token, access_token,
// PASSWORD, client_secret, API-KEY, PGPASS, docker's auth, identitytoken…
func sensitive(key string) bool {
	k := lowerASCII(strings.TrimLeft(key, "-"))
	k = strings.NewReplacer("-", "_", ".", "_").Replace(k)
	switch k {
	case "auth", "authorization", "pass", "pgpass":
		return true
	}
	if strings.HasSuffix(k, "_pass") {
		return true
	}
	for _, w := range []string{"token", "password", "passwd", "passphrase", "secret", "apikey", "api_key", "access_key", "private_key", "credential", "crashreporterkey", "cookie", "session"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// homeForms is the home folder as logs may write it. A Windows one
// (C:\Users\Ada) is also matched doubled inside JSON (C:\\Users\\Ada),
// with forward slashes, as WSL mounts it (/mnt/c/Users/Ada) and as the
// WSL home of the same name (/home/ada), all ignoring case, as Windows does.
func homeForms(home string) []homeForm {
	home = strings.TrimRight(home, `/\`)
	b := home
	if len(b) > 2 && (b[0] >= 'a' && b[0] <= 'z' || b[0] >= 'A' && b[0] <= 'Z') && b[1] == ':' && (b[2] == '\\' || b[2] == '/') {
		var parts []string
		for _, p := range strings.FieldsFunc(home[2:], func(r rune) bool { return r == '\\' || r == '/' }) {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		drive := home[:2]
		forms := []string{
			drive + `\\` + strings.Join(parts, `\\`),
			drive + `\` + strings.Join(parts, `\`),
			drive + "/" + strings.Join(parts, "/"),
			"/mnt/" + lowerASCII(drive[:1]) + "/" + strings.Join(parts, "/"),
		}
		if user := parts[len(parts)-1]; len(user) > 1 {
			forms = append(forms, "/home/"+user)
		}
		out := make([]homeForm, len(forms))
		for i, f := range forms {
			out[i] = homeForm{f, true}
		}
		return out
	}
	if strings.HasPrefix(home, "/") && len(home) > 1 {
		return []homeForm{{home, false}}
	}
	return nil
}

// replaceHome writes /Users/ada/x as ~/x, but not /Users/adam. anyCase
// matches c:\users\ADA too.
func replaceHome(s, home string, anyCase bool) string {
	hay, needle := s, home
	if anyCase {
		hay, needle = lowerASCII(s), lowerASCII(home)
	}
	var out strings.Builder
	last, from := 0, 0
	for {
		n := strings.Index(hay[from:], needle)
		if n < 0 {
			break
		}
		i := from + n
		end := i + len(needle)
		whole := end >= len(s) || !(isWord(s[end]) || s[end] == '-' || s[end] == '.')
		if whole {
			out.WriteString(s[last:i])
			out.WriteByte('~')
			last = end
		}
		from = end
	}
	out.WriteString(s[last:])
	return out.String()
}

// privateKeys takes out a PEM private key whole; without its end line,
// everything after its start, since the rest of the key may be in there.
func privateKeys(s string) string {
	const begin, dashes = "-----BEGIN ", "-----"
	var out strings.Builder
	rest := s
	for {
		i := strings.Index(rest, begin)
		if i < 0 {
			break
		}
		after := rest[i+len(begin):]
		labelLen := strings.Index(after, dashes)
		if labelLen < 0 {
			break
		}
		label := after[:labelLen]
		if !strings.HasSuffix(label, "PRIVATE KEY") {
			out.WriteString(rest[:i+len(begin)])
			rest = after
			continue
		}
		out.WriteString(rest[:i])
		out.WriteString(Mark)
		body := after[labelLen+len(dashes):]
		end := "-----END " + label + "-----"
		if n := strings.Index(body, end); n >= 0 {
			rest = body[n+len(end):]
		} else {
			rest = ""
		}
	}
	out.WriteString(rest)
	return out.String()
}

// urlUserinfo writes postgres://app:hunter2@db/x as
// postgres://[redacted]@db/x: all of the part before the @, since a token
// can stand where the user name goes.
func urlUserinfo(s string) string {
	var out strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "://")
		if i < 0 {
			break
		}
		start := i + 3
		out.WriteString(rest[:start])
		after := rest[start:]
		end := strings.IndexFunc(after, func(c rune) bool {
			return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' || strings.ContainsRune("/?#\"'<>\\", c)
		})
		if end < 0 {
			end = len(after)
		}
		if at := strings.LastIndexByte(after[:end], '@'); at >= 0 && after[:at] != Mark {
			out.WriteString(Mark)
			rest = after[at:]
		} else {
			rest = after
		}
	}
	out.WriteString(rest)
	return out.String()
}

// authorization writes Authorization: Basic abc as Authorization: Basic
// [redacted], whatever the scheme, and the value alone when it has none.
// Basic isn't matched by itself as Bearer is: "basic authentication" is
// ordinary text.
func authorization(s string) string {
	const key = "authorization:"
	lower := lowerASCII(s)
	skip := func(j int) int {
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		return j
	}
	word := func(j int) int {
		for j < len(s) && tokenByte(s[j]) {
			j++
		}
		return j
	}
	var out strings.Builder
	last, from := 0, 0
	for {
		n := strings.Index(lower[from:], key)
		if n < 0 {
			break
		}
		i := from + n
		from = i + len(key)
		first := skip(from)
		firstEnd := word(first)
		second := skip(firstEnd)
		secondEnd := word(second)
		if strings.HasPrefix(s[second:], Mark) {
			continue
		}
		start, end := first, firstEnd
		if secondEnd > second {
			start, end = second, secondEnd
		}
		if end > start {
			out.WriteString(s[last:start])
			out.WriteString(Mark)
			last = end
			from = end
		}
	}
	out.WriteString(s[last:])
	return out.String()
}

// bearer writes Bearer <token> (any case) as Bearer [redacted].
func bearer(s string) string {
	lower := lowerASCII(s)
	var out strings.Builder
	last, from := 0, 0
	for {
		n := strings.Index(lower[from:], "bearer")
		if n < 0 {
			break
		}
		i := from + n
		j := i + 6
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		start := j
		for j < len(s) && tokenByte(s[j]) {
			j++
		}
		if (i == 0 || !isWord(s[i-1])) && start > i+6 && j-start >= 8 {
			out.WriteString(s[last:start])
			out.WriteString(Mark)
			last = j
		}
		from = max(j, i+6)
	}
	out.WriteString(s[last:])
	return out.String()
}

// closeQuote is the end of a string value starting at s[0]: the next q
// (for a plain quote, one not escaped), or -1.
func closeQuote(s, q string) int {
	if q != `"` {
		return strings.Index(s, q)
	}
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			i += 2
		case '"':
			return i
		default:
			i++
		}
	}
	return -1
}

// jsonValues writes "token": "…" as "token": "[redacted]", with q as the quote.
func jsonValues(s, q string) string {
	var out strings.Builder
	last, from := 0, 0
	skip := func(j int) int { return j + (len(s[j:]) - len(strings.TrimLeft(s[j:], " \t\n\r\v\f"))) }
	for {
		n := strings.Index(s[from:], q)
		if n < 0 {
			break
		}
		i := from + n
		keyStart := i + len(q)
		keyLen := strings.Index(s[keyStart:], q)
		if keyLen < 0 {
			break
		}
		key := s[keyStart : keyStart+keyLen]
		j := skip(keyStart + keyLen + len(q))
		if key != "" && len(key) <= 64 && !strings.ContainsAny(key, `\"`) && strings.HasPrefix(s[j:], ":") {
			j = skip(j + 1)
			if sensitive(key) && strings.HasPrefix(s[j:], q) {
				start := j + len(q)
				if l := closeQuote(s[start:], q); l >= 0 {
					out.WriteString(s[last:start])
					out.WriteString(Mark)
					last = start + l
					from = last + len(q)
					continue
				}
			}
			from = j
			continue
		}
		// The closing quote may open the next key.
		from = keyStart + keyLen
	}
	out.WriteString(s[last:])
	return out.String()
}

// assignments takes out the value after a sensitive key and =:
// ?token=…&, PASSWORD=…, --password=…. flags has --password ….
func assignments(s string) string {
	var out strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if s[i] != '=' || (i+1 < len(s) && s[i+1] == '=') || i < last {
			i++
			continue
		}
		k := i
		for k > 0 && keyByte(s[k-1]) {
			k--
		}
		start := i + 1
		end := len(s)
		if start < len(s) && (s[start] == '"' || s[start] == '\'') {
			if n := strings.IndexByte(s[start+1:], s[start]); n >= 0 {
				end = start + 1 + n + 1
			}
		} else if n := indexFunc(s[start:], func(c byte) bool { return isSpace(c) || strings.IndexByte("&\"',;)#<>", c) >= 0 }); n >= 0 {
			end = start + n
		}
		if k < i && sensitive(s[k:i]) && end > start && s[start:end] != Mark {
			quoted := s[start] == '"' || s[start] == '\''
			out.WriteString(s[last:start])
			if quoted {
				out.WriteByte(s[start])
				out.WriteString(Mark)
				out.WriteByte(s[start])
			} else {
				out.WriteString(Mark)
			}
			last = end
			i = end
		} else {
			// Not a secret: the value may hold one (env=[DB_PASSWORD=x).
			i++
		}
	}
	out.WriteString(s[last:])
	return out.String()
}

// spacedValues takes out the value after a sensitive key and a : or an =
// with spaces around it: password: hunter2, token = "abc",
// aws_secret_access_key = …, Cookie: a=b; c=d. A cookie header loses the
// rest of its line. Authorization has its own pass, and key=value with no
// spaces is assignments'.
func spacedValues(s string) string {
	blank := func(c byte) bool { return c == ' ' || c == '\t' }
	var out strings.Builder
	last := 0
	for i := 0; i < len(s); {
		sep := s[i]
		if !(sep == ':' || sep == '=') || i < last {
			i++
			continue
		}
		j := i
		for j > 0 && blank(s[j-1]) {
			j--
		}
		k := j
		for k > 0 && keyByte(s[k-1]) {
			k--
		}
		start := i + 1
		for start < len(s) && blank(s[start]) {
			start++
		}
		key := s[k:j]
		lower := lowerASCII(key)
		spaced := j < i || start > i+1
		var next, prev byte
		if i+1 < len(s) {
			next = s[i+1]
		}
		if i > 0 {
			prev = s[i-1]
		}
		// = with no spaces is assignments; ==, != and <= compare; :// is a
		// URL and :: a path.
		skip := k == j ||
			(sep == '=' && (!spaced || next == '=' || prev == '=' || prev == '!' || prev == '<' || prev == '>')) ||
			(sep == ':' && (next == '/' || next == ':' || prev == ':')) ||
			lower == "authorization" || lower == "proxy-authorization" ||
			!sensitive(key) ||
			start >= len(s) || s[start] == '\n' || s[start] == '\r'
		if skip {
			i++
			continue
		}
		cookie := strings.HasSuffix(lower, "cookie")
		var quote byte
		if s[start] == '"' || s[start] == '\'' {
			quote = s[start]
		}
		end := len(s)
		switch {
		case quote != 0:
			if n := strings.IndexByte(s[start+1:], quote); n >= 0 {
				end = start + 1 + n + 1
			}
		case cookie:
			if n := strings.IndexAny(s[start:], "\n\r"); n >= 0 {
				end = start + n
			}
		default:
			if n := indexFunc(s[start:], func(c byte) bool { return isSpace(c) || strings.IndexByte(",;&)<>\"'", c) >= 0 }); n >= 0 {
				end = start + n
			}
		}
		value := s[start:end]
		if end <= start || strings.HasPrefix(strings.Trim(value, `"'`), Mark) {
			i++
			continue
		}
		out.WriteString(s[last:start])
		if quote != 0 {
			out.WriteByte(quote)
			out.WriteString(Mark)
			out.WriteByte(quote)
		} else {
			out.WriteString(Mark)
		}
		last = end
		i = end
	}
	out.WriteString(s[last:])
	return out.String()
}

// emails takes out ada@example.org whole: an e-mail address names the
// person. Not git@github.com:…, and not image@sha256:….
func emails(s string) string {
	local := func(c byte) bool { return isAlnum(c) || strings.IndexByte("._%+-", c) >= 0 }
	domain := func(c byte) bool { return isAlnum(c) || c == '-' || c == '.' }
	var out strings.Builder
	last, from := 0, 0
	for {
		n := strings.IndexByte(s[from:], '@')
		if n < 0 {
			break
		}
		at := from + n
		from = at + 1
		i := at
		for i > last && local(s[i-1]) {
			i--
		}
		j := at + 1
		for j < len(s) && domain(s[j]) {
			j++
		}
		host := strings.TrimRight(s[at+1:j], ".")
		j = at + 1 + len(host)
		tld := host[strings.LastIndexByte(host, '.')+1:]
		alpha := tld != ""
		for k := 0; k < len(tld); k++ {
			if !(tld[k] >= 'a' && tld[k] <= 'z' || tld[k] >= 'A' && tld[k] <= 'Z') {
				alpha = false
			}
		}
		if i < at && s[i:at] != "git" && strings.Contains(host, ".") && len(tld) >= 2 && alpha {
			out.WriteString(s[last:i])
			out.WriteString(Mark)
			last = j
			from = j
		}
	}
	out.WriteString(s[last:])
	return out.String()
}

// jwts takes out a JWT (eyJ….eyJ….sig) whole.
func jwts(s string) string {
	part := func(c byte) bool { return isAlnum(c) || c == '_' || c == '-' }
	var out strings.Builder
	last, from := 0, 0
	for {
		n := strings.Index(s[from:], "eyJ")
		if n < 0 {
			break
		}
		i := from + n
		j, dots := i, 0
		for j < len(s) && (part(s[j]) || (s[j] == '.' && dots < 2)) {
			if s[j] == '.' {
				dots++
			}
			j++
		}
		parts := strings.Split(s[i:j], ".")
		if (i == 0 || !isWord(s[i-1])) && dots == 2 && j-i >= 24 && len(parts) > 1 && strings.HasPrefix(parts[1], "eyJ") {
			out.WriteString(s[last:i])
			out.WriteString(Mark)
			last = j
		}
		from = max(j, i+3)
	}
	out.WriteString(s[last:])
	return out.String()
}

// flags takes out the value after a sensitive long flag and spaces:
// --password hunter2, --token "two words". The next flag isn't a value.
func flags(s string) string {
	var out strings.Builder
	last, from := 0, 0
	for {
		n := strings.Index(s[from:], "--")
		if n < 0 {
			break
		}
		i := from + n
		k := i + 2
		for k < len(s) && keyByte(s[k]) {
			k++
		}
		from = k
		start := k
		for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
			start++
		}
		if (i > 0 && keyByte(s[i-1])) || !sensitive(s[i:k]) || start == k || start == len(s) {
			continue
		}
		var quote byte
		if s[start] == '"' || s[start] == '\'' {
			quote = s[start]
		}
		end := len(s)
		if quote != 0 {
			if m := strings.IndexByte(s[start+1:], quote); m >= 0 {
				end = start + 1 + m + 1
			}
		} else if m := indexFunc(s[start:], isSpace); m >= 0 {
			end = start + m
		}
		value := s[start:end]
		if strings.HasPrefix(value, "-") || strings.Trim(value, `"'`) == Mark {
			continue
		}
		out.WriteString(s[last:start])
		if quote != 0 {
			out.WriteByte(quote)
			out.WriteString(Mark)
			out.WriteByte(quote)
		} else {
			out.WriteString(Mark)
		}
		last = end
		from = end
	}
	out.WriteString(s[last:])
	return out.String()
}

// tokenPrefixes start a token that says what it is: Fabo share's (and
// Kiri share's), GitHub's, GitLab's, Docker Hub's, an AWS access key id,
// Stripe's, Slack's, Anthropic's, OpenAI's, Google's, npm's and PyPI's.
var tokenPrefixes = []string{
	"ft_", "ks_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github" + "_pat_", "glpat-", "glptt-", "dckr_pat_", "AKIA", "ASIA", "sk_live_", "sk_test_", "rk_live_", "rk_test_",
	"xoxb-", "xoxp-", "xoxa-", "xoxr-", "sk-ant-", "sk-proj-", "sk-svcacct-", "AIza", "npm_", "pypi-",
}

// prefixedTokens takes out a token of prefix's kind, wherever it appears.
func prefixedTokens(s, prefix string) string {
	tail := func(c byte) bool { return isWord(c) || c == '-' }
	var out strings.Builder
	last, from := 0, 0
	for {
		n := strings.Index(s[from:], prefix)
		if n < 0 {
			break
		}
		i := from + n
		j := i + len(prefix)
		for j < len(s) && tail(s[j]) {
			j++
		}
		if (i == 0 || !isWord(s[i-1])) && j-(i+len(prefix)) >= 12 {
			out.WriteString(s[last : i+len(prefix)])
			out.WriteString(Mark)
			last = j
		}
		from = j
	}
	out.WriteString(s[last:])
	return out.String()
}

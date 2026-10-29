package envelope

import (
	"fmt"
	"regexp"
	"strings"
)

// SecretMatch is one hit from ScanSecrets. The matched text is never
// included, only its kind and position, so a scan result is safe to log.
type SecretMatch struct {
	Kind   string
	Offset int
}

func (m SecretMatch) String() string {
	return fmt.Sprintf("%s at byte %d", m.Kind, m.Offset)
}

type secretPattern struct {
	kind string
	re   *regexp.Regexp
}

// secretPatterns are the shapes the CLI refuses to send by default. They
// are deliberately conservative: a false positive costs one --force-unsafe,
// a false negative leaks a credential to another machine.
var secretPatterns = []secretPattern{
	{"PEM private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"GitHub fine-grained token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`)},
	{"AWS access key id", regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"OpenAI-style key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{"NATS user credential", regexp.MustCompile(`-----BEGIN NATS USER JWT-----`)},
	{"NATS nkey seed", regexp.MustCompile(`\bS[UAO][A-Z2-7]{50,}\b`)},
	{"JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{"bearer header", regexp.MustCompile(`(?i)\bauthorization:\s*bearer\s+[A-Za-z0-9._~+/=-]{16,}`)},
	{"password assignment", regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|api[_-]?key|token)\s*[:=]\s*['"]?[^\s'"]{6,}`)},
	{"credential in URL", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:[^\s/@]+@`)},
}

// ScanSecrets returns every secret-shaped span found in body. An empty
// result means the body passed the scan; it does not mean the body is safe.
func ScanSecrets(body string) []SecretMatch {
	var out []SecretMatch
	for _, p := range secretPatterns {
		for _, loc := range p.re.FindAllStringIndex(body, -1) {
			out = append(out, SecretMatch{Kind: p.kind, Offset: loc[0]})
		}
	}
	return out
}

// DescribeMatches renders matches as one line per hit for a CLI refusal.
func DescribeMatches(ms []SecretMatch) string {
	var b strings.Builder
	for _, m := range ms {
		b.WriteString("  - ")
		b.WriteString(m.String())
		b.WriteString("\n")
	}
	return b.String()
}

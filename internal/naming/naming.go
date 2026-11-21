// Package naming derives valid, stable cloud resource names from Kubernetes objects.
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	maxBucket    = 63
	maxAccountID = 30
	hashLen      = 8
)

// BucketName returns "<prefix>-<namespace>-<name>", made valid for GCS.
// Names that are too long are shortened and end in a hash, so they stay unique.
func BucketName(prefix, namespace, name string) string {
	full := sanitize(join(prefix, namespace, name))
	if len(full) <= maxBucket {
		return full
	}
	return shorten(full, maxBucket, prefix, namespace, name)
}

// AccountID returns a service-account ID (6-30 chars, [a-z][a-z0-9-]*[a-z0-9]).
// It always ends in a hash of the inputs, because the readable part is often cut.
func AccountID(prefix, namespace, name string) string {
	return shorten(sanitize(join(prefix, namespace, name)), maxAccountID, prefix, namespace, name)
}

// LabelValue makes s a valid label value (lowercase, max 63 chars).
func LabelValue(s string) string {
	s = sanitize(s)
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

func join(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "-")
}

// sanitize keeps [a-z0-9-], collapses runs of dashes, and makes sure the result
// starts with a letter and does not end with a dash.
func sanitize(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		out = "t" + out
	}
	return out
}

func shorten(s string, max int, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "/")))
	suffix := hex.EncodeToString(h[:])[:hashLen]
	keep := max - hashLen - 1
	if len(s) > keep {
		s = strings.TrimRight(s[:keep], "-")
	}
	return s + "-" + suffix
}

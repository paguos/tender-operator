package naming

import (
	"regexp"
	"strings"
	"testing"
)

var (
	bucketRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	accountRe = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
)

func TestBucketName(t *testing.T) {
	long := strings.Repeat("a", 60)
	cases := []struct {
		prefix, ns, name, want string
	}{
		{"tender", "shop-dev", "files", "tender-shop-dev-files"},
		{"", "shop", "files", "shop-files"},
		{"Lab", "my_ns", "Files.Bucket", "lab-my-ns-files-bucket"},
		{"9lab", "ns", "n", "t9lab-ns-n"},
	}
	for _, c := range cases {
		if got := BucketName(c.prefix, c.ns, c.name); got != c.want {
			t.Errorf("BucketName(%q, %q, %q) = %q, want %q", c.prefix, c.ns, c.name, got, c.want)
		}
	}

	a := BucketName("tender", long, "one")
	b := BucketName("tender", long, "two")
	if a == b {
		t.Fatalf("long names collided: %q", a)
	}
	for _, n := range []string{a, b} {
		if !bucketRe.MatchString(n) {
			t.Errorf("invalid bucket name %q (len %d)", n, len(n))
		}
	}
}

func TestAccountID(t *testing.T) {
	inputs := [][3]string{
		{"tender", "shop-dev", "files"},
		{"", "a", "b"},
		{"tender", strings.Repeat("x", 63), strings.Repeat("y", 63)},
		{"", "1ns", "-"},
	}
	seen := map[string]bool{}
	for _, in := range inputs {
		id := AccountID(in[0], in[1], in[2])
		if !accountRe.MatchString(id) {
			t.Errorf("AccountID(%q) = %q, invalid", in, id)
		}
		if AccountID(in[0], in[1], in[2]) != id {
			t.Errorf("AccountID(%q) is not stable", in)
		}
		if seen[id] {
			t.Errorf("AccountID(%q) = %q collides", in, id)
		}
		seen[id] = true
	}
	// The hash covers the parts, not the joined string.
	if AccountID("", "a-b", "c") == AccountID("", "a", "b-c") {
		t.Error("ambiguous joins collided")
	}
}

func TestLabelValue(t *testing.T) {
	if got := LabelValue("Shop_Dev"); got != "shop-dev" {
		t.Errorf("LabelValue = %q", got)
	}
	if got := LabelValue(strings.Repeat("a", 80)); len(got) != 63 {
		t.Errorf("LabelValue length = %d", len(got))
	}
}

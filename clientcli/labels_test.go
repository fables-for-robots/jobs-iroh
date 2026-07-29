package clientcli

import "testing"

func TestLabelNode(t *testing.T) {
	node := nodeName("buildrun", 1)
	plain := shortNode(node)
	if got := labelNode(node, ""); got != plain {
		t.Fatalf("no label: %q, want %q", got, plain)
	}
	if got := labelNode(node, "apk_acl_libs"); got != "apk_acl_libs ("+plain+")" {
		t.Fatalf("labeled: %q", got)
	}
}

package amberiroh

import (
	"testing"

	"github.com/tmc/go-iroh/relay"
)

func TestFromFlagEmptyIsDefault(t *testing.T) {
	m, err := FromFlag("")
	if err != nil {
		t.Fatal(err)
	}
	if m != relay.ModeDefault() {
		t.Fatalf("empty flag: got %v, want default mode", m)
	}
}

func TestFromFlagCustomURL(t *testing.T) {
	m, err := FromFlag("https://euc1-1.relay.n0.iroh-canary.iroh.link./")
	if err != nil {
		t.Fatal(err)
	}
	if m == relay.ModeDefault() {
		t.Fatal("custom URL must not yield the default mode")
	}
}

func TestFromFlagBadURL(t *testing.T) {
	if _, err := FromFlag("::not a url::"); err == nil {
		t.Fatal("want error for unparsable relay URL")
	}
}

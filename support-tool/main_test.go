// The banner the tool prints when it starts.

package main

import (
	"strings"
	"testing"
)

func TestTheBannerSaysWhereToOpenAndHowToCollect(t *testing.T) {
	b := banner("127.0.0.1:8099")
	for _, want := range []string{
		"http://127.0.0.1:8099/",
		"curl -s -L https://install.simplyblock.io/scripts/discovery-bundler | bash",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("the banner does not carry %q:\n%s", want, b)
		}
	}
}

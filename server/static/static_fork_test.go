package static

import (
	"strconv"
	"strings"
	"testing"
)

// Upstream serves every static folder with six months of unconditional caching
// and no validator. That is correct for upstream: Vite names each chunk after a
// hash of its contents, so a URL under /assets/ is genuinely immutable.
//
// This fork rewrites chunks after the frontend is built, so /assets/ URLs are
// no longer content-addressed -- two releases can serve different bytes at the
// same path. With upstream's header a corrected bundle never reaches a browser
// that has been here before, which already happened twice: a translation fix
// shipped, was verified with curl, and was invisible in the browser.
//
// A rebase that quietly restores the shared constant would put it back, and
// nothing else would notice until the next patch failed to show up. Hence a
// test on the property rather than on the literal.
func TestAssetsAreNotServedAsImmutable(t *testing.T) {
	const hour = 3600

	assets := maxAgeOf(t, assetCacheControl("assets"))
	if assets > hour {
		t.Errorf(
			"/assets/ is cached for %ds; a patched bundle would not reach a returning browser for that long",
			assets,
		)
	}

	// The folders we never rewrite should keep upstream's caching. Shortening
	// those buys nothing and costs bandwidth on every load.
	for _, folder := range []string{"images", "streamer", "static"} {
		if got := maxAgeOf(t, assetCacheControl(folder)); got <= assets {
			t.Errorf(
				"/%s/ is cached for %ds, no longer than /assets/; only /assets/ is rewritten after the build",
				folder, got,
			)
		}
	}
}

func maxAgeOf(t *testing.T, header string) int {
	t.Helper()
	_, value, ok := strings.Cut(header, "max-age=")
	if !ok {
		t.Fatalf("no max-age in %q", header)
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("max-age in %q: %v", header, err)
	}
	return seconds
}

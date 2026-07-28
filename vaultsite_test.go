package main

import (
	"strings"
	"testing"
)

// The vault's site key must match the server's devicehub.normalizeDomain
// exactly. When the two disagree the failure is SILENT: the server pings for a
// name the vault does not hold, the relay declines "no credential stored", and
// the login proceeds without one. Nothing logs an error, so the credential is
// simply unreachable forever.
func TestVaultSiteMatchesTheServersNormalization(t *testing.T) {
	// Left is what a user might type or an old build might have stored; right
	// is what the server will ping with.
	for _, tc := range []struct{ stored, pinged string }{
		{"www.example.com", "example.com"},
		{"example.com", "example.com"},
		{"WWW.Example.COM", "example.com"},
		{"https://www.example.com/login", "example.com"},
		{"mail.google.com", "mail.google.com"},
		// A subdomain that merely STARTS with www must not be mangled.
		{"wwwtest.example.com", "wwwtest.example.com"},
	} {
		got, err := normalizeVaultSite(tc.stored)
		if err != nil {
			t.Fatalf("normalizeVaultSite(%q): %v", tc.stored, err)
		}
		if got != tc.pinged {
			t.Errorf("normalizeVaultSite(%q) = %q, want %q", tc.stored, got, tc.pinged)
		}
	}
}

// THE REGRESSION. A credential stored with a www. prefix was unreachable.
func TestAWwwCredentialAnswersAStrippedPing(t *testing.T) {
	stored, err := normalizeVaultSite("www.example.com")
	if err != nil {
		t.Fatal(err)
	}
	vf := vaultFile{Version: 1, Records: []vaultRecord{{Site: stored}}}
	if findVaultRecord(vf, "example.com") < 0 {
		t.Fatal("a ping for example.com found nothing; the login gets no credential and no error")
	}
}

// A vault written by an EARLIER build holds the host verbatim. Those records
// must keep resolving: we normalise at lookup rather than rewriting someone's
// credential store on upgrade, because a bad migration loses secrets.
func TestALegacyVerbatimRecordStillResolves(t *testing.T) {
	legacy := vaultFile{Version: 1, Records: []vaultRecord{
		{Site: "www.Example.com"}, // exactly as an old build would have written it
	}}
	for _, ping := range []string{"example.com", "www.example.com", "EXAMPLE.com"} {
		if findVaultRecord(legacy, ping) < 0 {
			t.Errorf("legacy record did not resolve for a ping of %q", ping)
		}
	}
}

// Matching must stay EXACT on the canonical form. A tolerant matcher that
// reached across domains would hand one site's password to another.
func TestLookupDoesNotMatchADifferentSite(t *testing.T) {
	vf := vaultFile{Version: 1, Records: []vaultRecord{{Site: "example.com"}}}
	for _, ping := range []string{
		"notexample.com",
		"example.com.evil.test",
		"evil-example.com",
		"sub.example.com", // a subdomain is a DIFFERENT credential, not this one
		"",
	} {
		if idx := findVaultRecord(vf, ping); idx >= 0 {
			t.Errorf("a ping for %q matched the record for example.com", ping)
		}
	}
}

// The advertised inventory drives which domain the server pings with, so it
// must carry the same canonical form the vault looks up -- and must not list
// one domain twice when an old vault holds both spellings.
func TestAdvertisedInventoryIsCanonicalAndDeduped(t *testing.T) {
	vf := vaultFile{Version: 1, Records: []vaultRecord{
		{Site: "www.example.com"}, {Site: "example.com"}, {Site: "Mail.Google.com"},
	}}
	seen := map[string]bool{}
	var domains []string
	for _, r := range vf.Records {
		d := canonicalVaultSite(r.Site)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		domains = append(domains, d)
	}
	if len(domains) != 2 {
		t.Fatalf("advertised %v, want 2 deduped domains", domains)
	}
	for _, d := range domains {
		if d != canonicalVaultSite(d) {
			t.Errorf("advertised %q, which is not canonical", d)
		}
		if strings.HasPrefix(d, "www.") || strings.ToLower(d) != d {
			t.Errorf("advertised %q; the server will normalise it and ping a name we do not hold", d)
		}
	}
}

package ula

import (
	"net/netip"
	"strings"
	"testing"
)

func TestGeneratedPrefixIsAValidULA(t *testing.T) {
	p, err := GeneratePrefix()
	if err != nil {
		t.Fatalf("GeneratePrefix: %v", err)
	}
	a := p.Addr()
	if !a.Is6() {
		t.Fatalf("prefix address %v is not IPv6", a)
	}
	if !IsULA(a) {
		t.Errorf("prefix %v is not inside fd00::/8", a)
	}
	if !strings.HasSuffix(p.String(), "/64") {
		t.Errorf("prefix %q is not a /64", p)
	}
	// The interface half must be empty in the prefix itself.
	b := a.As16()
	for i := 8; i < 16; i++ {
		if b[i] != 0 {
			t.Errorf("prefix %v has a non-zero interface id", a)
			break
		}
	}
}

// RFC 4193 §3.2.2 requires the global id to be random, and a fixed one collides the moment two
// machines meet. There must be no default prefix anywhere in this package.
func TestPrefixesAreRandomPerInstallation(t *testing.T) {
	seen := map[string]bool{}
	const n = 50
	for i := 0; i < n; i++ {
		p, err := GeneratePrefix()
		if err != nil {
			t.Fatalf("GeneratePrefix: %v", err)
		}
		if seen[p.String()] {
			t.Fatalf("generated the same prefix twice (%s) - this is supposed to be random", p)
		}
		seen[p.String()] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct prefixes from %d generations", len(seen), n)
	}
}

// A specific prefix that must never be produced or assumed: it belongs to a person, not to this
// program. Hard-coding somebody's own range as a default is how a tool ends up squatting on it
// everywhere it is installed.
func TestNoHardCodedPrefixLeaksIn(t *testing.T) {
	const somebodysOwn = "fd00:2830::/64"
	for i := 0; i < 200; i++ {
		p, err := GeneratePrefix()
		if err != nil {
			t.Fatalf("GeneratePrefix: %v", err)
		}
		if p.String() == somebodysOwn {
			t.Fatalf("generated %s, which is a real person's prefix", somebodysOwn)
		}
		// Nor the lazy choices people reach for.
		for _, bad := range []string{"fd00:0:0:0::/64", "fd00::/64", "fd00:1::/64"} {
			if p.String() == bad {
				t.Fatalf("generated the placeholder prefix %s", bad)
			}
		}
	}
}

func TestPrefixRoundTrip(t *testing.T) {
	p, err := GeneratePrefix()
	if err != nil {
		t.Fatalf("GeneratePrefix: %v", err)
	}
	p.SubnetID = 7

	got, err := ParsePrefix(p.String())
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", p, err)
	}
	if got.String() != p.String() {
		t.Errorf("round trip changed the prefix: %s -> %s", p, got)
	}
	if got.SubnetID != 7 {
		t.Errorf("SubnetID = %d, want 7", got.SubnetID)
	}
}

func TestParsePrefixRejectsWhatItShould(t *testing.T) {
	cases := []struct {
		in, why string
	}{
		{"", "empty"},
		{"not a prefix", "gibberish"},
		{"192.168.1.0/24", "IPv4"},
		{"2001:db8::/64", "not a ULA"},
		{"fd12:3456:789a::/48", "not a /64"},
		{"fd12:3456:789a:b::/80", "not a /64"},
	}
	for _, c := range cases {
		if _, err := ParsePrefix(c.in); err == nil {
			t.Errorf("ParsePrefix(%q) was accepted; it is %s", c.in, c.why)
		}
	}
}

func TestParsePrefixErrorsExplainThemselves(t *testing.T) {
	_, err := ParsePrefix("2001:db8::/64")
	if err == nil {
		t.Fatal("a non-ULA prefix was accepted")
	}
	if !strings.Contains(err.Error(), "fd00::/8") {
		t.Errorf("error should say what the rule is, got: %v", err)
	}

	_, err = ParsePrefix("fd12:3456:789a::/48")
	if err == nil {
		t.Fatal("a /48 was accepted")
	}
	if !strings.Contains(err.Error(), "/64") {
		t.Errorf("error should say it wants a /64, got: %v", err)
	}
}

func TestAddressForIsStableAndInsideThePrefix(t *testing.T) {
	p, _ := GeneratePrefix()
	secret, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}

	a1 := p.AddressFor(secret, "web")
	a2 := p.AddressFor(secret, "web")
	if a1 != a2 {
		t.Errorf("the same service got two addresses: %v and %v", a1, a2)
	}
	if !p.Contains(a1) {
		t.Errorf("address %v is outside its own prefix %s", a1, p)
	}
	if a1 == p.Addr() {
		t.Error("a service was given the subnet-router anycast address")
	}
}

func TestDifferentServicesGetDifferentAddresses(t *testing.T) {
	p, _ := GeneratePrefix()
	secret, _ := NewSecret()

	seen := map[netip.Addr]string{}
	for _, name := range []string{"web", "db", "api", "cache", "worker", "a", "b", "ab"} {
		a := p.AddressFor(secret, name)
		if other, clash := seen[a]; clash {
			t.Errorf("%s and %s got the same address %v", name, other, a)
		}
		seen[a] = name
	}
}

// Deriving the address from the name alone would make every installation's addresses guessable
// from the outside. The installation secret is what stops that.
func TestAddressesDifferBetweenInstallations(t *testing.T) {
	p, _ := GeneratePrefix()
	s1, _ := NewSecret()
	s2, _ := NewSecret()

	if p.AddressFor(s1, "web") == p.AddressFor(s2, "web") {
		t.Error("the same service name gives the same address under two different secrets; " +
			"addresses would be guessable from the name alone")
	}
}

// ("ab","c") and ("a","bc") must not collide, which is what the length prefixing is for.
func TestHashInputsCannotBeConfused(t *testing.T) {
	p, _ := GeneratePrefix()
	a := p.AddressFor([]byte("ab"), "c")
	b := p.AddressFor([]byte("a"), "bc")
	if a == b {
		t.Error("concatenation collision: two different (secret, service) pairs hash to one address")
	}
}

// Addresses built from a hash are locally administered; claiming otherwise in the EUI-64 sense is
// a small lie that confuses anything reading the address.
func TestInterfaceIDIsMarkedLocallyAdministered(t *testing.T) {
	p, _ := GeneratePrefix()
	secret, _ := NewSecret()
	for _, name := range []string{"web", "db", "api", "x", "y", "z"} {
		b := p.AddressFor(secret, name).As16()
		if b[8]&0x02 != 0 {
			t.Errorf("%s: interface id claims to be universally administered (byte 8 = %#02x)", name, b[8])
		}
	}
}

func TestContainsIsNotFooledByANeighbour(t *testing.T) {
	p1, _ := GeneratePrefix()
	p2, _ := GeneratePrefix()
	secret, _ := NewSecret()

	a := p1.AddressFor(secret, "web")
	if !p1.Contains(a) {
		t.Error("a prefix does not contain its own address")
	}
	if p2.Contains(a) {
		t.Error("a different prefix claims to contain another prefix's address")
	}
	if p1.Contains(netip.MustParseAddr("2001:db8::1")) {
		t.Error("Contains accepted an address from a different range entirely")
	}
	if p1.Contains(netip.MustParseAddr("127.0.0.1")) {
		t.Error("Contains accepted an IPv4 address")
	}
}

func TestSecretRoundTrip(t *testing.T) {
	s, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	got, err := DecodeSecret(EncodeSecret(s))
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	if string(got) != string(s) {
		t.Error("the secret did not survive the round trip")
	}
}

func TestShortOrInvalidSecretsAreRefused(t *testing.T) {
	if _, err := DecodeSecret("not hex at all"); err == nil {
		t.Error("a non-hex secret was accepted")
	}
	if _, err := DecodeSecret("aabb"); err == nil {
		t.Error("a 2-byte secret was accepted; it should demand at least 16")
	}
}

// The setup step needs root, so it is printed for the operator rather than run. Design rule 4.
func TestSetupCommandIsSomethingYouCanPaste(t *testing.T) {
	p, _ := GeneratePrefix()
	cmd := p.SetupCommand()
	if !strings.Contains(cmd, p.String()) {
		t.Errorf("the setup command does not mention the prefix: %q", cmd)
	}
	if !strings.Contains(cmd, "ip -6 route add local") || !strings.Contains(cmd, "dev lo") {
		t.Errorf("the setup command is not the expected one: %q", cmd)
	}
}

// A freshly generated prefix has no route, and RouteInstalled must say so. This is the question
// people actually mean when they ask whether the setup worked.
func TestRouteInstalledIsFalseForAFreshPrefix(t *testing.T) {
	p, _ := GeneratePrefix()
	installed, err := p.RouteInstalled()
	if err != nil {
		t.Skipf("cannot read the routing table here: %v", err)
	}
	if installed {
		t.Errorf("a brand new random prefix (%s) already has a local route", p)
	}
}

// Bindable is deliberately NOT a setup check, and this test exists to stop anyone turning it back
// into one. On a machine with net.ipv6.ip_nonlocal_bind=1 a bind succeeds for any address at all,
// so "it bound, therefore it is set up" is exactly the false positive this package must not ship.
func TestBindableIsNotASetupCheck(t *testing.T) {
	p, _ := GeneratePrefix()
	secret, _ := NewSecret()

	bindable, _ := p.Bindable(secret)
	installed, err := p.RouteInstalled()
	if err != nil {
		t.Skipf("cannot read the routing table here: %v", err)
	}
	if installed {
		t.Fatalf("precondition failed: a random prefix %s somehow has a route", p)
	}
	if bindable && !NonLocalBindEnabled() {
		t.Errorf("an unrouted prefix was bindable although ip_nonlocal_bind is off; "+
			"that would make Bindable meaningless as written (%s)", p)
	}
	// The important assertion: whatever bind says, Ready must not be fooled by it.
	ready, why := p.Ready()
	if ready {
		t.Errorf("Ready() says an unrouted prefix is usable (bindable=%v)", bindable)
	}
	if !strings.Contains(why, "ip -6 route add local") {
		t.Errorf("Ready() did not tell the operator what to run; got: %q", why)
	}
}

// When binding lies, the explanation has to warn about it, or somebody will test with a bind, see
// it succeed, and conclude the setup is done.
func TestReadyWarnsWhenBindingWouldMislead(t *testing.T) {
	if !NonLocalBindEnabled() {
		t.Skip("ip_nonlocal_bind is off here, so there is nothing to warn about")
	}
	p, _ := GeneratePrefix()
	ready, why := p.Ready()
	if ready {
		t.Fatalf("a fresh prefix reports ready: %s", p)
	}
	if !strings.Contains(why, "ip_nonlocal_bind") {
		t.Errorf("the explanation does not mention that binding will appear to work; got: %q", why)
	}
}

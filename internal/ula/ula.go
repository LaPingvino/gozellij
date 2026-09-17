// Package ula gives each service its own IPv6 address instead of its own port.
//
// The idea, which predates this program: on a single host, ports are a flat, contended, guessable
// namespace. Everybody wants 8080, two services cannot share it, and "which thing is on 3000
// today" is a question people genuinely have to look up. Addresses are not scarce - a /64 holds
// 2^64 of them - so give every service its own address and let them all listen on the port that
// suits them. The address becomes the service's identity.
//
// The prefix is a Unique Local Address range (RFC 4193): fd00::/8 plus a 40-bit global ID that
// RFC 4193 §3.2.2 requires to be generated *randomly*, per installation. This matters and is
// easy to get wrong: a hard-coded prefix would collide the moment two machines are connected, and
// "everyone gets fd00:1::/64" is how you end up with a standard that nobody can route between.
// There is deliberately no default prefix in this package - GeneratePrefix or nothing.
//
// Setting it up needs root exactly once, and never at run time (design rule 4):
//
//	ip -6 route add local fdXX:XXXX:XXXX::/64 dev lo
//
// after which any process can bind any address in the range without privileges.
package ula

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Prefix is a /64 Unique Local Address prefix belonging to one installation.
type Prefix struct {
	// GlobalID is the 40 random bits from RFC 4193, held as 5 bytes.
	GlobalID [5]byte
	// SubnetID is the 16-bit subnet, giving the /64.
	SubnetID uint16
}

// GeneratePrefix makes a new random prefix.
//
// crypto/rand rather than math/rand: RFC 4193 asks for a globally unique value, and a predictable
// one would make every installation's addresses guessable from the outside - which is exactly the
// property the per-service interface IDs below are trying to avoid.
func GeneratePrefix() (Prefix, error) {
	var p Prefix
	if _, err := rand.Read(p.GlobalID[:]); err != nil {
		return Prefix{}, fmt.Errorf("generating a ULA global id: %w", err)
	}
	return p, nil
}

// Addr returns the prefix's base address, e.g. fdxx:xxxx:xxxx:0::.
func (p Prefix) Addr() netip.Addr {
	var b [16]byte
	b[0] = 0xfd
	copy(b[1:6], p.GlobalID[:])
	binary.BigEndian.PutUint16(b[6:8], p.SubnetID)
	return netip.AddrFrom16(b)
}

// String is the CIDR form, which is what you paste into `ip -6 route add local`.
func (p Prefix) String() string { return p.Addr().String() + "/64" }

// IsULA reports whether an address is inside fd00::/8, the locally-assigned ULA range.
func IsULA(a netip.Addr) bool {
	if !a.Is6() {
		return false
	}
	return a.As16()[0] == 0xfd
}

// Contains reports whether an address falls inside this prefix.
func (p Prefix) Contains(a netip.Addr) bool {
	if !a.Is6() {
		return false
	}
	base, addr := p.Addr().As16(), a.As16()
	for i := 0; i < 8; i++ {
		if base[i] != addr[i] {
			return false
		}
	}
	return true
}

// ParsePrefix reads a prefix back from its CIDR form.
func ParsePrefix(s string) (Prefix, error) {
	pfx, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return Prefix{}, fmt.Errorf("not a valid prefix %q: %w", s, err)
	}
	if pfx.Bits() != 64 {
		return Prefix{}, fmt.Errorf("prefix %s is a /%d; this wants a /64", s, pfx.Bits())
	}
	a := pfx.Addr()
	if !IsULA(a) {
		return Prefix{}, fmt.Errorf("prefix %s is not a unique local address (must be inside fd00::/8)", s)
	}
	var p Prefix
	b := a.As16()
	copy(p.GlobalID[:], b[1:6])
	p.SubnetID = binary.BigEndian.Uint16(b[6:8])
	return p, nil
}

// AddressFor derives a service's address inside this prefix.
//
// The interface id is derived from the installation's own secret and the service's *identity*,
// not from its name. Deriving it from the name would make every address on every installation
// guessable - "what is the address of the postgres on that host" would have one answer, forever,
// for everybody. The secret makes the mapping local to this installation, so a name that leaks
// tells an outsider nothing about where to find it.
func (p Prefix) AddressFor(secret []byte, serviceID string) netip.Addr {
	h := sha256.New()
	// Length-prefix the parts, so ("ab","c") and ("a","bc") cannot hash to the same address.
	writeChunk(h, secret)
	writeChunk(h, []byte(serviceID))
	sum := h.Sum(nil)

	var b [16]byte
	base := p.Addr().As16()
	copy(b[0:8], base[0:8])
	copy(b[8:16], sum[0:8])

	// Clear the universal/local bit in the interface id (bit 6 of the first byte). Addresses
	// built from a hash are locally administered, not derived from hardware, and saying
	// otherwise in the EUI-64 sense is simply untrue.
	b[8] &^= 0x02

	// The all-zero interface id is the subnet-router anycast address and must not be handed to
	// a service. Astronomically unlikely, cheap to exclude, and a bug that would surface once
	// in a decade is worse than one that surfaces every time.
	allZero := true
	for i := 8; i < 16; i++ {
		if b[i] != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		b[15] = 1
	}
	return netip.AddrFrom16(b)
}

func writeChunk(h interface{ Write([]byte) (int, error) }, b []byte) {
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(b)))
	_, _ = h.Write(l[:])
	_, _ = h.Write(b)
}

// NewSecret makes an installation secret for AddressFor.
func NewSecret() ([]byte, error) {
	s := make([]byte, 32)
	if _, err := rand.Read(s); err != nil {
		return nil, fmt.Errorf("generating an installation secret: %w", err)
	}
	return s, nil
}

// EncodeSecret and DecodeSecret move the secret to and from the config file.
func EncodeSecret(s []byte) string { return hex.EncodeToString(s) }

func DecodeSecret(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("the installation secret is not valid hex: %w", err)
	}
	if len(b) < 16 {
		return nil, fmt.Errorf("the installation secret is %d bytes; want at least 16", len(b))
	}
	return b, nil
}

// SetupCommand is the one-time, root-only step that makes a prefix usable.
//
// Returned as a string to print rather than executed: design rule 4 says anything needing root is
// a documented step the operator takes, never something the binary does behind their back.
func (p Prefix) SetupCommand() string {
	return fmt.Sprintf("sudo ip -6 route add local %s dev lo", p)
}

// Bindable reports whether an address in this prefix can be bound right now.
//
// This is *not* the same as "the prefix is set up", and the difference is a trap worth spelling
// out. With net.ipv6.ip_nonlocal_bind=1 - which is set on real machines, including the one this
// was written on - a bind succeeds for any address whatsoever, routed or not. A setup check built
// on binding alone therefore reports success for a prefix that does not work, and the operator
// finds out when traffic goes nowhere. Use RouteInstalled for the question people actually mean.
func (p Prefix) Bindable(secret []byte) (bool, error) {
	addr := p.AddressFor(secret, "gozellij-probe")
	l, err := net.Listen("tcp", "["+addr.String()+"]:0")
	if err != nil {
		return false, err
	}
	_ = l.Close()
	return true, nil
}

// NonLocalBindEnabled reports whether the kernel allows binding addresses that are not local.
//
// Exposed so a caller can interpret Bindable correctly rather than being quietly misled by it.
func NonLocalBindEnabled() bool {
	b, err := os.ReadFile("/proc/sys/net/ipv6/ip_nonlocal_bind")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "1"
}

// RouteInstalled reports whether a local route for this prefix exists - the thing
// `ip -6 route add local <prefix> dev lo` creates, and the thing that actually makes the addresses
// work.
//
// It reads /proc/net/ipv6_route rather than shelling out to `ip`, so there is no output format to
// parse loosely and no dependency on iproute2 being installed.
func (p Prefix) RouteInstalled() (bool, error) {
	f, err := os.Open("/proc/net/ipv6_route")
	if err != nil {
		// Not Linux, or /proc is not mounted. Say so rather than reporting "no route", which
		// would read as a definite answer to a question we could not ask.
		return false, fmt.Errorf("cannot read the routing table: %w", err)
	}
	defer f.Close()

	// Each line begins with the 32-hex-digit destination and its prefix length in hex.
	want := hex.EncodeToString(p.Addr().AsSlice())
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		if fields[0] != want {
			continue
		}
		if plen, perr := strconv.ParseUint(fields[1], 16, 16); perr == nil && plen == 64 {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("reading the routing table: %w", err)
	}
	return false, nil
}

// Ready reports whether this prefix is usable, and explains itself when it is not.
//
// The explanation is the point: "not ready" with no reason sends somebody to a search engine,
// while the exact command to run sends them to a working setup.
func (p Prefix) Ready() (bool, string) {
	installed, err := p.RouteInstalled()
	if err != nil {
		return false, fmt.Sprintf("could not check the routing table (%v); the one-time setup is:\n  %s",
			err, p.SetupCommand())
	}
	if installed {
		return true, ""
	}
	msg := fmt.Sprintf("no local route for %s. Run this once, as root:\n  %s", p, p.SetupCommand())
	if NonLocalBindEnabled() {
		// Without this note, somebody will test with a bind, see it succeed, and conclude the
		// setup worked.
		msg += "\n  (note: net.ipv6.ip_nonlocal_bind is on, so binding these addresses will " +
			"appear to work even without the route - but nothing will reach them)"
	}
	return false, msg
}

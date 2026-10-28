package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/jackc/pgx/v5"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

// caddyLabel is the belune-system label value on the Caddy container.
const caddyLabel = "caddy"

// TrustedProxyOverlap says that a trusted range overlaps a Docker network the
// proxy is attached to.
//
// That is the dangerous kind of overlap and it is not hypothetical: Caddy sits on
// every project network, so a container on one can open a connection to it. If its
// address is in a trusted range, whatever it puts in X-Forwarded-For is believed —
// into WAF detections and, with autoban on, into bans against an address of its
// choosing. Private ranges are NOT refused for this (an internal load balancer in
// front of Caddy is a real topology, and RFC 1918 is where it lives); what is
// worth saying loudly is the specific, checkable case.
type TrustedProxyOverlap struct {
	CIDR    string `json:"cidr"`
	Network string `json:"network"`
	Subnet  string `json:"subnet"`
}

// TrustedProxyNetwork is an address range of a Docker network the proxy is attached
// to: the ranges a trusted proxy must not cover.
type TrustedProxyNetwork struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
}

// TrustedProxies is the setting, with what is wrong with it that Belune can see.
type TrustedProxies struct {
	CIDRs    []string              `json:"cidrs"`
	Overlaps []TrustedProxyOverlap `json:"overlaps"`
	// Networks are the ranges the overlap is judged against, returned so the form
	// can say whether a range being typed would overlap one BEFORE it is saved. The
	// only other way to see the warning was to create the condition it warns about.
	Networks []TrustedProxyNetwork `json:"networks"`
	// OverlapsChecked is false when Belune could not list its networks. "No
	// overlaps" and "could not look" are different answers.
	OverlapsChecked bool `json:"overlaps_checked"`
}

// TrustedProxyValidationError is a submitted list that was refused, and why.
type TrustedProxyValidationError struct{ Reason string }

func (e *TrustedProxyValidationError) Error() string { return e.Reason }

// TrustedProxyService owns the list of proxies in front of Caddy.
type TrustedProxyService struct {
	queries *generated.Queries
	rts     runtime.Runtimes
}

func NewTrustedProxyService(queries *generated.Queries, rts runtime.Runtimes) *TrustedProxyService {
	return &TrustedProxyService{queries: queries, rts: rts}
}

// Get returns the stored list. A stored value that no longer validates is an
// error here too, rather than an empty list that hides it.
func (s *TrustedProxyService) Get(ctx context.Context) (TrustedProxies, error) {
	cidrs, err := s.stored(ctx)
	if err != nil {
		return TrustedProxies{}, err
	}
	return s.describe(ctx, cidrs), nil
}

// Set validates and stores a list, returning the previous one for the audit trail.
// Validation is the same function the reconciler applies on the way out, so a
// list saved here is a list the reconciler will accept.
func (s *TrustedProxyService) Set(ctx context.Context, entries []string) (before []string, after TrustedProxies, err error) {
	normalized, verr := proxy.NormalizeTrustedProxies(entries)
	if verr != nil {
		return nil, TrustedProxies{}, &TrustedProxyValidationError{Reason: verr.Error()}
	}
	// A previous value that cannot be read is not a reason to refuse a new one:
	// replacing a bad stored value is how it gets fixed.
	if prev, perr := s.stored(ctx); perr == nil {
		before = prev
	} else {
		slog.Warn("trusted proxies: the stored value could not be read; replacing it", "error", perr)
	}
	if _, err := s.queries.UpsertSetting(ctx, generated.UpsertSettingParams{
		Key: proxy.SettingTrustedProxies, Value: proxy.EncodeTrustedProxies(normalized),
	}); err != nil {
		return nil, TrustedProxies{}, fmt.Errorf("saving the trusted proxies: %w", err)
	}
	return before, s.describe(ctx, normalized), nil
}

func (s *TrustedProxyService) stored(ctx context.Context) ([]string, error) {
	row, err := s.queries.GetSetting(ctx, proxy.SettingTrustedProxies)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the trusted proxies: %w", err)
	}
	return proxy.DecodeTrustedProxies(row.Value)
}

func (s *TrustedProxyService) describe(ctx context.Context, cidrs []string) TrustedProxies {
	out := TrustedProxies{CIDRs: cidrs, Overlaps: []TrustedProxyOverlap{}, Networks: []TrustedProxyNetwork{}}
	if out.CIDRs == nil {
		out.CIDRs = []string{}
	}
	// Looked up even for an empty list: the networks are what lets the form warn
	// about a range before it is saved.
	nets, ok := s.proxyNetworks(ctx)
	out.Networks, out.OverlapsChecked = nets, ok
	out.Overlaps = overlapsOf(cidrs, nets)
	return out
}

// proxyNetworks lists the address ranges of the Docker networks the proxy is
// attached to. The second result is false when they could not be listed.
func (s *TrustedProxyService) proxyNetworks(ctx context.Context) ([]TrustedProxyNetwork, bool) {
	none := []TrustedProxyNetwork{}
	rt, err := s.rts.Local(ctx)
	if err != nil {
		return none, false
	}
	system, err := rt.ListSystemContainers(ctx)
	if err != nil {
		return none, false
	}
	caddy := map[string]struct{}{}
	for _, c := range system {
		if c.Labels["belune-system"] == caddyLabel {
			caddy[c.ID] = struct{}{}
			caddy[c.Name] = struct{}{}
		}
	}
	nets, err := rt.ListNetworks(ctx)
	if err != nil {
		return none, false
	}

	out := []TrustedProxyNetwork{}
	for _, n := range nets {
		attached := false
		for _, c := range n.Containers {
			_, byID := caddy[c.ID]
			_, byName := caddy[c.Name]
			if byID || byName {
				attached = true
				break
			}
		}
		if !attached {
			continue
		}
		for _, sn := range n.Subnets {
			if _, err := netip.ParsePrefix(sn); err == nil {
				out = append(out, TrustedProxyNetwork{Name: n.Name, Subnet: sn})
			}
		}
	}
	return out, true
}

// overlapsOf finds trusted ranges that overlap a network the proxy is attached to.
func overlapsOf(cidrs []string, nets []TrustedProxyNetwork) []TrustedProxyOverlap {
	out := []TrustedProxyOverlap{}
	for _, n := range nets {
		subnet, err := netip.ParsePrefix(n.Subnet)
		if err != nil {
			continue
		}
		for _, c := range cidrs {
			if p, err := netip.ParsePrefix(c); err == nil && p.Overlaps(subnet) {
				out = append(out, TrustedProxyOverlap{CIDR: c, Network: n.Name, Subnet: n.Subnet})
			}
		}
	}
	return out
}

package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/store/generated"
)

// FeatureDNSChallenge is the Degradation.Feature for a dns_challenge domain that
// is being served without its DNS-01 policy.
const FeatureDNSChallenge = "dns_challenge"

// ReasonNoProvider marks a dns_challenge domain with no DNS provider attached:
// a row from before the mode was withdrawn, or one whose provider was never
// chosen. Not a Caddy limitation, so the advice differs from module_absent.
const ReasonNoProvider = "no_provider"

// PlanDNSPolicies turns the dns_challenge domains of running applications into
// the automation policies Caddy should hold, plus a Degradation for each domain
// that cannot have one.
//
// It is shared by the reconciler and the domain handlers on purpose: two callers
// that each derived the set would flip-flop the policy list between them.
//
// A domain is degraded rather than dropped when its provider's module is missing
// or it has no provider; its route stays, so "a hostname is in Caddy's route
// table iff it is in the skip computation" is untouched. caps may be nil (a Caddy
// we do not probe), in which case nothing is withheld on module grounds.
func PlanDNSPolicies(ctx context.Context, q *generated.Queries, dec Decryptor, caps capability.Provider) ([]DNSPolicy, []Degradation, error) {
	rows, err := q.ListDNSChallengeDomains(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list dns challenge domains: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}

	var have capability.Set
	reason := ReasonModuleAbsent
	if caps != nil {
		have = caps.Capabilities(ctx)
		if !have.Known() {
			reason = ReasonModulesUnknown
		}
	}

	byProvider := map[string]*DNSPolicy{}
	var degraded []Degradation
	var firstErr error
	for _, row := range rows {
		degrade := func(module, why string) {
			degraded = append(degraded, Degradation{
				Hostname: row.Hostname, Path: row.Path, Feature: FeatureDNSChallenge, Module: module, Reason: why,
			})
		}
		if !row.ProviderID.Valid {
			degrade("", ReasonNoProvider)
			continue
		}
		spec := DNSProviders[row.Provider.String]
		if caps != nil && !have.Has(spec.Module) {
			degrade(spec.Module, reason)
			continue
		}

		id := uuid.UUID(row.ProviderID.Bytes).String()
		pol := byProvider[id]
		if pol == nil {
			creds, err := DecryptDNSCredentials(dec, row.CredentialsEncrypted)
			if err != nil {
				// One unreadable provider must not take the others' issuance with it.
				slog.Warn("proxy: skipping dns provider whose credentials cannot be read", "provider_id", id, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				degrade(spec.Module, "credentials_unreadable")
				continue
			}
			pol = &DNSPolicy{ProviderID: id, Provider: spec.Name, Credentials: creds}
			byProvider[id] = pol
		}
		// A hostname split across paths is one certificate name.
		if !containsString(pol.Subjects, row.Hostname) {
			pol.Subjects = append(pol.Subjects, row.Hostname)
		}
	}

	policies := make([]DNSPolicy, 0, len(byProvider))
	for _, p := range byProvider {
		sort.Strings(p.Subjects)
		policies = append(policies, *p)
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].ProviderID < policies[j].ProviderID })
	return policies, degraded, firstErr
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

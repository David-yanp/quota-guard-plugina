package main

import (
	"fmt"
	"strings"
	"time"
)

func cloneExclusiveOwner(owner *exclusiveOwnerState) *exclusiveOwnerState {
	if owner == nil {
		return nil
	}
	copyOwner := *owner
	return &copyOwner
}

func (g *quotaGuard) exclusiveConfigLocked(account *accountState) (exclusiveAuthConfig, bool) {
	if account == nil || len(g.cfg.ClientAffinityExclusiveAuths) == 0 {
		return exclusiveAuthConfig{}, false
	}
	for selector, policy := range g.cfg.ClientAffinityExclusiveAuths {
		if strings.TrimSpace(selector) == strings.TrimSpace(account.AuthID) || strings.TrimSpace(selector) == strings.TrimSpace(account.AuthIndex) {
			if policy.MaxClients <= 0 {
				policy.MaxClients = 1
			}
			if policy.IdleReleaseSeconds <= 0 {
				policy.IdleReleaseSeconds = 3600
			}
			return policy, true
		}
	}
	return exclusiveAuthConfig{}, false
}

func (g *quotaGuard) exclusiveOwnerKeyLocked(account *accountState) string {
	if account == nil {
		return ""
	}
	if account.AuthID != "" {
		return account.AuthID
	}
	return account.AuthIndex
}

func (g *quotaGuard) exclusiveEligibilityLocked(account *accountState, clientID string, now time.Time) (bool, string) {
	policy, ok := g.exclusiveConfigLocked(account)
	if !ok {
		return true, ""
	}
	clientID = strings.TrimSpace(clientID)
	key := g.exclusiveOwnerKeyLocked(account)
	if key == "" {
		return false, "exclusive account has no identity"
	}
	if g.state.ExclusiveOwners == nil {
		g.state.ExclusiveOwners = map[string]*exclusiveOwnerState{}
	}
	owner := g.state.ExclusiveOwners[key]
	if owner != nil && owner.ClientID != "" && owner.LeaseExpiresAt.After(now) {
		if owner.ClientID == clientID {
			return true, ""
		}
		return false, fmt.Sprintf("exclusive owner %s until %s", owner.ClientID, owner.LeaseExpiresAt.Format(time.RFC3339))
	}
	if clientID == "" && !policy.AllowClientlessClaim {
		return false, "exclusive account requires client id"
	}
	return true, ""
}

func (g *quotaGuard) claimExclusiveLocked(account *accountState, clientID string, now time.Time) {
	policy, ok := g.exclusiveConfigLocked(account)
	if !ok || strings.TrimSpace(clientID) == "" {
		return
	}
	if g.state.ExclusiveOwners == nil {
		g.state.ExclusiveOwners = map[string]*exclusiveOwnerState{}
	}
	key := g.exclusiveOwnerKeyLocked(account)
	lease := time.Duration(policy.IdleReleaseSeconds) * time.Second
	g.state.ExclusiveOwners[key] = &exclusiveOwnerState{
		AuthID: account.AuthID, AuthIndex: account.AuthIndex, ClientID: clientID,
		LastActivityAt: now, LeaseExpiresAt: now.Add(lease),
	}
}

func (g *quotaGuard) touchExclusiveOwnerLocked(account *accountState, clientID string, now time.Time) {
	policy, ok := g.exclusiveConfigLocked(account)
	if !ok || strings.TrimSpace(clientID) == "" {
		return
	}
	key := g.exclusiveOwnerKeyLocked(account)
	owner := g.state.ExclusiveOwners[key]
	if owner == nil || owner.ClientID != clientID {
		g.claimExclusiveLocked(account, clientID, now)
		return
	}
	owner.LastActivityAt = now
	owner.LeaseExpiresAt = now.Add(time.Duration(policy.IdleReleaseSeconds) * time.Second)
}

func (g *quotaGuard) clientHasActiveExclusiveLeaseLocked(clientID string, now time.Time) bool {
	for _, owner := range g.state.ExclusiveOwners {
		if owner != nil && owner.ClientID == clientID && owner.LeaseExpiresAt.After(now) {
			return true
		}
	}
	return false
}

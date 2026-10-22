package cmd

import (
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/witness"
)

// The enrollment policies witness serve --enrollment-policy names. The licensed policy is the one
// the KordLoom-operated witness runs. The open policy is for a company running its own push witness
// on a private network, where who can reach the witness at all is the control.
const (
	// policyLicensed enrolls an install only under a license that includes the hosted witness.
	policyLicensed = "licensed"
	// policyOpen enrolls every install whose request is signed by the key it enrolls.
	policyOpen = "open"
)

// enrollmentPolicy returns the policy a name selects, reading the clock through now.
func enrollmentPolicy(name string, now func() time.Time) (witness.EnrollmentPolicy, error) {
	switch name {
	case policyLicensed:
		return licensedEnrollment(now), nil
	case policyOpen:
		return witness.OpenEnrollment, nil
	}
	return nil, fmt.Errorf("--enrollment-policy %q is not a policy; use %s or %s", name,
		policyLicensed, policyOpen)
}

// licensedEnrollment returns the policy that enrolls an install only under the license file its
// request carries: one that verifies against the keys compiled into this build, includes the hosted
// witness, and is within its term. It hands the witness the license id and the term and nothing
// else, so the organization the license names never reaches the witness's state.
func licensedEnrollment(now func() time.Time) witness.EnrollmentPolicy {
	return func(r witness.EnrollRequest) (witness.Entitlement, error) {
		if len(r.License) == 0 {
			return witness.Entitlement{}, fmt.Errorf("the enrollment carries no license, and this " +
				"witness enrolls licensed installs only")
		}
		l, err := license.Verify(r.License, "")
		if err != nil {
			return witness.Entitlement{}, fmt.Errorf("the license does not verify: %w", err)
		}
		if !l.Covers(license.FeatureHostedWitness) {
			tier := l.Claims.Tier
			if tier == "" {
				tier = license.TierTeam
			}
			return witness.Entitlement{}, fmt.Errorf("the hosted witness needs an Enterprise license "+
				"or the Team add-on, and this %s license does not include it", tier)
		}
		if l.Claims.ID == "" {
			return witness.Entitlement{}, fmt.Errorf("the license names no id, so its installs " +
				"cannot be counted")
		}
		expires, err := time.Parse(time.RFC3339, l.Claims.Expires)
		if err != nil {
			return witness.Entitlement{}, fmt.Errorf("the license term does not parse: %w", err)
		}
		if now().After(expires) {
			return witness.Entitlement{}, fmt.Errorf("the license term ended %s",
				l.Claims.Expires)
		}
		return witness.Entitlement{LicenseID: l.Claims.ID, Expires: expires}, nil
	}
}

package config

import (
	"slices"

	"github.com/TechXTT/reelay/internal/model"
)

// ToModel converts a seed profile from the config file into the domain type.
//
// config depending on model is the allowed direction: model imports nothing
// internal, so there is no cycle. The alternative — duplicating the profile
// shape in two packages and converting by hand at each call site — is how the
// two drift apart.
//
// Used by the phase 6 first-run seeding and by the --search CLI, which needs a
// profile before any database exists.
func (p Profile) ToModel() model.QualityProfile {
	groups := make(map[string]int, len(p.PreferredGroups))
	for k, v := range p.PreferredGroups {
		groups[k] = v
	}
	return model.QualityProfile{
		Name:               p.Name,
		IsDefault:          p.Default,
		AllowedResolutions: slices.Clone(p.AllowedResolutions),
		AllowedSources:     slices.Clone(p.AllowedSources),
		MinSizeMB:          p.MinSizeMB,
		MaxSizeMB:          p.MaxSizeMB,
		MinSeeders:         p.MinSeeders,
		RequiredTerms:      slices.Clone(p.RequiredTerms),
		BannedTerms:        slices.Clone(p.BannedTerms),
		PreferredGroups:    groups,
		LanguagePrefs:      slices.Clone(p.LanguagePrefs),
		HDRPrefs:           slices.Clone(p.HDRPrefs),
		UpgradeUntil:       p.UpgradeUntil,
	}
}

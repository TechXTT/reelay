package config

import "github.com/TechXTT/reelay/internal/model"

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
		AllowedResolutions: copyStrings(p.AllowedResolutions),
		AllowedSources:     copyStrings(p.AllowedSources),
		MinSizeMB:          p.MinSizeMB,
		MaxSizeMB:          p.MaxSizeMB,
		MinSeeders:         p.MinSeeders,
		RequiredTerms:      copyStrings(p.RequiredTerms),
		BannedTerms:        copyStrings(p.BannedTerms),
		PreferredGroups:    groups,
		LanguagePrefs:      copyStrings(p.LanguagePrefs),
		HDRPrefs:           copyStrings(p.HDRPrefs),
		UpgradeUntil:       p.UpgradeUntil,
	}
}

// copyStrings defends against the caller mutating a slice that is still shared
// with the parsed config, which is read once and treated as immutable.
func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

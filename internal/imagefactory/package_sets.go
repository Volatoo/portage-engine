package imagefactory

import (
	"fmt"
	"sort"
)

// PackageSetCatalog is a locked, reusable package-group definition shared by
// multiple image profiles. BuildPlans select set IDs and may add a small list
// of target-only packages without duplicating common package groups.
type PackageSetCatalog struct {
	SchemaVersion int                    `json:"schema_version"`
	CatalogID     string                 `json:"catalog_id"`
	Sets          []PackageSetDefinition `json:"sets"`
}

type PackageSetDefinition struct {
	ID       string   `json:"id"`
	Includes []string `json:"includes,omitempty"`
	Packages []string `json:"packages,omitempty"`
}

func LoadPackageSetCatalog(path string) (*PackageSetCatalog, error) {
	var catalog PackageSetCatalog
	if err := decodeStrictFile(path, &catalog); err != nil {
		return nil, err
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	return &catalog, nil
}

func (c *PackageSetCatalog) Validate() error {
	if c == nil || c.SchemaVersion != 1 || !objectIDPattern.MatchString(c.CatalogID) || len(c.Sets) == 0 || len(c.Sets) > 128 {
		return fmt.Errorf("package-set catalog has invalid metadata")
	}
	definitions := make(map[string]PackageSetDefinition, len(c.Sets))
	for _, set := range c.Sets {
		if !objectIDPattern.MatchString(set.ID) || len(set.Includes) > 32 || len(set.Packages) > 256 || len(set.Includes)+len(set.Packages) == 0 {
			return fmt.Errorf("package set %q has invalid metadata", set.ID)
		}
		if _, exists := definitions[set.ID]; exists {
			return fmt.Errorf("duplicate package set %q", set.ID)
		}
		seenIncludes := make(map[string]struct{}, len(set.Includes))
		for _, included := range set.Includes {
			if !objectIDPattern.MatchString(included) || included == set.ID {
				return fmt.Errorf("package set %q has invalid include %q", set.ID, included)
			}
			if _, exists := seenIncludes[included]; exists {
				return fmt.Errorf("package set %q includes %q more than once", set.ID, included)
			}
			seenIncludes[included] = struct{}{}
		}
		seenPackages := make(map[string]struct{}, len(set.Packages))
		for _, atom := range set.Packages {
			if !validPackageAtom(atom) {
				return fmt.Errorf("package set %q has invalid package atom %q", set.ID, atom)
			}
			if _, exists := seenPackages[atom]; exists {
				return fmt.Errorf("package set %q contains duplicate package %q", set.ID, atom)
			}
			seenPackages[atom] = struct{}{}
		}
		definitions[set.ID] = set
	}
	for _, set := range c.Sets {
		for _, included := range set.Includes {
			if _, exists := definitions[included]; !exists {
				return fmt.Errorf("package set %q includes unknown set %q", set.ID, included)
			}
		}
	}
	state := make(map[string]uint8, len(definitions))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("package set include cycle at %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, included := range definitions[id].Includes {
			if err := visit(included); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(definitions))
	for id := range definitions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (c *PackageSetCatalog) Resolve(requested, extras []string) ([]string, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if len(requested) == 0 || len(requested) > 32 {
		return nil, fmt.Errorf("BuildPlan must select 1..32 package sets")
	}
	definitions := make(map[string]PackageSetDefinition, len(c.Sets))
	for _, set := range c.Sets {
		definitions[set.ID] = set
	}
	seenRequested := make(map[string]struct{}, len(requested))
	seenSets := make(map[string]struct{}, len(definitions))
	seenPackages := make(map[string]struct{}, 256)
	resolved := make([]string, 0, 256)
	var addSet func(string) error
	addSet = func(id string) error {
		definition, exists := definitions[id]
		if !exists {
			return fmt.Errorf("BuildPlan references unknown package set %q", id)
		}
		if _, exists := seenSets[id]; exists {
			return nil
		}
		seenSets[id] = struct{}{}
		for _, included := range definition.Includes {
			if err := addSet(included); err != nil {
				return err
			}
		}
		for _, atom := range definition.Packages {
			if _, exists := seenPackages[atom]; !exists {
				seenPackages[atom] = struct{}{}
				resolved = append(resolved, atom)
			}
		}
		return nil
	}
	for _, id := range requested {
		if _, exists := seenRequested[id]; exists {
			return nil, fmt.Errorf("BuildPlan selects package set %q more than once", id)
		}
		seenRequested[id] = struct{}{}
		if err := addSet(id); err != nil {
			return nil, err
		}
	}
	for _, atom := range extras {
		if !validPackageAtom(atom) {
			return nil, fmt.Errorf("invalid target-only package atom %q", atom)
		}
		if _, exists := seenPackages[atom]; !exists {
			seenPackages[atom] = struct{}{}
			resolved = append(resolved, atom)
		}
	}
	if len(resolved) == 0 || len(resolved) > 512 {
		return nil, fmt.Errorf("effective package set is empty or too large")
	}
	return resolved, nil
}

func validPackageAtom(atom string) bool {
	parts := splitAtom(atom)
	return len(parts) == 2 && repoComponentPattern.MatchString(parts[0]) && repoComponentPattern.MatchString(parts[1])
}

func splitAtom(atom string) []string {
	for i := range atom {
		if atom[i] == '/' {
			return []string{atom[:i], atom[i+1:]}
		}
	}
	return nil
}

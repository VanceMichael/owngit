package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repository names. A repository answers at its ID until it is renamed; the
// ID and the storage folder never change. A rename records one current name,
// never the repository's own ID (renaming back removes that row), and keeps
// earlier names as aliases that redirect until AliasUntil. No name is ever
// another repository's ID, so an old address can never reach, or become the
// authority of, a different repository.
const (
	RepositoryNameCurrent = "current"
	RepositoryNameAlias   = "alias"
)

type RepositoryName struct {
	Name         string
	RepositoryID string
	Kind         string
	CreatedAt    time.Time
	// AliasUntil is when an alias stops redirecting; nil for the current name.
	AliasUntil *time.Time
}

// RepositoryPolicy holds a repository's own policies. No row means every
// default.
type RepositoryPolicy struct {
	RepositoryID string
	// RetainHistory overrides the server default for kept history; nil
	// follows it.
	RetainHistory        *bool
	ProtectDefaultBranch bool
	// ExtraRefPrefixes are ref namespaces beyond branches and tags that
	// pushes may change, such as refs/notes/.
	ExtraRefPrefixes []string
	// Archived makes the repository answer every read and refuse every
	// write, including imports. It survives restarts and backups.
	Archived  bool
	UpdatedAt time.Time
}

// IsDefault reports whether the policy equals having no row.
func (p RepositoryPolicy) IsDefault() bool {
	return p.RetainHistory == nil && !p.ProtectDefaultBranch && len(p.ExtraRefPrefixes) == 0
}

// maximumExtraRefPrefixes bounds a list of extra ref namespaces, and
// maximumExtraRefPrefix one namespace, so every list fits its column.
const (
	maximumExtraRefPrefixes = 32
	maximumExtraRefPrefix   = 100
)

// ValidateExtraRefPrefixes accepts ref namespaces such as refs/notes/: each
// starts with refs/, ends with /, is written with ASCII letters, digits and
// "-", "_", "." and "/" as Git accepts in a ref name, and has at most 100
// characters. None may lie inside or around another of the list or
// branches, tags or OwnGit's own refs, letter case aside, or spell a folder
// they share differently (refs/Notes/a/ beside refs/notes/b/), since some
// file systems store two such spellings in one folder.
func ValidateExtraRefPrefixes(prefixes []string) error {
	if len(prefixes) > maximumExtraRefPrefixes {
		return fmt.Errorf("at most %d extra ref namespaces are allowed", maximumExtraRefPrefixes)
	}
	taken := []string{"refs/heads/", "refs/tags/", "refs/owngit/"}
	for _, prefix := range prefixes {
		name := strings.TrimSuffix(prefix, "/")
		valid := len(prefix) <= maximumExtraRefPrefix && strings.HasPrefix(prefix, "refs/") && strings.HasSuffix(prefix, "/") &&
			validBranchText(name) && strings.Count(name, "/") >= 1
		for _, character := range prefix {
			if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_./", character)) {
				valid = false
			}
		}
		if !valid {
			return fmt.Errorf("extra ref namespace %q is not a ref prefix such as refs/notes/", prefix)
		}
		for _, other := range taken {
			if refPrefixesConflict(prefix, other) {
				return fmt.Errorf("extra ref namespace %q overlaps %s or spells one of its folders differently", prefix, other)
			}
		}
		taken = append(taken, prefix)
	}
	return nil
}

// refPrefixesConflict reports whether two ref prefixes name one folder in
// two spellings, or one lies inside the other, letter case aside. They are
// compared folder by folder from refs/: folders that differ apart from
// letter case are siblings, which never conflict.
func refPrefixesConflict(a, b string) bool {
	left, right := strings.Split(strings.Trim(a, "/"), "/"), strings.Split(strings.Trim(b, "/"), "/")
	for index := range min(len(left), len(right)) {
		if !strings.EqualFold(left[index], right[index]) {
			return false
		}
		if left[index] != right[index] {
			return true
		}
	}
	return true
}

// encodeRefPrefixes returns the column text of a valid list: [] when empty.
func encodeRefPrefixes(prefixes []string) (string, error) {
	if err := ValidateExtraRefPrefixes(prefixes); err != nil {
		return "", err
	}
	if len(prefixes) == 0 {
		return "[]", nil
	}
	encoded, err := json.Marshal(prefixes)
	return string(encoded), err
}

// decodeRefPrefixes reads column text that encodeRefPrefixes wrote.
func decodeRefPrefixes(text string) ([]string, error) {
	var prefixes []string
	if err := json.Unmarshal([]byte(text), &prefixes); err != nil || prefixes == nil {
		return nil, fmt.Errorf("stored extra ref namespaces %q are not a list", text)
	}
	if err := ValidateExtraRefPrefixes(prefixes); err != nil {
		return nil, fmt.Errorf("stored extra ref namespaces: %w", err)
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	return prefixes, nil
}

// ValidateRepositoryRecords checks the names and policies in snapshot
// against its repositories. Name syntax is the repository ID syntax, which
// the recovery manifest checks with the repository package.
func ValidateRepositoryRecords(snapshot RecoveryState) error {
	repositories := make(map[string]bool, len(snapshot.Repositories))
	for _, repository := range snapshot.Repositories {
		repositories[repository.ID] = true
	}
	names := make(map[string]bool, len(snapshot.RepositoryNames))
	current := make(map[string]bool)
	for _, name := range snapshot.RepositoryNames {
		if !repositories[name.RepositoryID] {
			return fmt.Errorf("repository name %q refers to an unknown repository", name.Name)
		}
		if name.Name == "" || len(name.Name) > 100 || name.CreatedAt.IsZero() {
			return errors.New("invalid repository name record")
		}
		if names[name.Name] {
			return fmt.Errorf("repository name %q is recorded twice", name.Name)
		}
		names[name.Name] = true
		if repositories[name.Name] && name.Name != name.RepositoryID {
			return fmt.Errorf("repository name %q is another repository's ID", name.Name)
		}
		switch name.Kind {
		case RepositoryNameCurrent:
			if name.AliasUntil != nil || name.Name == name.RepositoryID || current[name.RepositoryID] {
				return fmt.Errorf("invalid current name %q", name.Name)
			}
			current[name.RepositoryID] = true
		case RepositoryNameAlias:
			if name.AliasUntil == nil || !name.AliasUntil.After(name.CreatedAt) {
				return fmt.Errorf("alias %q has no valid end", name.Name)
			}
		default:
			return fmt.Errorf("invalid repository name kind %q", name.Kind)
		}
	}
	policies := make(map[string]bool, len(snapshot.RepositoryPolicies))
	for _, policy := range snapshot.RepositoryPolicies {
		if !repositories[policy.RepositoryID] || policies[policy.RepositoryID] || policy.UpdatedAt.IsZero() {
			return fmt.Errorf("invalid policy record for repository %q", policy.RepositoryID)
		}
		policies[policy.RepositoryID] = true
		if err := ValidateExtraRefPrefixes(policy.ExtraRefPrefixes); err != nil {
			return fmt.Errorf("repository %q policy: %w", policy.RepositoryID, err)
		}
	}
	return nil
}

func readRepositoryRecords(ctx context.Context, tx *sql.Tx, snapshot *RecoveryState) error {
	rows, err := tx.QueryContext(ctx, `SELECT name,repository_id,kind,created_at,alias_until FROM repository_names ORDER BY repository_id,name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name RepositoryName
		var created int64
		var until sql.NullInt64
		if err := rows.Scan(&name.Name, &name.RepositoryID, &name.Kind, &created, &until); err != nil {
			rows.Close()
			return err
		}
		name.CreatedAt = unixTime(created)
		name.AliasUntil = nullableTimePointer(until)
		snapshot.RepositoryNames = append(snapshot.RepositoryNames, name)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT repository_id,retain_history,protect_default_branch,extra_ref_prefixes,archived,updated_at FROM repository_policies ORDER BY repository_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var policy RepositoryPolicy
		var retain sql.NullBool
		var prefixes string
		var archived int64
		var updated int64
		if err := rows.Scan(&policy.RepositoryID, &retain, &policy.ProtectDefaultBranch, &prefixes, &archived, &updated); err != nil {
			rows.Close()
			return err
		}
		if retain.Valid {
			policy.RetainHistory = &retain.Bool
		}
		if policy.ExtraRefPrefixes, err = decodeRefPrefixes(prefixes); err != nil {
			rows.Close()
			return fmt.Errorf("repository %q policy: %w", policy.RepositoryID, err)
		}
		policy.Archived = archived == 1
		policy.UpdatedAt = unixTime(updated)
		snapshot.RepositoryPolicies = append(snapshot.RepositoryPolicies, policy)
	}
	return closeRows(rows)
}

func restoreRepositoryRecords(ctx context.Context, tx *sql.Tx, snapshot RecoveryState) error {
	for _, name := range snapshot.RepositoryNames {
		if _, err := tx.ExecContext(ctx, `INSERT INTO repository_names(name,repository_id,kind,created_at,alias_until) VALUES(?,?,?,?,?)`,
			name.Name, name.RepositoryID, name.Kind, name.CreatedAt.Unix(), nullableUnix(name.AliasUntil)); err != nil {
			return fmt.Errorf("restore repository name %q: %w", name.Name, err)
		}
	}
	for _, policy := range snapshot.RepositoryPolicies {
		prefixes, err := encodeRefPrefixes(policy.ExtraRefPrefixes)
		if err != nil {
			return fmt.Errorf("restore repository %q policy: %w", policy.RepositoryID, err)
		}
		var retain any
		if policy.RetainHistory != nil {
			retain = boolInt(*policy.RetainHistory)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO repository_policies(repository_id,retain_history,protect_default_branch,extra_ref_prefixes,archived,updated_at) VALUES(?,?,?,?,?,?)`,
			policy.RepositoryID, retain, boolInt(policy.ProtectDefaultBranch), prefixes, boolInt(policy.Archived), policy.UpdatedAt.Unix()); err != nil {
			return fmt.Errorf("restore repository %q policy: %w", policy.RepositoryID, err)
		}
	}
	return nil
}

package review

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
)

// Validate reports every way p is malformed given the files changed in the
// pull request: a bad DocPath, an Anchor on a file the PR does not change or
// deletes or whose line is outside its hunks, missing Reason or
// Content, a multi-line Reason, or an IndexEntry that doesn't match whether
// Section is empty.
func (p Proposal) Validate(changed []ChangedFile) error {
	var errs []error

	if err := p.ValidateTarget(); err != nil {
		errs = append(errs, err)
	}
	if err := validateAnchor(p.Anchor, changed); err != nil {
		errs = append(errs, err)
	}
	if p.Reason == "" {
		errs = append(errs, errors.New("reason: must not be empty"))
	} else if strings.ContainsAny(p.Reason, "\n\r") {
		errs = append(errs, errors.New("reason: must be one line"))
	}
	if p.Content == "" {
		errs = append(errs, errors.New("content: must not be empty"))
	}
	if (p.IndexEntry != "") != (p.Section == "") {
		errs = append(errs, errors.New("index_entry: must be set iff section is empty"))
	}

	return errors.Join(errs...)
}

// ValidateDocPath checks that a proposal target is a safe, canonical docs path.
// Actions checks every target before allowing proposal-local failures to be dropped.
func ValidateDocPath(docPath string) error {
	if docPath == "" {
		return errors.New("doc_path: must not be empty")
	}
	if path.IsAbs(docPath) || strings.HasPrefix(docPath, "/") {
		return fmt.Errorf("doc_path %q: must be relative", docPath)
	}
	if path.Clean(docPath) != docPath {
		return fmt.Errorf("doc_path %q: must be a clean path", docPath)
	}
	if strings.Contains(docPath, "..") {
		return fmt.Errorf("doc_path %q: must not contain \"..\"", docPath)
	}
	if docPath == "docs" || !strings.HasPrefix(docPath, "docs/") {
		return fmt.Errorf("doc_path %q: must be under \"docs/\"", docPath)
	}
	if ext := path.Ext(docPath); ext != ".md" && ext != ".mdx" {
		return fmt.Errorf("doc_path %q: must end in .md or .mdx", docPath)
	}
	if strings.ContainsFunc(docPath, unicode.IsControl) {
		return fmt.Errorf("doc_path %q: must not contain control characters", docPath)
	}
	if strings.Contains(docPath, "`") {
		return fmt.Errorf("doc_path %q: must not contain backticks", docPath)
	}
	return nil
}

// validateAnchor requires a changed, non-removed file and a line inside one of
// its hunks, and lists the commentable ranges so the caller can correct it.
func validateAnchor(anchor Anchor, changed []ChangedFile) error {
	i := slices.IndexFunc(changed, func(f ChangedFile) bool { return f.Path == anchor.File })
	if i < 0 {
		return fmt.Errorf("anchor.file %q: not a changed file", anchor.File)
	}
	file := changed[i]
	if file.Removed || len(file.Hunks) == 0 {
		return fmt.Errorf("anchor.file %q: has no head-side lines in the diff", anchor.File)
	}
	if slices.ContainsFunc(file.Hunks, func(h LineRange) bool { return anchor.Line >= h.Start && anchor.Line <= h.End }) {
		return nil
	}
	ranges := make([]string, len(file.Hunks))
	for j, h := range file.Hunks {
		ranges[j] = fmt.Sprintf("%d-%d", h.Start, h.End)
	}
	return fmt.Errorf("anchor.line %d: not a numbered line in the diff of %q; commentable lines: %s", anchor.Line, anchor.File, strings.Join(ranges, ", "))
}

// ValidateTarget checks the parts of p that decide what Apply writes: the doc
// path, the section heading, and the index entry.
func (p Proposal) ValidateTarget() error {
	var errs []error

	if err := ValidateDocPath(p.DocPath); err != nil {
		errs = append(errs, err)
	}
	if err := validateSingleLine("section", p.Section); err != nil {
		errs = append(errs, err)
	} else if p.Section != "" && strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(p.Section), "#")) == "" {
		errs = append(errs, errors.New("section: must name a heading"))
	}
	if err := validateSingleLine("index_entry", p.IndexEntry); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func validateSingleLine(field, value string) error {
	if strings.ContainsFunc(value, unicode.IsControl) {
		return fmt.Errorf("%s: must be one line without control characters", field)
	}
	return nil
}

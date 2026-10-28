package proxy

import (
	"fmt"
	"regexp"
	"strings"
)

// A WAF exclusion tells the rule set to stop looking at something it was wrong
// about: a cookie a framework sets, a path a scanner is not on. It is a SECURITY
// DOWNGRADE, so almost everything in this file is about what is refused.
//
// Three shapes, one table:
//   - a target: one named request field, for one rule ("ignore REQUEST_COOKIES:
//     ph_x for rule 942290"). Keyed matches have a name that discriminates.
//   - a path: for one rule, on requests to one path. Keyless matches
//     (REQUEST_FILENAME, REQUEST_URI) have no name that discriminates, and the
//     value is deliberately never stored, so the operator says which path.
//   - the rule: off entirely, for an application or the install.

const (
	// MaxWAFExclusions bounds the list. The rendered directives are rebuilt on
	// every reconcile pass and parsed by Caddy on every change; CRS at paranoia
	// level 1 has about 150 signature rules, so an install that needs more than
	// this has a tuning problem that more exclusions will not solve.
	MaxWAFExclusions = 100

	// The rules an exclusion may name. 910000-944999 are the CRS request-side
	// DETECTION rules (scanners, protocol, LFI/RFI, RCE, PHP, XSS, SQLi, ...).
	// Everything outside it is something the engine or the verdict depends on:
	// 901/905 initialise the transaction, and 949110, 959100 and 980130 evaluate
	// the anomaly score and report it. Excluding an evaluation rule does not
	// quiet a false positive, it removes the verdict, so those are refused and
	// not merely warned about.
	WAFExclusionMinRuleID = 910000
	WAFExclusionMaxRuleID = 944999

	maxWAFTargetName  = 80
	maxWAFPathPattern = 200
)

// WAFExclusionSpec is what an operator submits.
type WAFExclusionSpec struct {
	RuleID int
	// Collection and TargetName are both set (a keyed target) or both empty.
	Collection string
	TargetName string
	// PathPattern is a rooted path; a trailing * makes it a prefix.
	PathPattern string
}

// WAFExclusion is a stored exclusion, with the hosts it applies to resolved. The
// hosts are not stored with it: they are the application's CURRENT domains, so a
// deleted or renamed domain simply drops out, and nothing can name a host that no
// longer exists.
type WAFExclusion struct {
	ID int64
	WAFExclusionSpec
	// Global is an install-wide exclusion. It is the only way to apply to every
	// host, and it is explicit on purpose: an exclusion scoped to an application
	// that has no domains must be inert, and must never read as install-wide.
	Global bool
	Hosts  []string
}

// Shape names what kind of exclusion this is.
func (s WAFExclusionSpec) Shape() string {
	switch {
	case s.Collection != "":
		return "target"
	case s.PathPattern != "":
		return "path"
	default:
		return "rule"
	}
}

// The collections a keyed target may name: the request fields a rule can match in
// and a client can cause to carry something. A closed list, because the
// collection is written into a directive.
var wafExclusionCollections = map[string]struct{}{
	"ARGS": {}, "ARGS_GET": {}, "ARGS_POST": {}, "ARGS_NAMES": {},
	"REQUEST_COOKIES": {}, "REQUEST_COOKIES_NAMES": {},
	"REQUEST_HEADERS": {}, "REQUEST_HEADERS_NAMES": {},
}

// WAFExclusionCollections lists them, for the interface to offer.
func WAFExclusionCollections() []string {
	return []string{
		"ARGS", "ARGS_GET", "ARGS_POST", "ARGS_NAMES",
		"REQUEST_COOKIES", "REQUEST_COOKIES_NAMES",
		"REQUEST_HEADERS", "REQUEST_HEADERS_NAMES",
	}
}

var (
	// A name is written unquoted into `ctl:ruleRemoveTargetById=ID;COLL:name`, inside
	// a quoted SecRule string. `,` ends the value and starts a new action
	// (`,ctl:ruleEngine=Off`), `;` and `:` are the ctl's own separators, `"` and `\`
	// end the string, and `%{` expands a macro that can read any variable. None of
	// them is in this set, and a name needing one is refused rather than escaped:
	// it cannot be excluded by name, and the rule or the path shape is the answer.
	wafTargetNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)

	// A path is written inside @streq "..." or @beginsWith "...". REQUEST_FILENAME is
	// already URL-decoded, so there is no reason to admit %, and `%{` is a macro.
	// Rooted, no query, no regular expression: a * is only ever allowed as the last
	// character, where it means "begins with".
	wafPathPatternRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{0,199}\*?$`)
)

// ValidateWAFExclusion says why a submitted exclusion is refused, or nil.
func ValidateWAFExclusion(s WAFExclusionSpec) error {
	if err := validateWAFRuleID(s.RuleID); err != nil {
		return err
	}
	if (s.Collection == "") != (s.TargetName == "") {
		return fmt.Errorf("a target needs both a collection and a name")
	}
	if s.Collection != "" {
		if s.PathPattern != "" {
			return fmt.Errorf("an exclusion is for a target or for a path, not both")
		}
		if _, ok := wafExclusionCollections[s.Collection]; !ok {
			return fmt.Errorf("%q is not a collection an exclusion can name", clipForError(s.Collection))
		}
		if !wafTargetNameRe.MatchString(s.TargetName) {
			return fmt.Errorf("the name %q cannot be excluded by name: only letters, digits, '.', '_' and '-' (up to %d) are allowed. Exclude the rule for a path instead",
				clipForError(s.TargetName), maxWAFTargetName)
		}
	}
	if s.PathPattern != "" {
		if !wafPathPatternRe.MatchString(s.PathPattern) {
			return fmt.Errorf("the path %q is not allowed: it must start with '/', use only letters, digits and . _ ~ / -, and may end with one '*' to mean 'begins with'",
				clipForError(s.PathPattern))
		}
		if s.PathPattern == "/*" {
			// Every path: that is the rule off, and should say so.
			return fmt.Errorf("'/*' matches every path; exclude the whole rule instead")
		}
	}
	return nil
}

func validateWAFRuleID(id int) error {
	switch {
	case id == 949110 || id == 959100 || id == 980130,
		id >= 949000 && id <= 949999, id >= 959000 && id <= 959999, id >= 980000 && id <= 980999:
		return fmt.Errorf("rule %d evaluates the anomaly score; it is the verdict, not a signature, and excluding it would remove the verdict instead of quieting a false positive. Exclude the rule that scored instead", id)
	case id < WAFExclusionMinRuleID || id > WAFExclusionMaxRuleID:
		return fmt.Errorf("rule %d is not a request-side detection rule (%d-%d)", id, WAFExclusionMinRuleID, WAFExclusionMaxRuleID)
	}
	return nil
}

func clipForError(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
	if len(s) > 48 {
		return s[:48] + "…"
	}
	return s
}

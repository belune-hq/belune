package service

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Where an application comes from is described by four fields that have to
// agree with each other. Nothing enforced that agreement: the database CHECKs
// each column's value in isolation, and the deploy worker switches on `type`
// alone, so a field belonging to the other kind of source was accepted, stored,
// and then silently ignored forever.
//
// The two failures this closes, both observed rather than theorised:
//
//   - Clearing source_image on an image application stored NULL, because the
//     update maps "" to NULL. The save succeeded and the next deploy failed on
//     an empty pull, far away from the edit that caused it.
//
//   - Setting source_repo on an image application was accepted and ignored. A
//     push webhook then matched the app by repository URL and "successfully"
//     deployed it — by re-pulling the same image, never touching the repo. A
//     green deployment that built nothing is worse than an error.
//
// Rejecting at the API boundary keeps the failure next to the edit, and keeps
// the invariant in one place instead of spread across every consumer that has
// to guess which fields are meaningful.

var (
	validTypes      = []string{"git", "image"}
	validBuildTypes = []string{"dockerfile", "buildpacks", "railpack", "image"}
)

func oneOf(value string, allowed []string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

// SourceFields is the effective state being validated. On create every field
// comes from the request; on update, type and build_type come from the stored
// row, because neither is updatable (until item 6 makes the switch an explicit
// action).
type SourceFields struct {
	Type              string
	BuildType         string
	BuildTypeOverride string
	DockerfilePath    string
	SourceRepo        string
	SourceImage       string
}

// ValidateSource reports the first incoherence, phrased so the message says
// what to do rather than what is wrong with the payload.
func ValidateSource(f SourceFields) error {
	if !oneOf(f.Type, validTypes) {
		return fmt.Errorf("type must be one of: %s", strings.Join(validTypes, ", "))
	}
	if !oneOf(f.BuildType, validBuildTypes) {
		return fmt.Errorf("build_type must be one of: %s", strings.Join(validBuildTypes, ", "))
	}
	if f.BuildTypeOverride != "" && !oneOf(f.BuildTypeOverride, validBuildTypes) {
		return fmt.Errorf("build_type_override must be one of: %s", strings.Join(validBuildTypes, ", "))
	}

	switch f.Type {
	case "git":
		if f.SourceRepo == "" {
			return errors.New("a git application needs a source_repo")
		}
		if f.SourceImage != "" {
			return errors.New("a git application builds its own image; remove source_image")
		}
		// build_type selects the builder, so "image" would leave nothing to
		// build the checkout with.
		if f.BuildType == "image" || f.BuildTypeOverride == "image" {
			return errors.New("build_type 'image' is only for image applications; choose dockerfile, buildpacks, or railpack")
		}
	case "image":
		if f.SourceImage == "" {
			return errors.New("an image application needs a source_image")
		}
		if f.SourceRepo != "" {
			return errors.New("an image application is not built from source; remove source_repo")
		}
		// Anything else would be accepted and then ignored, because the deploy
		// worker pulls without consulting a builder for image applications.
		if f.BuildType != "image" {
			return errors.New("an image application must use build_type 'image'")
		}
		if f.BuildTypeOverride != "" {
			return errors.New("build_type_override does not apply to image applications; remove it")
		}
		if f.DockerfilePath != "" {
			return errors.New("dockerfile_path does not apply to image applications; remove it")
		}
	}
	return nil
}

// ValidBranchName reports whether a branch name is safe to hand to
// `git clone --branch`. Not a full git-refname validator — just enough to keep
// obviously broken input out of an argv slot and out of the database.
//
// A leading "-" is rejected specifically: git would read it as a flag rather
// than a ref name if argument order ever changed.
func ValidBranchName(branch string) bool {
	if branch == "" {
		return true // empty is meaningful: the repository's default ref
	}
	if len(branch) > 255 || strings.HasPrefix(branch, "-") {
		return false
	}
	for _, r := range branch {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	// Refs cannot contain these, per git-check-ref-format.
	return !strings.ContainsAny(branch, "~^:?*[\\") && !strings.Contains(branch, "..")
}

// ValidRootDirectory reports whether a root directory value is safe to join
// onto a clone's temp directory and hand to a builder. Not a full path
// validator — just enough to keep traversal and control characters out.
//
// Empty is meaningful: build from the repository root, today's only
// behavior. A leading "/" is rejected because the value is relative to the
// clone root, not absolute; ".." (and empty) segments are rejected outright
// rather than relying solely on the worker's post-join containment check, so
// a bad value is caught at save time instead of surfacing as a deploy
// failure.
func ValidRootDirectory(dir string) bool {
	if dir == "" {
		return true
	}
	if len(dir) > 500 || strings.HasPrefix(dir, "/") {
		return false
	}
	for _, r := range dir {
		if r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	for segment := range strings.SplitSeq(dir, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

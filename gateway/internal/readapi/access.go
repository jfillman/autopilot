package readapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/glob"
)

// Directory is the groups a person is in (Backstage group refs). An error means the answer is not known (the catalog is down or refused
// us); a person who is in no group, or is not in the catalog, is an empty set and no error.
type Directory interface {
	Groups(ctx context.Context, person string) (map[string]bool, error)
}

// Ownership is the apps a person owns, directly or through a group (lower-cased component names), with the same error rule.
type Ownership interface {
	Apps(ctx context.Context, person string) (map[string]bool, error)
}

// Roles of a viewer (read_api.Viewer.role; a delegate acting for a person is role "delegate" for the route's check).
const (
	RoleAuditor  = "auditor"
	RolePerson   = "person"
	RoleNotifier = "notifier"
	RoleDelegate = "delegate"
)

// Viewer is who is asking and what they may see.
type Viewer struct {
	Role   string
	Person string
	Via    string          // the delegate, when a delegate asked
	Agents map[string]bool // agents of the person's teams
	Apps   map[string]bool // apps the person owns
	Groups map[string]bool
	// OwnersUnknown: the catalog could not say which groups or apps the person has, so the viewer is held to what is theirs alone (their
	// own sessions, their own inbox items). Never set for an auditor, whose reads do not ask the catalog.
	OwnersUnknown bool
}

// All is the auditor's view: everything.
func (v *Viewer) All() bool { return v.Role == RoleAuditor }

// RouteRole is the role the route's table is checked against.
func (v *Viewer) RouteRole() string {
	if v.Via != "" {
		return RoleDelegate
	}
	return v.Role
}

// Error is a refusal with the status and the message a caller is shown.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// Access decides who a caller is, in one place (read_api.Access.viewer).
//
//   - No token: 401. A token the authenticator refuses: 401 "token not accepted"; an authenticator that fails for another reason (the
//     TokenReview API is down) is not a refusal: it is a failed read (503), so an outage is never taken for a bad token.
//   - X-Hangar-On-Behalf-Of names a person and only a delegate may send it (a workload that is not a run, matched by its place: the
//     deployment's AUTOPILOT_DELEGATES); a person, a run and every other workload get 403. The delegate is trusted for the person it names
//     and for nothing else: the person is then an ordinary person (the auditors list is not consulted for them) whose name must be a
//     principal form (person) and whose groups the catalog says.
//   - Otherwise the caller is an auditor (GATEWAY_API_AUDITORS globs), else a notifier (GATEWAY_API_NOTIFIERS), else a person if its
//     token is a person's, else refused (403).
type Access struct {
	Auth      dispatch.Authenticator
	Delegates []string
	Notifiers []string
	Auditors  []string
	Directory Directory
	Ownership Ownership
	Defs      *definitions.Set
}

// personRe is the form of a person a delegate may assert: user:<name>, the name a login, an email or a dotted name.
var personRe = regexp.MustCompile(`^user:[A-Za-z0-9][A-Za-z0-9._@+:/-]{0,127}$`)

// ValidPerson is whether s is a principal form a delegate may assert.
func ValidPerson(s string) bool { return personRe.MatchString(s) }

const errBehalf = "X-Hangar-On-Behalf-Of must name a person (user:<name>)"

func anyGlob(id string, globs []string) bool {
	for _, g := range globs {
		if glob.Match(id, g) {
			return true
		}
	}
	return false
}

// Viewer authenticates token and decides the viewer. onBehalfOf is nil when the header is absent, else its values (more than one is
// refused: a request names one person).
func (a *Access) Viewer(ctx context.Context, token string, onBehalfOf []string) (*Viewer, error) {
	if token == "" {
		return nil, &Error{401, "a bearer token is required"}
	}
	p, err := a.Auth.Authenticate(ctx, token)
	if err != nil {
		var ae *core.AuthError
		if errors.As(err, &ae) {
			return nil, &Error{401, "token not accepted"}
		}
		return nil, err
	}
	if len(onBehalfOf) > 0 {
		if !dispatch.IsDelegate(p, a.Delegates) {
			return nil, &Error{403, p.ID + " may not act for a person"}
		}
		if len(onBehalfOf) != 1 || !ValidPerson(onBehalfOf[0]) {
			return nil, &Error{400, errBehalf}
		}
		return a.person(ctx, onBehalfOf[0], p.ID), nil
	}
	if anyGlob(p.ID, a.Auditors) {
		return &Viewer{Role: RoleAuditor, Person: p.ID}, nil
	}
	if anyGlob(p.ID, a.Notifiers) {
		return &Viewer{Role: RoleNotifier}, nil
	}
	if p.Kind == "user" {
		return a.person(ctx, p.ID, ""), nil
	}
	return nil, &Error{403, p.ID + " is not a person, delegate, notifier or auditor"}
}

// person is a person's viewer: their groups (from the catalog), the agents those groups own and the apps they own. A catalog that
// cannot answer leaves them with what is theirs alone, and says so (OwnersUnknown).
func (a *Access) person(ctx context.Context, person, via string) *Viewer {
	v := &Viewer{Role: RolePerson, Person: person, Via: via, Agents: map[string]bool{}, Apps: map[string]bool{}, Groups: map[string]bool{}}
	if a.Directory != nil {
		groups, err := a.Directory.Groups(ctx, person)
		if err != nil {
			v.OwnersUnknown = true
			slog.Warn("a person's groups could not be read from the catalog: they see their own sessions only", "err", err.Error())
		}
		for g := range groups {
			v.Groups[g] = true
		}
	}
	if a.Defs != nil && len(v.Groups) > 0 {
		for name, d := range a.Defs.Snapshot() {
			if d.Owner != nil && v.Groups[*d.Owner] {
				v.Agents[name] = true
			}
		}
	}
	if a.Ownership != nil {
		apps, err := a.Ownership.Apps(ctx, person)
		if err != nil {
			v.OwnersUnknown = true
			slog.Warn("a person's apps could not be read from the catalog: they see their own sessions only", "err", err.Error())
		}
		for app := range apps {
			v.Apps[strings.ToLower(app)] = true
		}
	}
	return v
}

// status is the HTTP status of an error from Viewer.
func status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return http.StatusServiceUnavailable
}

package airframe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
)

// xr.request (xr_request.py): one Airframe XR request, built here byte by byte and written as one new file of the dev
// cluster's tenants repository, tenants/<app>/xr-requests/<name>.yaml, by a pull request a person merges. Argo CD's
// <app>-xr-requests Application then applies the file in app-<app>-cicd and a composition does the rest.
//
// The file is the dangerous part of the call: Kubernetes-shaped YAML that a controller acts on once it is merged, and (for the
// Agent kind) that the definition loader reads as an agent's authority. So the agent supplies a kind, an optional name and the
// XR's spec fields, and nothing else: no path, namespace, apiVersion, annotation or label. What Python checks is kept (the kind
// is one an app requests for itself, the env is a lower environment, the spec is a mapping without metadata under 32 KiB, the
// name is a DNS label). What Python let through and Go does not (each a recorded difference):
//
//   - the KINDS are a closed list of six that this file has a field table for, whatever roles the contract gives a kind: a kind
//     the contract adds later is not requestable until its fields are listed here;
//   - the FIELDS of a spec are the kind's own, each with the type, enum, pattern and range its XRD gives it: any other key is
//     refused, so no compositionRef, compositionSelector, compositionRevisionRef, compositionUpdatePolicy, providerConfigRef,
//     writeConnectionSecretToRef, publishConnectionDetailsTo, managementPolicies, deletionPolicy, resourceRefs, claimRef,
//     crossplane or environmentConfigRefs can be set (none is in any table), nor an ApplicationEnvironment's `chart` (an
//     arbitrary Helm chart source);
//   - every name of something the request creates in a cloud (a server, a queue, a bucket, a storage account, a resource group,
//     a service bus namespace) and every reference to another XR (an environmentRef, a consuming Lambda target) is bound to the
//     app: it is the app's name followed by `-` and more, so a request cannot name, adopt or take over another app's resource;
//   - an RolloutWatch's and an SLO's environment reference, app, cluster and env are the request's own (they are filled in,
//     and a value that says otherwise is refused), as an ApplicationEnvironment's are in Python;
//   - a database's network allowlist names private ranges only, no wider than a /16;
//   - the XR's name starts with `<app>-` (Python: any DNS label);
//   - the finished text is read back with a strict YAML reader and must be exactly one document, with no anchor, alias, tag or
//     duplicate key, and the very value that was built.

// XRAPIVersion is the only apiVersion an XR request has.
const XRAPIVersion = "catalog.hangar.io/v1alpha1"

// MaxXRSpecBytes is Python's bound on a request's spec (the length of its JSON).
const MaxXRSpecBytes = 32 << 10

// XRHeader is the comment that starts every request file.
const XRHeader = "# Requested by a Hangar agent through xr.request; merged by a person.\n"

var (
	xrLabel = regexp.MustCompile(`\A[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\z`)
	// requestableRoles are the contract roles of the kinds an app requests for itself.
	requestableRoles = map[string]bool{"dependency": true, "observability": true, "environment": true}
	lowerEnvs        = map[string]bool{"dev": true, "development": true, "sandbox": true, "scratch": true}
)

// CicdLike says an environment name would make `<type>-<app>-<env>` read as a cicd namespace (`cicd`, or one that ends `-cicd`): no
// environment may be requested, planned or read under such a name.
func CicdLike(env string) bool { return env == "cicd" || strings.HasSuffix(env, "-cicd") }

// IsLowerEnv is xr_request.is_lower.
func IsLowerEnv(env string) bool {
	return lowerEnvs[env] || strings.HasPrefix(env, "pr-") || strings.HasPrefix(env, "agent-")
}

func xrRefuse(rule, msg, hint string) error {
	return &core.Denied{Rule: rule, Message: msg, Hint: hint}
}

// xrEcho is `{v!r}` of an argument, cut: a refusal never repeats more than 100 characters of what an agent wrote.
func xrEcho(v any) string {
	s := pyrepr.Value(definitionsValue(v))
	if r := []rune(s); len(r) > 100 {
		return string(r[:100]) + "..."
	}
	return s
}

// definitionsValue makes a plain argument printable by pyrepr (an ordered value stays as it is).
func definitionsValue(v any) any { return definitions.FromPlain(v) }

// -- the field tables -------------------------------------------------------------------------------------------------------

type ftype int

const (
	tString ftype = iota
	tInt
	tNumber
	tBool
	tObject
	tArray
	tCIDR
	tSelector
)

// rule is what one field may be. A zero rule of a type allows anything of that type up to the size limits (which every string,
// array and object has).
type rule struct {
	t        ftype
	enumS    []string
	enumI    []int64
	re       *regexp.Regexp
	maxLen   int // strings: characters
	min, max float64
	ranged   bool
	exclMin  bool // the minimum itself is refused (the XRD's exclusiveMinimum)
	maxItems int
	item     *rule
	fields   map[string]*rule
	required []string
	// bind: the value is the app's name followed by `-` and more (appDash), or the app's name with its dashes taken out followed
	// by more (appFlat, for names that may hold no dash).
	bind string
	// global: the name is one the cloud holds for everyone (a bucket, a storage account, a server, a queue, a resource group): it
	// must also belong to this app by the longest-owner rule (BuiltXR.CheckOwners), decided from the apps the tenants repository lists.
	global bool
	// policy names the platform setting that bounds the value (XRPolicy): region, vpc, slack, dbsize, storage, protect, emptyOnDelete.
	policy string
	// selector: a mapping of label to value that this gateway writes as a Prometheus matcher list; the agent supplies no selector
	// syntax. ownApp says one of the values must name the app's own.
	selector, ownApp bool
}

func s(re string, bind string) *rule {
	return &rule{t: tString, re: regexp.MustCompile(`\A(?:` + re + `)\z`), bind: bind, maxLen: 256}
}
func se(vals ...string) *rule        { return &rule{t: tString, enumS: vals} }
func glob(r *rule) *rule             { r.global = true; return r }
func pol(r *rule, name string) *rule { r.policy = name; return r }
func ie(vals ...int64) *rule         { return &rule{t: tInt, enumI: vals} }
func ir(min, max int64) *rule {
	return &rule{t: tInt, min: float64(min), max: float64(max), ranged: true}
}
func b() *rule { return &rule{t: tBool} }
func obj(fields map[string]*rule, required ...string) *rule {
	return &rule{t: tObject, fields: fields, required: required}
}
func arr(max int, item *rule) *rule { return &rule{t: tArray, maxItems: max, item: item} }

const (
	appDash = "appDash"
	appFlat = "appFlat"
)

var (
	region   = &rule{t: tString, re: regexp.MustCompile(`\A(?:[a-z]{2}(-[a-z]+)+-[0-9])\z`), maxLen: 256, policy: "region"}
	location = s(`[a-z0-9]+`, "")
	rgroup   = glob(s(`[a-zA-Z0-9._()-]{1,90}`, appDash))
	cloud    = se("aws", "azure")
	// target is a reference to a Lambda target XR of the same app (the XR is in the app's own namespace).
	target = s(`[a-z0-9]([-a-z0-9]*[a-z0-9])?`, appDash)
)

// kindFields is the whole of what xr.request may set, kind by kind: the XRDs of the Airframe commit the contract was made
// from (xrds/*.yaml), each field with its XRD's own enum, pattern and range. A field that is not here is not settable.
var kindFields = map[string]*rule{
	"ManagedPostgreSQL": obj(map[string]*rule{
		"cloud":               cloud,
		"serverName":          glob(s(`[a-z][a-z0-9-]{1,61}[a-z0-9]`, appDash)),
		"version":             se("16", "17"),
		"size":                pol(se("small", "medium", "large"), "dbsize"),
		"storageGiB":          pol(ie(32, 64, 128, 256), "storage"),
		"databaseName":        s(`[a-z][a-z0-9_]{0,62}`, ""),
		"backupRetentionDays": ir(7, 35),
		"allowedCidrs":        arr(20, &rule{t: tCIDR}),
		"protect":             pol(b(), "protect"),
		"region":              region,
		"vpcCidr":             pol(s(`10\.([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])\.0\.0/16`, ""), "vpc"),
		"location":            location,
		"resourceGroup":       rgroup,
	}),
	"MessageQueue": obj(map[string]*rule{
		"cloud":            cloud,
		"queueName":        glob(s(`[a-z0-9][a-z0-9-]{0,59}`, appDash)),
		"fifo":             b(),
		"lockSeconds":      ir(1, 43200),
		"retentionSeconds": ir(60, 1209600),
		"deadLetter":       obj(map[string]*rule{"enabled": b(), "maxReceiveCount": ir(1, 1000)}),
		"region":           region,
		"location":         location,
		"resourceGroup":    rgroup,
		"namespaceName":    glob(s(`[a-zA-Z][a-zA-Z0-9-]{4,48}[a-zA-Z0-9]`, appDash)),
		"consumers": pol(arr(10, obj(map[string]*rule{
			"awsLambdaTarget": &rule{t: tString, re: target.re, bind: appDash, maxLen: 40},
			"access":          se("send", "receive", "sendReceive"),
			"trigger":         b(),
			"batchSize":       ir(1, 10),
		}, "awsLambdaTarget")), "consumers"),
	}),
	"ObjectStorage": obj(map[string]*rule{
		"cloud":              cloud,
		"bucketName":         glob(s(`[a-z0-9][a-z0-9-]{1,61}[a-z0-9]`, appDash)),
		"versioning":         b(),
		"emptyOnDelete":      pol(b(), "emptyOnDelete"),
		"region":             region,
		"location":           location,
		"resourceGroup":      rgroup,
		"storageAccountName": glob(s(`[a-z0-9]{3,24}`, appFlat)),
		"consumers": pol(arr(10, obj(map[string]*rule{
			"awsLambdaTarget": &rule{t: tString, re: target.re, bind: appDash, maxLen: 40},
			"access":          se("read", "readWrite"),
		}, "awsLambdaTarget")), "consumers"),
	}),
	"RolloutWatch": obj(map[string]*rule{
		"environmentRef": obj(map[string]*rule{"name": s(`[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`, "")}, "name"),
		"appName":        s(`[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`, ""),
		"cluster":        s(`[A-Za-z0-9][A-Za-z0-9._-]{0,62}`, ""),
		"env":            s(`[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`, ""),
		"notifications": obj(map[string]*rule{"slack": obj(map[string]*rule{
			"enabled": b(),
			"channel": pol(s(`[#A-Za-z0-9._-]{1,80}`, ""), "slack"),
		}, "enabled")}),
	}),
	"SLO": obj(map[string]*rule{
		"environmentRef": obj(map[string]*rule{"name": s(`[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`, "")}, "name"),
		"service":        &rule{t: tString, re: regexp.MustCompile(`\A(?:[a-zA-Z0-9_.-]+)\z`), bind: appDash, maxLen: 100},
		"objective":      {t: tNumber, min: 0, max: 100, ranged: true, exclMin: true},
		"indicator": obj(map[string]*rule{
			"type":             se("availability", "latency"),
			"metric":           s(`[a-zA-Z_:][a-zA-Z0-9_:]{0,199}`, ""),
			"totalFilter":      &rule{t: tSelector, selector: true},
			"errorFilter":      &rule{t: tSelector, selector: true},
			"latencyThreshold": s(`[0-9]{1,6}(\.[0-9]{1,6})?`, ""),
		}, "type", "metric", "totalFilter"),
	}),
	"ApplicationEnvironment": obj(map[string]*rule{
		"appName":            s(`[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`, ""),
		"cluster":            s(`[A-Za-z0-9][A-Za-z0-9._-]{0,62}`, ""),
		"env":                &rule{t: tString, re: regexp.MustCompile(`\A(?:[a-z0-9]([-a-z0-9]*[a-z0-9])?)\z`), maxLen: 20},
		"appType":            se("app", "infra"),
		"configMapGenerator": b(),
	}),
}

// requiredFields are the fields a kind's request must carry once the gateway has filled in what it fills.
var requiredFields = map[string][]string{
	"SLO":          {"environmentRef", "service", "objective", "indicator"},
	"RolloutWatch": {"environmentRef", "appName", "cluster", "env"},
}

// platformOwned are the keys of an XR that Crossplane or the platform controls, named in the refusal so an agent that tries one
// is told it is not a field (and why). None is in any table, so none can be set whatever this list says.
var platformOwned = map[string]bool{
	"compositionRef": true, "compositionSelector": true, "compositionRevisionRef": true, "compositionRevisionSelector": true,
	"compositionUpdatePolicy": true, "providerConfigRef": true, "writeConnectionSecretToRef": true, "publishConnectionDetailsTo": true,
	"managementPolicies": true, "deletionPolicy": true, "resourceRefs": true, "claimRef": true, "crossplane": true,
	"environmentConfigRefs": true, "chart": true, "metadata": true, "status": true, "apiVersion": true, "kind": true,
}

// RequestableKinds is the closed list of kinds, sorted: the contract's requestable kinds that this file has a table for.
func RequestableKinds() []string {
	out := []string{}
	for k := range contractRequestable() {
		if kindFields[k] != nil {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// contractRequestable is requestable_kinds(contract): {kind: role} for each kind of the contract with a requestable role.
func contractRequestable() map[string]string {
	out := map[string]string{}
	kinds, _ := Contract().M["kinds"].(*definitions.OMap)
	if kinds == nil {
		return out
	}
	for _, key := range kinds.Keys {
		k, ok := kinds.M[key].(*definitions.OMap)
		if !ok {
			continue
		}
		role, _ := k.M["role"].(string)
		kind, _ := k.M["kind"].(string)
		if requestableRoles[role] && kind != "" {
			out[kind] = role
		}
	}
	return out
}

// -- building --------------------------------------------------------------------------------------------------------------

// XRRequest is one request as an agent made it: every field untrusted.
type XRRequest struct {
	App, Env, Kind, Name, Spec any
	// Namespaces are the exact namespaces the platform assigns to this app's environment (an SLO's matchers are forced to them): the
	// handler resolves them from the cluster; none for another kind.
	Namespaces []string
}

// BuiltXR is a request ready to be written: the path derived from the app and the name, the file's text, and the XR.
type BuiltXR struct {
	Path, Text, Name, Namespace, Kind, App, Env string
	XR                                          *definitions.OMap
	// Owned are the names the cloud holds for everyone that the request asks for, to be checked against the apps that exist.
	Owned []OwnedName
}

// OwnedName is a name in the request that must belong to the requesting app.
type OwnedName struct {
	Path, Value string
	Flat        bool // a name that may hold no dash: the app's name with its dashes taken out is its prefix
}

type checkCtx struct {
	pol        XRPolicy
	owned      []OwnedName
	namespaces []string
}

func asLabel(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && xrLabel.MatchString(s)
}

// BuildXR is xr_request.build, with the field tables above: the path, the text and the XR for one request, or a *core.Denied
// with a hint the agent can act on. Nothing in it makes a request.
func BuildXR(r XRRequest, reg Registry) (*BuiltXR, error) {
	app, ok := asLabel(r.App)
	if !ok {
		return nil, xrRefuse("XR-APP", xrEcho(r.App)+" is not an app name", "Pass the app's name as in tenants/<app>/ (e.g. flight-api).")
	}
	env, ok := asLabel(r.Env)
	if !ok || !IsLowerEnv(env) || CicdLike(env) {
		return nil, xrRefuse("XR-ENV", xrEcho(r.Env)+" is not a lower environment",
			"xr.request asks for lower environments only (dev, pr-*, agent-*); an upper one is a Flight change a person makes.")
	}
	kinds := RequestableKinds()
	kind, isStr := r.Kind.(string)
	if !isStr || kindFields[kind] == nil || contractRequestable()[kind] == "" {
		return nil, xrRefuse("XR-KIND", xrEcho(r.Kind)+" is not a kind an app requests through xr.request",
			"Request one of "+strings.Join(kinds, ", ")+" (airframe.capabilities describes each).")
	}
	if kind == "SLO" && !sloRequestsOpen {
		return nil, xrRefuse("XR-KIND", SLORefusalMessage, SLORefusalHint)
	}
	spec, ok := r.Spec.(*definitions.OMap)
	if !ok {
		return nil, xrRefuse("XR-SPEC", "spec is the XR's spec mapping, without metadata", "Pass only the fields under spec: (airframe.capabilities lists them for the kind).")
	}
	if _, has := spec.M["metadata"]; has {
		return nil, xrRefuse("XR-SPEC", "spec is the XR's spec mapping, without metadata", "Pass only the fields under spec: (airframe.capabilities lists them for the kind).")
	}
	if n := jsonLen(spec); n < 0 || n > MaxXRSpecBytes {
		return nil, xrRefuse("XR-SPEC", fmt.Sprintf("spec is larger than %d bytes", MaxXRSpecBytes), "Request a smaller XR.")
	}
	// What the request fixes, set here and never taken from the agent: a value the agent gave that says otherwise is refused.
	fixed := fixedFields(kind, app, env, reg)
	devName := app + "-" + reg.DevCluster + "-" + env
	out := definitions.OMap{M: map[string]any{}}
	cx := &checkCtx{pol: reg.XR.withDefaults(), namespaces: r.Namespaces}
	for _, k := range fixed.keys {
		if v, has := spec.M[k]; has && !xrEqual(v, fixed.vals[k]) {
			return nil, xrRefuse("XR-SPEC", fmt.Sprintf("spec.%s must be %s for this request", k, pyrepr.Value(fixed.vals[k])),
				fixed.hint)
		}
		out.Set(k, fixed.vals[k])
	}
	if ref := fixed.envRef; ref {
		cur, has := spec.M["environmentRef"]
		if has {
			m, ok := cur.(*definitions.OMap)
			if !ok || len(m.Keys) != 1 || m.M["name"] != devName {
				return nil, xrRefuse("XR-SPEC", "spec.environmentRef must be {name: "+devName+"} for this request",
					"The environment reference of a request is the app's own ApplicationEnvironment for this env on this cluster; leave it out.")
			}
		}
		out.Set("environmentRef", definitions.FromPlain(map[string]any{"name": devName}))
	}
	for _, k := range spec.Keys {
		if _, done := out.M[k]; done {
			continue
		}
		fr := kindFields[kind].fields[k]
		if fr == nil {
			why := "is not a field xr.request sets for " + kind
			hint := "The fields of a " + kind + " request are " + strings.Join(fieldNames(kindFields[kind]), ", ") + "."
			if platformOwned[k] {
				hint = "That is Crossplane's or the platform's to set, never a request's. " + hint
			}
			return nil, xrRefuse("XR-FIELD", "spec."+clip100(k)+" "+why, hint)
		}
		v, err := check(fr, spec.M[k], "spec."+k, app, cx)
		if err != nil {
			return nil, err
		}
		out.Set(k, v)
	}
	if kind == "ManagedPostgreSQL" && !cx.pol.AllowUnprotectedDB {
		if _, has := out.M["protect"]; !has {
			out.Set("protect", true) // a database an agent asks for is deletion-protected unless the platform says otherwise
		}
	}
	for _, k := range requiredFields[kind] {
		if _, has := out.M[k]; !has {
			return nil, xrRefuse("XR-FIELD", "spec."+k+" is required for a "+kind+" request", "The fields of a "+kind+" request are "+strings.Join(fieldNames(kindFields[kind]), ", ")+".")
		}
	}
	// The name: an ApplicationEnvironment's is the planner's (Python's rule); any other is the agent's, bound to the app.
	var name string
	switch n := r.Name.(type) {
	case nil:
	case string:
		name = n
	default:
		return nil, xrRefuse("XR-NAME", xrEcho(r.Name)+" is not an XR name", "Use a DNS label: lowercase, digits and '-', at most 63.")
	}
	if kind == "ApplicationEnvironment" {
		if name != "" && name != devName {
			return nil, xrRefuse("XR-NAME", "an ApplicationEnvironment for "+app+" "+env+" is named "+devName, "Leave name out.")
		}
		name = devName
	} else if name == "" {
		name = app + "-" + env + "-" + strings.ToLower(kind)
	}
	if !xrLabel.MatchString(name) {
		return nil, xrRefuse("XR-NAME", xrEcho(name)+" is not an XR name", "Use a DNS label: lowercase, digits and '-', at most 63.")
	}
	if !strings.HasPrefix(name, app+"-") {
		return nil, xrRefuse("XR-NAME", xrEcho(name)+" is not one of "+app+"'s names", "An app's XR names start with the app's name and a dash (e.g. "+app+"-"+env+"-"+strings.ToLower(kind)+").")
	}
	filename := name + ".yaml"
	xr := definitions.OMap{M: map[string]any{}}
	xr.Set("apiVersion", XRAPIVersion)
	xr.Set("kind", kind)
	md := definitions.OMap{M: map[string]any{}}
	md.Set("name", name)
	md.Set("namespace", "app-"+app+"-cicd")
	md.Set("annotations", XRAnnotations(app, filename, reg))
	xr.Set("metadata", &md)
	xr.Set("spec", &out)
	text := XRHeader + DumpYAML(&xr)
	if err := ReadBackXR(text, &xr); err != nil {
		return nil, xrRefuse("XR-SPEC", "the request does not read back as the one XR it was built as: "+err.Error(), "Request fewer or simpler fields.")
	}
	return &BuiltXR{Path: "tenants/" + app + "/xr-requests/" + filename, Text: text, Name: name, Namespace: "app-" + app + "-cicd",
		Kind: kind, App: app, Env: env, XR: &xr, Owned: cx.owned}, nil
}

type fixedSet struct {
	keys   []string
	vals   map[string]any
	hint   string
	envRef bool
}

func fixedFields(kind, app, env string, reg Registry) fixedSet {
	switch kind {
	case "ApplicationEnvironment":
		return fixedSet{keys: []string{"appName", "cluster", "env"}, vals: map[string]any{"appName": app, "cluster": reg.DevCluster, "env": env},
			hint: "An ApplicationEnvironment from xr.request is the app's env on this cluster; leave those fields out."}
	case "RolloutWatch":
		return fixedSet{keys: []string{"appName", "cluster", "env"}, vals: map[string]any{"appName": app, "cluster": reg.DevCluster, "env": env}, envRef: true,
			hint: "A RolloutWatch from xr.request watches the app's env on this cluster; leave those fields out."}
	case "SLO":
		return fixedSet{envRef: true}
	}
	return fixedSet{}
}

func fieldNames(r *rule) []string {
	out := make([]string, 0, len(r.fields))
	for k := range r.fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clip100(s string) string {
	if r := []rune(s); len(r) > 100 {
		return string(r[:100]) + "..."
	}
	return s
}

func bound(app, how, v string) bool {
	switch how {
	case appDash:
		return strings.HasPrefix(v, app+"-") && len(v) > len(app)+1
	case appFlat:
		flat := strings.ReplaceAll(app, "-", "")
		return strings.HasPrefix(v, flat) && len(v) > len(flat)
	}
	return true
}

func badField(path, why string) error {
	return xrRefuse("XR-FIELD", clip100(path)+" "+why, "airframe.capabilities {kind} describes each field of a kind; a request sets only those.")
}

// check validates one value against its rule and returns the value to write (the same, copied: nothing the agent sent is
// aliased into the XR).
func check(r *rule, v any, path, app string, cx *checkCtx) (any, error) {
	switch r.t {
	case tString:
		str, ok := v.(string)
		if !ok {
			return nil, badField(path, "must be a string")
		}
		if !utf8.ValidString(str) || strings.IndexFunc(str, unicode.IsControl) >= 0 || len(str) > 1024 {
			return nil, badField(path, "must be plain text of at most 1024 bytes")
		}
		if r.maxLen > 0 && utf8.RuneCountInString(str) > r.maxLen {
			return nil, badField(path, fmt.Sprintf("is longer than %d characters", r.maxLen))
		}
		if len(r.enumS) > 0 {
			for _, e := range r.enumS {
				if e == str {
					if err := policyString(r.policy, str, path, cx.pol); err != nil {
						return nil, err
					}
					return str, nil
				}
			}
			return nil, badField(path, "must be one of "+strings.Join(r.enumS, ", "))
		}
		if r.re != nil && !r.re.MatchString(str) {
			return nil, badField(path, "is not in the form the XRD gives it")
		}
		if r.bind != "" && !bound(app, r.bind, str) {
			return nil, badField(path, "must start with the app's name and a dash ("+app+"-...): a request names its own app's resources only")
		}
		if r.global {
			cx.owned = append(cx.owned, OwnedName{Path: path, Value: str, Flat: r.bind == appFlat})
		}
		if err := policyString(r.policy, str, path, cx.pol); err != nil {
			return nil, err
		}
		return str, nil
	case tInt:
		n, ok := v.(int64)
		if !ok {
			return nil, badField(path, "must be a whole number")
		}
		if r.policy == "storage" && n > cx.pol.MaxStorageGiB {
			return nil, badField(path, fmt.Sprintf("is above the %d GiB this platform lets an agent request (XR_MAX_DB_STORAGE_GIB)", cx.pol.MaxStorageGiB))
		}
		if len(r.enumI) > 0 {
			for _, e := range r.enumI {
				if e == n {
					return n, nil
				}
			}
			return nil, badField(path, "is not one of the values the XRD allows")
		}
		if r.ranged && (float64(n) < r.min || float64(n) > r.max) {
			return nil, badField(path, fmt.Sprintf("must be from %v to %v", r.min, r.max))
		}
		return n, nil
	case tNumber:
		var f float64
		switch x := v.(type) {
		case int64:
			f = float64(x)
		case float64:
			f = x
		default:
			return nil, badField(path, "must be a number")
		}
		if math.IsNaN(f) || math.IsInf(f, 0) || (r.ranged && (f < r.min || f > r.max || (r.exclMin && f == r.min))) {
			return nil, badField(path, fmt.Sprintf("must be from %v to %v", r.min, r.max))
		}
		return v, nil
	case tBool:
		bl, ok := v.(bool)
		if !ok {
			return nil, badField(path, "must be true or false")
		}
		switch {
		case r.policy == "protect" && !bl && !cx.pol.AllowUnprotectedDB:
			return nil, badField(path, "may not be false: deleting a database is a person's change (XR_ALLOW_UNPROTECTED_DB)")
		case r.policy == "emptyOnDelete" && bl && !cx.pol.AllowEmptyOnDelete:
			return nil, badField(path, "may not be true: emptying a bucket is a person's change (XR_ALLOW_EMPTY_ON_DELETE)")
		}
		return bl, nil
	case tSelector:
		return checkSelector(v, path, cx)
	case tCIDR:
		str, ok := v.(string)
		if !ok || !PrivateCIDR(str) {
			return nil, badField(path, "must be a private IPv4 range (10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16) no wider than a /16, written as its network address")
		}
		return str, nil
	case tArray:
		l, ok := v.([]any)
		if !ok {
			return nil, badField(path, "must be a list")
		}
		max := r.maxItems
		if r.policy == "consumers" && cx.pol.MaxConsumers < max {
			max = cx.pol.MaxConsumers
		}
		if len(l) > max {
			return nil, badField(path, fmt.Sprintf("has more than %d entries", max))
		}
		out := make([]any, len(l))
		for i, e := range l {
			c, err := check(r.item, e, fmt.Sprintf("%s[%d]", path, i), app, cx)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case tObject:
		m, ok := v.(*definitions.OMap)
		if !ok {
			return nil, badField(path, "must be a mapping")
		}
		out := definitions.OMap{M: map[string]any{}}
		for _, k := range m.Keys {
			fr := r.fields[k]
			if fr == nil {
				return nil, xrRefuse("XR-FIELD", path+"."+clip100(k)+" is not a field of this request",
					"The fields under "+path+" are "+strings.Join(fieldNames(r), ", ")+".")
			}
			c, err := check(fr, m.M[k], path+"."+k, app, cx)
			if err != nil {
				return nil, err
			}
			out.Set(k, c)
		}
		for _, k := range r.required {
			if _, has := out.M[k]; !has {
				return nil, badField(path+"."+k, "is required")
			}
		}
		return &out, nil
	}
	return nil, badField(path, "is not allowed")
}

// PrivateCIDR says a range is an IPv4 network address inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16 and no wider than
// a /16.
func PrivateCIDR(s string) bool {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p.Addr().Is4In6() || p.Masked() != p || p.Bits() < 16 || s != p.String() {
		return false
	}
	for _, r := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if rp := netip.MustParsePrefix(r); rp.Contains(p.Addr()) && p.Bits() >= rp.Bits() {
			return true
		}
	}
	return false
}

func xrEqual(a, b any) bool {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	}
	return false
}

// jsonLen is the length of the value's compact JSON (Python measures json.dumps with default separators; a few bytes
// either way do not matter against a 32 KiB bound). -1 for a value that does not encode.
func jsonLen(v any) int {
	b, err := json.Marshal(definitions.Plain(v))
	if err != nil {
		return -1
	}
	return len(b)
}

// -- reading the text back -------------------------------------------------------------------------------------------------

// ReadBackXR is the check on the finished file: the text is exactly one YAML document with no anchor, alias, tag, merge key or
// duplicate key, and it holds the XR that was built (comparing numbers by value). The YAML is built by this gateway from
// validated values, so this is the second guard, against the emitter and against a value whose spelling a reader would
// take for something else.
func ReadBackXR(text string, want *definitions.OMap) error {
	dec := yaml.NewDecoder(strings.NewReader(text))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("more than one YAML document")
	}
	if err := plainNodes(&doc, 0); err != nil {
		return err
	}
	var got any
	if err := doc.Decode(&got); err != nil {
		return err
	}
	if !sameValue(got, definitions.Plain(want)) {
		return errors.New("the text reads as another value")
	}
	return nil
}

func plainNodes(n *yaml.Node, depth int) error {
	if depth > 32 {
		return errors.New("nested too deeply")
	}
	if n.Anchor != "" || n.Kind == yaml.AliasNode || n.Alias != nil {
		return errors.New("an anchor or alias")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return errors.New("a tag")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Value == "<<" {
				return errors.New("a key that is not a plain name")
			}
			if seen[k.Value] {
				return errors.New("a duplicate key")
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if err := plainNodes(c, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func sameValue(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, has := y[k]
			if !has || !sameValue(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case nil:
		return b == nil
	}
	fa, oka := asFloat(a)
	fb, okb := asFloat(b)
	return oka && okb && fa == fb
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// -- platform settings ---------------------------------------------------------------------------------------------------------

// XRPolicy are the bounds the platform puts on what an agent may request, each refused above its maximum. The defaults are the
// safe ones: nothing that deletes or empties data, no network of the agent's choosing, no chat channel, small databases.
type XRPolicy struct {
	Regions            []string // XR_REGIONS: the regions a request may name (default us-east-1, the XRDs' own default)
	VPCCidrs           []string // XR_VPC_CIDRS: the vpcCidr values a request may name (default none: the platform's default network)
	SlackChannels      []string // XR_SLACK_CHANNELS: the channels a RolloutWatch may notify (default none)
	DBSizes            []string // XR_DB_SIZES: the database size classes (default small, medium)
	MaxStorageGiB      int64    // XR_MAX_DB_STORAGE_GIB: at most 256, the XRD's own largest (default 64)
	MaxConsumers       int      // XR_MAX_CONSUMERS: consumers of a queue or a bucket, at most 10 (default 3)
	AllowUnprotectedDB bool     // XR_ALLOW_UNPROTECTED_DB: protect: false (default off: a database is deletion-protected)
	AllowEmptyOnDelete bool     // XR_ALLOW_EMPTY_ON_DELETE: emptyOnDelete: true (default off)
	SLOLabels          []string // XR_SLO_LABELS: the only labels an SLO's matchers may select on (default job, service, route, method, code, status_code, le)
}

func (p XRPolicy) withDefaults() XRPolicy {
	if p.Regions == nil {
		p.Regions = []string{"us-east-1"}
	}
	if p.DBSizes == nil {
		p.DBSizes = []string{"small", "medium"}
	}
	if p.MaxStorageGiB <= 0 {
		p.MaxStorageGiB = 64
	}
	if p.MaxStorageGiB > 256 {
		p.MaxStorageGiB = 256
	}
	if p.SLOLabels == nil {
		p.SLOLabels = []string{"job", "service", "route", "method", "code", "status_code", "le"}
	}
	if p.MaxConsumers <= 0 {
		p.MaxConsumers = 3
	}
	return p
}

func inList(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func policyString(name, v, path string, p XRPolicy) error {
	switch name {
	case "region":
		if !inList(p.Regions, v) {
			return badField(path, "is not a region this platform lets an agent choose (XR_REGIONS: "+strings.Join(p.Regions, ", ")+")")
		}
	case "vpc":
		if !inList(p.VPCCidrs, v) {
			return badField(path, "is not a network this platform lets an agent choose: leave it out for the platform's own (XR_VPC_CIDRS)")
		}
	case "slack":
		if !inList(p.SlackChannels, v) {
			return badField(path, "is not a channel this platform lets an agent notify (XR_SLACK_CHANNELS)")
		}
	case "dbsize":
		if !inList(p.DBSizes, v) {
			return badField(path, "is not a database size this platform lets an agent request (XR_DB_SIZES: "+strings.Join(p.DBSizes, ", ")+")")
		}
	}
	return nil
}

// -- names the cloud holds for everyone ------------------------------------------------------------------------------------------

func flatName(s string) string { return strings.ReplaceAll(strings.ToLower(s), "-", "") }

// CheckOwners decides that every global name in the request belongs to the requesting app, from the apps that are onboarded (the
// directories of tenants/ in the tenants repository). A dashed name belongs to the onboarded app with the LONGEST name that is
// a dash-terminated prefix of it, and only that app may ask for it (so `gate` cannot take `gate-api-prod-assets`). A flattened
// name (a storage account takes no dash) is compared by flattened app names: where another app's flattened name is a prefix of this
// app's, equal to it, or has it as a prefix, no flattened name is decided for either, and the request is refused.
func (b *BuiltXR) CheckOwners(onboarded []string) error {
	apps := append([]string{}, onboarded...)
	if !inList(apps, b.App) {
		apps = append(apps, b.App)
	}
	for _, o := range b.Owned {
		if o.Flat {
			mine := flatName(b.App)
			for _, other := range apps {
				if other == b.App {
					continue
				}
				f := flatName(other)
				if strings.HasPrefix(f, mine) || strings.HasPrefix(mine, f) {
					return xrRefuse("XR-FIELD", clip100(o.Path)+" cannot be decided: the names of "+b.App+" and "+other+" are the same with the dashes taken out, or one starts the other",
						"A storage account name has no dash, so it cannot say which app it belongs to; ask a person for it.")
				}
			}
			continue
		}
		owner := ""
		for _, a := range apps {
			if strings.HasPrefix(o.Value, a+"-") && len(a) > len(owner) {
				owner = a
			}
		}
		if owner != b.App {
			return xrRefuse("XR-FIELD", clip100(o.Path)+" is a name of "+clip100(owner)+"'s, not "+b.App+"'s",
				"A name belongs to the onboarded app whose name is its longest prefix; request names that no other app's name starts.")
		}
	}
	return nil
}

// -- Prometheus matchers -----------------------------------------------------------------------------------------------------------

var (
	promLabel   = regexp.MustCompile(`\A[a-zA-Z_][a-zA-Z0-9_]{0,63}\z`)
	promValue   = regexp.MustCompile(`\A[A-Za-z0-9 _.:/@+=-]{1,128}\z`)
	anyNamespac = regexp.MustCompile(`(?i)namespace`)
)

// An SLO through xr.request is refused for now. The SLO composition scopes every query to the XR's own namespace (Airframe
// compositions/slo/composition.yaml:305, :328), meant to be the environment namespace where the chart puts it; an xr.request XR is
// applied into app-<app>-cicd, so such an SLO would render and read no data, which is worse than a refusal. The matcher checks below stay
// for when xr.request can place an SLO in an environment namespace; set sloRequestsOpen (tests only, today) to reach them.
var sloRequestsOpen = false

// SLORequestsOpen says whether xr.request takes SLOs (it does not yet).
func SLORequestsOpen() bool { return sloRequestsOpen }

const (
	SLORefusalMessage = "an SLO requested through xr.request would be scoped to the app's cicd namespace (app-<app>-cicd) and read no data"
	SLORefusalHint    = "Declare it in the app's chart values instead: under `slos:` in gitops-<app>/<cluster>/<env>/values.yaml for that environment " +
		"(the airframe-application chart renders it into the environment namespace), as a pull request on that repository. " +
		"xr.request will take SLOs again once it can place one in an environment namespace."
)

// sloPatterns are the contract's own patterns for an SLO's metric and filters, read from the AppSpec schema (chartValues.slos), so the
// gateway refuses what the composition's validation would and cannot drift from it.
var sloPatterns = sync.OnceValue(func() map[string]*regexp.Regexp {
	var doc map[string]any
	if err := json.Unmarshal(appSpecJSON, &doc); err != nil {
		panic("the AppSpec schema: " + err.Error())
	}
	node := doc
	for _, k := range []string{"$defs", "chartValues", "properties", "slos", "items", "properties", "indicator", "properties"} {
		next, _ := node[k].(map[string]any)
		if next == nil {
			panic("the AppSpec schema has no " + k + " on the way to an SLO's indicator")
		}
		node = next
	}
	out := map[string]*regexp.Regexp{}
	for _, f := range []string{"metric", "totalFilter", "errorFilter"} {
		prop, _ := node[f].(map[string]any)
		pat, _ := prop["pattern"].(string)
		if pat == "" {
			panic("the AppSpec schema gives an SLO's " + f + " no pattern")
		}
		out[f] = definitions.CompilePattern(pat)
	}
	return out
})

// checkSelector reads a mapping of label to value and writes it as the matcher list the composition puts inside braces. The agent
// supplies no selector syntax and no tenancy: the composition (Airframe compositions/slo) forces its own `namespace` matcher first
// and refuses any `namespace` or `__name__` matcher, so the gateway writes none, refuses a request that names one early, and
// refuses an environment of several namespaces (an SLO covers exactly its own). The agent may add equality matchers on a CLOSED
// list of label names (XR_SLO_LABELS) with a plain value (no quote, backslash, brace or line break). The text written is checked
// against the contract's own filter pattern before it is returned.
func checkSelector(v any, path string, cx *checkCtx) (any, error) {
	m, ok := v.(*definitions.OMap)
	if !ok || len(m.Keys) == 0 || len(m.Keys) > 8 {
		return nil, badField(path, "must be a mapping of one to eight labels to the values they equal (the gateway writes the matcher)")
	}
	for _, n := range cx.namespaces {
		if !xrLabel.MatchString(n) {
			return nil, badField(path, "cannot be written: a namespace is not a plain name")
		}
	}
	if len(cx.namespaces) == 0 {
		return nil, badField(path, "cannot be written: the platform has assigned this app's environment no namespace on this cluster to select in")
	}
	if len(cx.namespaces) > 1 {
		return nil, badField(path, "cannot be written: an SLO covers exactly its own namespace, and this environment has "+
			strconv.Itoa(len(cx.namespaces))+" ("+strings.Join(cx.namespaces, ", ")+"), so it needs one SLO per namespace; xr.request cannot place an SLO "+
			"in a chosen one of them, so ask a person to add those SLOs")
	}
	names := append([]string{}, m.Keys...)
	sort.Strings(names)
	var pairs []string
	for _, k := range names {
		if k == "__name__" || anyNamespac.MatchString(k) {
			return nil, badField(path+"."+k, "is a namespace or __name__ matcher, which an SLO may not have: the composition scopes every SLO to its own namespace and refuses these; select on "+
				strings.Join(cx.pol.SLOLabels, ", ")+" instead")
		}
		val, isStr := m.M[k].(string)
		if !promLabel.MatchString(k) || !inList(cx.pol.SLOLabels, k) {
			return nil, badField(path+"."+k, "is not a label an agent may select on (XR_SLO_LABELS: "+strings.Join(cx.pol.SLOLabels, ", ")+")")
		}
		if !isStr || !promValue.MatchString(val) {
			return nil, badField(path+"."+k, "must be plain text of 1 to 128 characters, with no quote, backslash, brace or line break")
		}
		pairs = append(pairs, k+`="`+val+`"`)
	}
	out := strings.Join(pairs, ",")
	if re := sloPatterns()["totalFilter"]; !re.MatchString(out) {
		return nil, badField(path, "would be written as "+clip100(out)+", which is not a filter the contract accepts (comma-separated label=\"value\" matchers)")
	}
	return out, nil
}

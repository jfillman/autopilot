package airframe

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

var xrReg = Registry{DevCluster: "dev-cluster", UpperClusters: []string{"up-cluster"}, TenantsRepo: "gitops-tenants", Owner: "acme"}

func spec(t *testing.T, js string) any {
	t.Helper()
	var v any
	d := json.NewDecoder(strings.NewReader(js))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	out, err := FromArgs(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func build(t *testing.T, app, env, kind any, name any, js string) (*BuiltXR, error) {
	t.Helper()
	return BuildXR(XRRequest{App: app, Env: env, Kind: kind, Name: name, Spec: spec(t, js), Namespaces: []string{"app-flight-api-dev"}}, xrReg)
}

// xrRegOpen is the registry of a platform that lets agents choose a network and a channel.
var xrRegOpen = Registry{DevCluster: "dev-cluster", UpperClusters: []string{"up-cluster"}, TenantsRepo: "gitops-tenants", Owner: "acme",
	XR: XRPolicy{VPCCidrs: []string{"10.43.0.0/16"}, SlackChannels: []string{"#deploys"}}}

func buildOpen(t *testing.T, app, kind any, name any, js string) (*BuiltXR, error) {
	t.Helper()
	return BuildXR(XRRequest{App: app, Env: "dev", Kind: kind, Name: name, Spec: spec(t, js), Namespaces: []string{"app-flight-api-dev"}}, xrRegOpen)
}

func ruleOf(err error) string {
	var d *core.Denied
	if errors.As(err, &d) {
		return d.Rule
	}
	return ""
}

func TestAnXRRequestIsBuiltFromTheKindsOwnFieldsAndNothingElse(t *testing.T) {
	b, err := build(t, "flight-api", "dev", "MessageQueue", "flight-api-orders", `{"lockSeconds": 60, "fifo": true, "deadLetter": {"maxReceiveCount": 3}}`)
	if err != nil {
		t.Fatal(err)
	}
	if b.Path != "tenants/flight-api/xr-requests/flight-api-orders.yaml" || b.Namespace != "app-flight-api-cicd" || b.Name != "flight-api-orders" {
		t.Fatalf("%+v", b)
	}
	md := b.XR.M["metadata"].(*definitions.OMap)
	if md.M["namespace"] != "app-flight-api-cicd" || b.XR.M["apiVersion"] != XRAPIVersion || b.XR.M["kind"] != "MessageQueue" {
		t.Fatalf("%v", definitions.Plain(b.XR))
	}
	if !strings.HasPrefix(b.Text, XRHeader) {
		t.Fatalf("no header: %q", b.Text)
	}
	// the metadata is the gateway's: the three keys, the annotations Airframe's planner writes
	if len(md.Keys) != 3 {
		t.Fatalf("metadata has %v", md.Keys)
	}
}

func TestTheDefaultNameAndAnApplicationEnvironmentsNameAreDerived(t *testing.T) {
	b, err := build(t, "flight-api", "dev", "ObjectStorage", nil, `{"versioning": true}`)
	if err != nil || b.Name != "flight-api-dev-objectstorage" {
		t.Fatalf("%v %+v", err, b)
	}
	b, err = build(t, "flight-api", "dev", "ApplicationEnvironment", nil, `{"configMapGenerator": true}`)
	if err != nil || b.Name != "flight-api-dev-cluster-dev" {
		t.Fatalf("%v %+v", err, b)
	}
	sp := b.XR.M["spec"].(*definitions.OMap)
	if sp.M["appName"] != "flight-api" || sp.M["cluster"] != "dev-cluster" || sp.M["env"] != "dev" {
		t.Fatalf("%v", definitions.Plain(sp))
	}
	if _, err = build(t, "flight-api", "dev", "ApplicationEnvironment", "flight-api-x", `{}`); ruleOf(err) != "XR-NAME" {
		t.Fatalf("%v", err)
	}
	if _, err = build(t, "flight-api", "dev", "ApplicationEnvironment", nil, `{"env": "prod"}`); ruleOf(err) != "XR-SPEC" {
		t.Fatalf("an env that is not the request's: %v", err)
	}
}

// What an agent must never be able to put in a request, one case each. Every one was accepted by the Python builder.
func TestEveryPlatformOwnedOrWiderFieldIsRefused(t *testing.T) {
	tenants := []struct{ name, kind, js string }{
		{"providerConfigRef to another tenant's credentials", "ManagedPostgreSQL", `{"providerConfigRef": {"name": "other-tenant"}}`},
		{"compositionRef", "MessageQueue", `{"compositionRef": {"name": "evil"}}`},
		{"compositionSelector", "MessageQueue", `{"compositionSelector": {"matchLabels": {"a": "b"}}}`},
		{"compositionRevisionRef", "ObjectStorage", `{"compositionRevisionRef": {"name": "x"}}`},
		{"compositionUpdatePolicy", "ObjectStorage", `{"compositionUpdatePolicy": "Automatic"}`},
		{"writeConnectionSecretToRef into another namespace", "MessageQueue", `{"writeConnectionSecretToRef": {"name": "s", "namespace": "kube-system"}}`},
		{"publishConnectionDetailsTo", "MessageQueue", `{"publishConnectionDetailsTo": {"name": "s"}}`},
		{"managementPolicies", "ObjectStorage", `{"managementPolicies": ["Observe"]}`},
		{"deletionPolicy", "ObjectStorage", `{"deletionPolicy": "Orphan"}`},
		{"resourceRefs", "ObjectStorage", `{"resourceRefs": []}`},
		{"claimRef", "ObjectStorage", `{"claimRef": {"name": "x"}}`},
		{"crossplane", "ObjectStorage", `{"crossplane": {"resourceRefs": []}}`},
		{"environmentConfigRefs", "ObjectStorage", `{"environmentConfigRefs": [{"name": "x"}]}`},
		{"metadata", "ObjectStorage", `{"metadata": {"annotations": {"crossplane.io/external-name": "someone-elses-bucket"}}}`},
		{"a chart source of an ApplicationEnvironment", "ApplicationEnvironment", `{"chart": {"repoURL": "https://evil.example/charts", "chart": "x", "targetRevision": "1"}}`},
		{"a made-up field", "MessageQueue", `{"surprise": 1}`},
		{"an unknown field under deadLetter", "MessageQueue", `{"deadLetter": {"redrive": "x"}}`},
		{"an unknown field of a consumer", "MessageQueue", `{"consumers": [{"awsLambdaTarget": "flight-api-fn", "roleArn": "arn:aws:iam::1:role/x"}]}`},
		{"another app's bucket (adopting it)", "ObjectStorage", `{"bucketName": "someone-elses-bucket"}`},
		{"another app's queue", "MessageQueue", `{"queueName": "payments-orders"}`},
		{"another app's database server", "ManagedPostgreSQL", `{"serverName": "payments-db"}`},
		{"another app's storage account", "ObjectStorage", `{"storageAccountName": "paymentsdata"}`},
		{"another app's resource group", "MessageQueue", `{"resourceGroup": "payments-rg"}`},
		{"a consumer that is another app's Lambda target", "MessageQueue", `{"consumers": [{"awsLambdaTarget": "payments-fn"}]}`},
		{"a database open to the world", "ManagedPostgreSQL", `{"allowedCidrs": ["0.0.0.0/0"]}`},
		{"a database open to a public range", "ManagedPostgreSQL", `{"allowedCidrs": ["8.8.8.0/24"]}`},
		{"a database open to a wide private range", "ManagedPostgreSQL", `{"allowedCidrs": ["10.0.0.0/8"]}`},
		{"a database range that is not a network address", "ManagedPostgreSQL", `{"allowedCidrs": ["10.1.2.3/16"]}`},
		{"an ipv6 range", "ManagedPostgreSQL", `{"allowedCidrs": ["fd00::/16"]}`},
		{"a slo for another app's environment", "SLO", `{"environmentRef": {"name": "payments-dev-cluster-dev"}, "service": "flight-api-x", "objective": 99, "indicator": {"type": "availability", "metric": "m", "totalFilter": {"job": "flight-api"}, "errorFilter": {"code": "5"}}}`},
		{"a rollout watch for another app", "RolloutWatch", `{"environmentRef": {"name": "flight-api-dev-cluster-dev"}, "appName": "payments"}`},
		{"a rollout watch on another cluster", "RolloutWatch", `{"environmentRef": {"name": "flight-api-dev-cluster-dev"}, "cluster": "up-cluster"}`},
		{"a number as a string", "MessageQueue", `{"lockSeconds": "60"}`},
		{"a float for an integer", "MessageQueue", `{"lockSeconds": 60.5}`},
		{"out of range", "MessageQueue", `{"lockSeconds": 999999}`},
		{"an enum that is not one", "ManagedPostgreSQL", `{"size": "huge"}`},
		{"a string with a control character", "SLO", `{"environmentRef": {"name": "flight-api-dev-cluster-dev"}, "service": "flight-api-s", "objective": 99, "indicator": {"type": "availability", "metric": "m\nx", "totalFilter": {"job": "flight-api"}, "errorFilter": {"code": "5"}}}`},
		{"a spec that is a list", "MessageQueue", `[]`},
	}
	for _, c := range tenants {
		t.Run(c.name, func(t *testing.T) {
			b, err := build(t, "flight-api", "dev", c.kind, nil, c.js)
			if err == nil {
				t.Fatalf("accepted: %s", b.Text)
			}
			if r := ruleOf(err); r != "XR-FIELD" && r != "XR-SPEC" {
				t.Fatalf("refused as %q: %v", r, err)
			}
		})
	}
}

func TestKindsThatBecomeAuthorityOrPlatformObjectsAreNeverRequestable(t *testing.T) {
	// Agent is what the definition loader reads from app-<app>-cicd as an agent's authority; the rest are the platform's, a
	// bootstrap (a new repository) or kinds that grant credentials, roles or cluster objects.
	for _, kind := range []string{"Agent", "AgentRun", "AgentRepository", "SecretStore", "TektonCICD", "PythonApplication", "NodeJSApplication",
		"GoApplication", "SpringBootApplication", "InfraService", "LambdaFunction", "AzureFunction", "AwsLambdaTarget", "AwsEcsTarget",
		"AzureContainerAppTarget", "Dex", "RabbitMQ", "Redis", "PostgreSQL", "MongoDB",
		"Secret", "ClusterRole", "ClusterRoleBinding", "RoleBinding", "ServiceAccount", "ProviderConfig", "Composition", "Function",
		"DeploymentRuntimeConfig", "CustomResourceDefinition", "CompositeResourceDefinition", "ValidatingAdmissionPolicy",
		"ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "Application", "AppProject", "ApplicationSet", "Namespace",
		"", "managedpostgresql", "MessageQueue ", "../Agent"} {
		_, err := build(t, "flight-api", "dev", kind, nil, `{}`)
		if ruleOf(err) != "XR-KIND" {
			t.Errorf("%q: %v", kind, err)
		}
	}
	if _, err := build(t, "flight-api", "dev", []any{"MessageQueue"}, nil, `{}`); ruleOf(err) != "XR-KIND" {
		t.Errorf("a list: %v", err)
	}
	if got := strings.Join(RequestableKinds(), ","); got != "ApplicationEnvironment,ManagedPostgreSQL,MessageQueue,ObjectStorage,RolloutWatch,SLO" {
		t.Fatalf("the closed list is %s", got)
	}
}

func TestTheAppTheEnvAndTheNameAreBoundAndNoAnnotationOrLabelIsTheAgents(t *testing.T) {
	for _, app := range []any{"", "Flight", "flight/api", "../x", "flight-api\n", "-x", 7, nil, "a b", strings.Repeat("a", 64)} {
		if _, err := build(t, app, "dev", "MessageQueue", nil, `{}`); ruleOf(err) != "XR-APP" {
			t.Errorf("app %v: %v", app, err)
		}
	}
	for _, env := range []any{"prod", "staging", "dev\n", "Dev", "", 3, "pr-1/../x"} {
		if _, err := build(t, "flight-api", env, "MessageQueue", nil, `{}`); ruleOf(err) != "XR-ENV" {
			t.Errorf("env %v: %v", env, err)
		}
	}
	for _, env := range []string{"dev", "development", "sandbox", "scratch", "pr-12", "agent-x"} {
		if _, err := build(t, "flight-api", env, "MessageQueue", nil, `{}`); err != nil {
			t.Errorf("env %s: %v", env, err)
		}
	}
	for _, name := range []any{"orders", "flight-api", "flight-api-", "flight-apiorders", "Flight-api-x", "flight-api-x.y", "flight-api/../x", "flight-api-\n", 5,
		"payments-orders", strings.Repeat("a", 64), "-flight-api-x", "flight-api-x\x00"} {
		if _, err := build(t, "flight-api", "dev", "MessageQueue", name, `{}`); ruleOf(err) != "XR-NAME" {
			t.Errorf("name %v: %v", name, err)
		}
	}
	// the file is under the app's own directory, whatever the name, and the name is the file's
	b, err := build(t, "flight-api", "dev", "MessageQueue", "flight-api-q", `{}`)
	if err != nil || b.Path != "tenants/flight-api/xr-requests/flight-api-q.yaml" {
		t.Fatalf("%v %+v", err, b)
	}
	if strings.Count(b.Text, "\n---") != 0 || strings.Contains(b.Text, "&") && strings.Contains(b.Text, "*") {
		t.Fatalf("%s", b.Text)
	}
}

func TestAnAgentsStringsCannotInjectStructureIntoTheFile(t *testing.T) {
	// every string that reaches the file is read back: one document, the value that was built, no anchor, tag, alias or duplicate key
	hostile := []string{"a: b\n---\nkind: Agent", "&x *x", "!!python/object x", "{{x}}", "#comment", "x\ny", "- a", "\"quoted\"", "'single'", " ", "<<: *a"}
	for _, h := range hostile {
		js, _ := json.Marshal(map[string]any{"environmentRef": map[string]any{"name": "flight-api-dev-cluster-dev"}, "service": "flight-api-s", "objective": 99,
			"indicator": map[string]any{"type": "availability", "metric": "m", "totalFilter": map[string]any{"job": h}, "errorFilter": map[string]any{"code": "5"}}})
		b, err := build(t, "flight-api", "dev", "SLO", nil, string(js))
		if err == nil {
			if e := ReadBackXR(b.Text, b.XR); e != nil {
				t.Errorf("%q read back: %v\n%s", h, e, b.Text)
			}
			if strings.Contains(b.Text, "kind: Agent\n") {
				t.Errorf("%q smuggled a kind: %s", h, b.Text)
			}
		}
	}
	for _, text := range []string{"a: 1\n---\nb: 2\n", "a: &x 1\nb: *x\n", "a: !!str 1\n", "a: 1\na: 2\n", "<<: {a: 1}\n", "a: !!python/object:x y\n"} {
		if ReadBackXR(text, &definitions.OMap{M: map[string]any{}}) == nil {
			t.Errorf("read back accepted %q", text)
		}
	}
}

func TestAnXRThatIsTooBigIsRefused(t *testing.T) {
	big := strings.Repeat("a", 40000)
	if _, err := build(t, "flight-api", "dev", "MessageQueue", nil, `{"namespaceName": "`+big+`"}`); ruleOf(err) != "XR-SPEC" {
		t.Fatal(err)
	}
	if _, err := build(t, "flight-api", "dev", "MessageQueue", nil, `{"metadata": {}}`); ruleOf(err) != "XR-SPEC" {
		t.Fatal(err)
	}
}

func TestEveryDefaultTheCommonRequestsUseStillWorks(t *testing.T) {
	for kind, js := range map[string]string{
		"ManagedPostgreSQL":      `{"cloud": "aws", "serverName": "flight-api-db", "version": "17", "size": "small", "storageGiB": 32, "databaseName": "orders", "backupRetentionDays": 7, "allowedCidrs": ["10.43.0.0/16", "192.168.4.0/24"], "protect": true, "region": "us-east-1", "vpcCidr": "10.43.0.0/16"}`,
		"MessageQueue":           `{"queueName": "flight-api-orders", "fifo": false, "lockSeconds": 30, "retentionSeconds": 345600, "deadLetter": {"enabled": true, "maxReceiveCount": 5}, "consumers": [{"awsLambdaTarget": "flight-api-fn", "access": "sendReceive", "trigger": true, "batchSize": 10}]}`,
		"ObjectStorage":          `{"bucketName": "flight-api-files", "versioning": true, "storageAccountName": "flightapifiles", "consumers": [{"awsLambdaTarget": "flight-api-fn", "access": "read"}]}`,
		"RolloutWatch":           `{"notifications": {"slack": {"enabled": true, "channel": "#deploys"}}}`,
		"SLO":                    `{"service": "flight-api-svc", "objective": 99.9, "indicator": {"type": "latency", "metric": "http_request_duration_seconds", "totalFilter": {"job": "flight-api"}, "errorFilter": {"code": "500"}, "latencyThreshold": "0.5"}}`,
		"ApplicationEnvironment": `{"appType": "app"}`,
	} {
		b, err := buildOpen(t, "flight-api", kind, nil, js)
		if err != nil {
			t.Errorf("%s: %v", kind, err)
			continue
		}
		if e := ReadBackXR(b.Text, b.XR); e != nil {
			t.Errorf("%s: %v", kind, e)
		}
	}
}

func FuzzBuildXR(f *testing.F) {
	for _, s := range []string{`{}`, `{"lockSeconds": 5}`, `{"metadata": 1}`, `{"a": {"b": [1, 2, {"c": null}]}}`, `{"consumers": [{}]}`} {
		f.Add(s, "MessageQueue", "flight-api", "dev", "flight-api-q")
	}
	f.Fuzz(func(t *testing.T, js, kind, app, env, name string) {
		var v any
		d := json.NewDecoder(strings.NewReader(js))
		d.UseNumber()
		if d.Decode(&v) != nil {
			return
		}
		sp, err := FromArgs(v)
		if err != nil {
			return
		}
		b, err := BuildXR(XRRequest{App: app, Env: env, Kind: kind, Name: name, Spec: sp}, xrReg)
		if err != nil {
			var den *core.Denied
			if !errors.As(err, &den) {
				t.Fatalf("a refusal that is not a refusal: %v", err)
			}
			return
		}
		// whatever was built is one XR of the app, in its own directory, of a requestable kind, read back as built
		if !strings.HasPrefix(b.Path, "tenants/"+b.App+"/xr-requests/") || strings.Contains(strings.TrimPrefix(b.Path, "tenants/"+b.App+"/xr-requests/"), "/") {
			t.Fatalf("path %q", b.Path)
		}
		if kindFields[b.Kind] == nil || !xrLabel.MatchString(b.App) || !strings.HasPrefix(b.Name, b.App+"-") || !IsLowerEnv(b.Env) {
			t.Fatalf("built %+v", b)
		}
		if err := ReadBackXR(b.Text, b.XR); err != nil {
			t.Fatalf("%v\n%s", err, b.Text)
		}
		sp2 := b.XR.M["spec"].(*definitions.OMap)
		for k := range sp2.M {
			if platformOwned[k] {
				t.Fatalf("platform field %s", k)
			}
		}
	})
}

// An agent supplies no selector syntax and no tenancy: the composition (Airframe compositions/slo, dc5ffa6) forces the SLO's own
// namespace as the first matcher and refuses a namespace or __name__ matcher, so the gateway writes only the agent's equality
// matchers on a closed list of labels, and refuses, early, anything the contract's patterns or the composition would refuse.
func TestAnSLOTakesNoSelectorSyntaxAndNoNamespaceMatcher(t *testing.T) {
	slo := func(metric, total, errf string) string {
		return `{"environmentRef": {"name": "flight-api-dev-cluster-dev"}, "service": "flight-api-s", "objective": 99, "indicator": {"type": "availability", "metric": ` + metric + `, "totalFilter": ` + total + `, "errorFilter": ` + errf + `}}`
	}
	good := `{"job": "flight-api"}`
	bad := []string{
		// the reviewer's four: another app's series named by a label an agent may not select on
		slo(`"kube_secret_info"`, `{"service_name": "gate-api"}`, `{"code": "5"}`),
		slo(`"m"`, `{"k8s_namespace_name": "gate-api-prod"}`, `{"code": "5"}`),
		slo(`"m"`, `{"deployment": "gate-api"}`, `{"code": "5"}`),
		slo(`"m"`, `{"exported_pod": "gate-x"}`, `{"code": "5"}`),
		// the same in the error filter, which has no value of its own to name the app
		slo(`"m"`, good, `{"service_name": "gate-api"}`),
		slo(`"m"`, good, `{"exported_namespace": "app-other-prod"}`),
		slo(`"m"`, good, `{"Namespace": "x"}`),
		slo(`"{__name__=~\".+\"}"`, good, `{"code": "5"}`),
		slo(`"m"`, `"namespace=\"app-other-cicd\"} or {__name__=~\".+\""`, `{"code": "5"}`),
		slo(`"m"`, good, `"code=~\"5..\""`),
		slo(`"m"`, `{"__name__": "x"}`, `{"code": "5"}`),
		slo(`"m"`, `{"job": "flight-api\"} or {x=\"y"}`, `{"code": "5"}`),
		slo(`"m"`, `{"job": "flight-api}"}`, `{"code": "5"}`),
		slo(`"m"`, `{"job": "a\\b"}`, `{"code": "5"}`),
		slo(`"m"`, `{"job": "flight-api\nx"}`, `{"code": "5"}`),
		slo(`"m"`, `{"jo b": "flight-api"}`, `{"code": "5"}`),
		slo(`"m{x=\"y\"}"`, good, `{"code": "5"}`),
		slo(`"m"`, `{}`, `{"code": "5"}`),
	}
	for i, js := range bad {
		if b, err := build(t, "flight-api", "dev", "SLO", nil, js); err == nil {
			t.Errorf("case %d accepted:\n%s", i, b.Text)
		}
	}
	b, err := build(t, "flight-api", "dev", "SLO", nil, slo(`"http_requests_total"`, `{"service": "anything", "code": "200"}`, `{"code": "500"}`))
	if err != nil {
		t.Fatal(err)
	}
	ind := b.XR.M["spec"].(*definitions.OMap).M["indicator"].(*definitions.OMap)
	// a plain filter passes through unchanged: no namespace matcher is added (the composition adds the SLO's own)
	if ind.M["totalFilter"] != `code="200",service="anything"` || ind.M["errorFilter"] != `code="500"` || ind.M["metric"] != "http_requests_total" {
		t.Fatalf("the matchers the gateway wrote: %v", definitions.Plain(ind))
	}
	// a namespace or __name__ matcher, by any spelling, is refused at the gateway with the reason, even when the platform's label list allows it
	reg := xrReg
	reg.XR = XRPolicy{SLOLabels: []string{"job", "code", "namespace", "__name__", "Namespace", "k8s_namespace_name"}}
	for _, k := range []string{"namespace", "__name__", "Namespace", "k8s_namespace_name"} {
		_, err := BuildXR(XRRequest{App: "flight-api", Env: "dev", Kind: "SLO", Spec: spec(t, slo(`"m"`, `{"`+k+`": "x"}`, `{"code": "5"}`)), Namespaces: []string{"app-flight-api-dev"}}, reg)
		if err == nil || !strings.Contains(err.Error(), "namespace or __name__ matcher") {
			t.Errorf("%s: %v", k, err)
		}
	}
	// several namespaces: an SLO covers exactly its own, so one SLO per namespace, named; never narrowed to one or fanned out
	_, err = BuildXR(XRRequest{App: "flight-api", Env: "dev", Kind: "SLO", Spec: spec(t, slo(`"m"`, good, `{"code": "5"}`)), Namespaces: []string{"app-flight-api-dev", "infra-flight-api-dev"}}, xrReg)
	if err == nil || !strings.Contains(err.Error(), "one SLO per namespace") || !strings.Contains(err.Error(), "app-flight-api-dev, infra-flight-api-dev") {
		t.Fatalf("%v", err)
	}
	// with no namespace the platform assigned, there is nothing to select in
	if _, err := BuildXR(XRRequest{App: "flight-api", Env: "dev", Kind: "SLO", Spec: spec(t, slo(`"m"`, good, `{"code": "5"}`))}, xrReg); err == nil {
		t.Error("an SLO with no namespace was written")
	}
	// the labels are the platform's setting
	reg = xrReg
	reg.XR = XRPolicy{SLOLabels: []string{"job"}}
	if _, err := BuildXR(XRRequest{App: "flight-api", Env: "dev", Kind: "SLO", Spec: spec(t, slo(`"m"`, `{"code": "1"}`, `{"job": "x"}`)), Namespaces: []string{"app-flight-api-dev"}}, reg); err == nil {
		t.Error("a label outside XR_SLO_LABELS was written")
	}
}

// What deletes or empties data, picks a network, a channel, a region or a size is the platform's setting, not an agent's.
func TestWhatDeletesDataOrPicksANetworkIsThePlatformsSetting(t *testing.T) {
	db := func(js string) (*BuiltXR, error) {
		return build(t, "flight-api", "dev", "ManagedPostgreSQL", "flight-api-db", js)
	}
	for _, js := range []string{`{"protect": false}`, `{"size": "large"}`, `{"storageGiB": 128}`, `{"storageGiB": 256}`, `{"region": "eu-west-1"}`, `{"vpcCidr": "10.43.0.0/16"}`} {
		if b, err := db(js); err == nil {
			t.Errorf("%s accepted:\n%s", js, b.Text)
		}
	}
	if _, err := build(t, "flight-api", "dev", "ObjectStorage", "flight-api-b", `{"emptyOnDelete": true}`); err == nil {
		t.Error("emptyOnDelete accepted")
	}
	if _, err := build(t, "flight-api", "dev", "RolloutWatch", nil, `{"notifications": {"slack": {"channel": "#anywhere"}}}`); err == nil {
		t.Error("an arbitrary channel accepted")
	}
	if _, err := build(t, "flight-api", "dev", "MessageQueue", "flight-api-q", `{"consumers": [{"awsLambdaTarget": "flight-api-a"}, {"awsLambdaTarget": "flight-api-b"}, {"awsLambdaTarget": "flight-api-c"}, {"awsLambdaTarget": "flight-api-d"}]}`); err == nil {
		t.Error("four consumers accepted under a default of three")
	}
	// a database is deletion-protected unless the platform says otherwise, and the file says so
	b, err := db(`{"size": "small", "storageGiB": 64}`)
	if err != nil {
		t.Fatal(err)
	}
	if b.XR.M["spec"].(*definitions.OMap).M["protect"] != true {
		t.Fatalf("%s", b.Text)
	}
	// the platform can allow each, up to the XRD's own maximum
	reg := xrReg
	reg.XR = XRPolicy{Regions: []string{"eu-west-1"}, DBSizes: []string{"large"}, MaxStorageGiB: 9999, AllowUnprotectedDB: true, AllowEmptyOnDelete: true}
	for _, c := range []struct{ kind, js string }{{"ManagedPostgreSQL", `{"protect": false, "size": "large", "storageGiB": 256, "region": "eu-west-1"}`}, {"ObjectStorage", `{"emptyOnDelete": true}`}} {
		if _, err := BuildXR(XRRequest{App: "flight-api", Env: "dev", Kind: c.kind, Name: "flight-api-x", Spec: spec(t, c.js)}, reg); err != nil {
			t.Errorf("%s: %v", c.kind, err)
		}
	}
	if _, err := BuildXR(XRRequest{App: "flight-api", Env: "dev", Kind: "ManagedPostgreSQL", Name: "flight-api-x", Spec: spec(t, `{"storageGiB": 512}`)}, reg); err == nil {
		t.Error("512 GiB is above the XRD's own largest")
	}
}

// A name the cloud holds for everyone belongs to the onboarded app with the longest name that is its prefix.
func TestAGlobalNameBelongsToTheAppWithTheLongestNameThatStartsIt(t *testing.T) {
	check := func(app, kind, js string, onboarded ...string) error {
		b, err := BuildXR(XRRequest{App: app, Env: "dev", Kind: kind, Name: app + "-x", Spec: spec(t, js)}, xrReg)
		if err != nil {
			t.Fatalf("%s %s: %v", app, js, err)
		}
		return b.CheckOwners(onboarded)
	}
	apps := []string{"gate", "gate-api", "a-b", "ab", "flight-api"}
	for _, c := range []struct {
		app, kind, js string
		ok            bool
	}{
		{"gate", "ObjectStorage", `{"bucketName": "gate-api-prod-assets"}`, false},
		{"gate", "ObjectStorage", `{"bucketName": "gate-assets"}`, true},
		{"gate-api", "ObjectStorage", `{"bucketName": "gate-api-prod-assets", "emptyOnDelete": false}`, true},
		{"gate", "MessageQueue", `{"queueName": "gate-api-jobs"}`, false},
		{"gate", "ManagedPostgreSQL", `{"serverName": "gate-api-db"}`, false},
		{"gate", "MessageQueue", `{"resourceGroup": "gate-api-rg"}`, false},
		{"gate", "MessageQueue", `{"namespaceName": "gate-api-bus"}`, false},
		{"gate", "ObjectStorage", `{"storageAccountName": "gateapistore1"}`, false},
		{"gate", "ObjectStorage", `{"storageAccountName": "gatestore1"}`, false}, // gate's and gate-api's flat names are related: neither may
		{"flight-api", "ObjectStorage", `{"storageAccountName": "flightapistore"}`, true},
		{"a-b", "ObjectStorage", `{"storageAccountName": "abstore"}`, false}, // a-b and ab flatten to the same
		{"ab", "ObjectStorage", `{"storageAccountName": "abstore"}`, false},
	} {
		if err := check(c.app, c.kind, c.js, apps...); (err == nil) != c.ok {
			t.Errorf("%s %s: %v", c.app, c.js, err)
		}
	}
	// an app that is the only one of its name tree owns what it starts
	if err := check("solo", "ObjectStorage", `{"bucketName": "solo-data"}`, "solo", "gate"); err != nil {
		t.Error(err)
	}
}

func TestAnSLOObjectiveOfZeroIsRefusedBeforeThePullRequest(t *testing.T) {
	slo := func(o string) string {
		return `{"environmentRef": {"name": "flight-api-dev-cluster-dev"}, "service": "flight-api-s", "objective": ` + o + `, "indicator": {"type": "availability", "metric": "m", "totalFilter": {"job": "flight-api"}, "errorFilter": {"code": "5"}}}`
	}
	if _, err := build(t, "flight-api", "dev", "SLO", nil, slo("0")); err == nil {
		t.Fatal("objective 0 accepted: the XRD's minimum is exclusive")
	}
	for _, o := range []string{"0.0001", "99.9", "100"} {
		if _, err := build(t, "flight-api", "dev", "SLO", nil, slo(o)); err != nil {
			t.Errorf("%s: %v", o, err)
		}
	}
}

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
	return BuildXR(XRRequest{App: app, Env: env, Kind: kind, Name: name, Spec: spec(t, js)}, xrReg)
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
		{"a slo for another app's environment", "SLO", `{"environmentRef": {"name": "payments-dev-cluster-dev"}, "service": "flight-api-x", "objective": 99, "indicator": {"type": "availability", "metric": "m", "totalFilter": "a", "errorFilter": "b"}}`},
		{"a rollout watch for another app", "RolloutWatch", `{"environmentRef": {"name": "flight-api-dev-cluster-dev"}, "appName": "payments"}`},
		{"a rollout watch on another cluster", "RolloutWatch", `{"environmentRef": {"name": "flight-api-dev-cluster-dev"}, "cluster": "up-cluster"}`},
		{"a number as a string", "MessageQueue", `{"lockSeconds": "60"}`},
		{"a float for an integer", "MessageQueue", `{"lockSeconds": 60.5}`},
		{"out of range", "MessageQueue", `{"lockSeconds": 999999}`},
		{"an enum that is not one", "ManagedPostgreSQL", `{"size": "huge"}`},
		{"a string with a control character", "SLO", `{"environmentRef": {"name": "flight-api-dev-cluster-dev"}, "service": "flight-api-s", "objective": 99, "indicator": {"type": "availability", "metric": "m\nx", "totalFilter": "a", "errorFilter": "b"}}`},
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
			"indicator": map[string]any{"type": "availability", "metric": h, "totalFilter": "a", "errorFilter": "b"}})
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
		"SLO":                    `{"service": "flight-api-svc", "objective": 99.9, "indicator": {"type": "latency", "metric": "http_request_duration_seconds", "totalFilter": "{job=\"flight-api\"}", "errorFilter": "{code=~\"5..\"}", "latencyThreshold": "0.5"}}`,
		"ApplicationEnvironment": `{"appType": "app"}`,
	} {
		b, err := build(t, "flight-api", "dev", kind, nil, js)
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

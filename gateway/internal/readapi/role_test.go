package readapi

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The api refuses to start on a role that could write: a session default of read-only is not a defence when the role may be asked
// to switch it off. It must not be a superuser (or hold pg_write_all_data), own or write any gateway table, or create in the schema or
// the database.
func TestTheAPIRefusesARoleThatCouldWrite(t *testing.T) {
	dsn, st := scratch(t)
	ctx := context.Background()
	u, _ := url.Parse(dsn)
	dbName := strings.TrimPrefix(u.Path, "/")
	suffix := fmt.Sprint(time.Now().UnixNano())
	mk := func(name string, grants ...string) string {
		role := name + suffix
		for _, q := range append([]string{
			"CREATE ROLE " + role + " LOGIN PASSWORD 'pw'",
			"GRANT CONNECT ON DATABASE " + dbName + " TO " + role,
			"GRANT USAGE ON SCHEMA public TO " + role,
			"GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + role,
		}, grants...) {
			if _, err := st.Pool.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		t.Cleanup(func() {
			_, _ = st.Pool.Exec(ctx, "DROP OWNED BY "+role)
			_, _ = st.Pool.Exec(ctx, "DROP ROLE IF EXISTS "+role)
		})
		c := *u
		c.User = url.UserPassword(role, "pw")
		return c.String()
	}
	check := func(dsn string) error {
		s, err := OpenPg(ctx, dsn, 2)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		return s.CheckRole(ctx)
	}
	if err := check(mk("ro")); err != nil {
		t.Fatalf("a read-only role was refused: %v", err)
	}
	for _, c := range []struct {
		name   string
		dsn    string
		expect string
	}{
		{"the writer's own role (a superuser here)", dsn, "superuser"},
		{"insert on a gateway table", mk("ins", "GRANT INSERT ON gateway_holds TO ins"+suffix), "gateway_holds"},
		{"update on a gateway table", mk("upd", "GRANT UPDATE ON gateway_sessions TO upd"+suffix), "gateway_sessions"},
		{"delete on a gateway table", mk("del", "GRANT DELETE ON gateway_events TO del"+suffix), "gateway_events"},
		{"truncate on a gateway table", mk("trn", "GRANT TRUNCATE ON gateway_results TO trn"+suffix), "gateway_results"},
		{"create in the schema", mk("sch", "GRANT CREATE ON SCHEMA public TO sch"+suffix), "create"},
		{"create in the database", mk("dbc", "GRANT CREATE ON DATABASE "+dbName+" TO dbc"+suffix), "create"},
		{"pg_write_all_data", mk("wad", "GRANT pg_write_all_data TO wad"+suffix), "pg_write_all_data"},
		{"membership of a role that can write", mk("mem", "CREATE ROLE grp"+suffix+" NOLOGIN", "GRANT INSERT ON gateway_holds TO grp"+suffix, "GRANT grp"+suffix+" TO mem"+suffix), "gateway_holds"},
	} {
		err := check(c.dsn)
		if err == nil || !strings.Contains(err.Error(), c.expect) || !strings.Contains(err.Error(), "read-only role") {
			t.Errorf("%s: %v, want a refusal naming %q", c.name, err, c.expect)
		}
	}
	// A role that owns a gateway table (the writer's usual role): refused whether or not it is a superuser.
	owner := mk("own")
	if _, err := st.Pool.Exec(ctx, "CREATE ROLE ownr"+suffix+" LOGIN PASSWORD 'pw'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool.Exec(ctx, "DROP OWNED BY ownr"+suffix)
		_, _ = st.Pool.Exec(ctx, "DROP ROLE IF EXISTS ownr"+suffix)
	})
	for _, q := range []string{"GRANT CONNECT ON DATABASE " + dbName + " TO ownr" + suffix, "GRANT USAGE ON SCHEMA public TO ownr" + suffix, "ALTER TABLE gateway_holds OWNER TO ownr" + suffix} {
		if _, err := st.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = owner
	c := *u
	c.User = url.UserPassword("ownr"+suffix, "pw")
	if err := check(c.String()); err == nil || !strings.Contains(err.Error(), "gateway_holds") {
		t.Fatalf("an owner of a gateway table: %v", err)
	}
}

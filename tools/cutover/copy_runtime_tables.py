"""R-4 cutover: copy the gateway's runtime tables from clearance-pg into the gateway's own database (GATEWAY_DSN).

Used once, at the hangar-dev cutover on 2026-11-03 (24 sessions, 2 human requests, 145 events, 2 results and 1
definition copied). Kept as the record of how the runtime state moved; nothing runs it now, and the read API no longer
reads the clearance_* tables it copied from (R-4).

    python tools/cutover/copy_runtime_tables.py --from "$CLEARANCE_DSN" --to "$GATEWAY_DSN" [--dry-run]

Sessions, the human inbox, run results, artifacts, trigger history, holds and the cluster-pinned definition set are
runtime state, not evidence (Hangar spec R-3): until R-3b the Python gateway kept them in clearance-pg beside the
evidence chain, as clearance_<name>; the gateway now keeps them in its own database as gateway_<name>, with the same
columns (src/autopilot/engine/pg.py SCHEMA). This copies each table across, row for row, column for column.

Nothing is deleted or changed in clearance-pg: the evidence (clearance_evidence), the gate's changes
(clearance_changes) and the core's own copy of the definitions (clearance_definitions) stay where they are, and the
copied runtime tables stay too until someone drops them after the cutover is verified.

Order at cutover (docs/admin/gateway-database.md):
  1. scale the old gateway (the Python one with CLEARANCE_DSN) to 0, so nothing writes the runtime tables;
  2. run this with --dry-run, then without;
  3. start the new gateway (GATEWAY_DSN, CLEARANCE_CORE_URL).

Safety: the source is read in one REPEATABLE READ, READ ONLY transaction (one consistent snapshot); the target is
written in one transaction that holds the gateway's writer lock (so a running gateway refuses to start, and this
refuses to run beside one), creates the tables if they are missing, refuses a target table that already has rows
(unless --replace), and checks every table's row count before it commits. Prints counts only, never row contents.
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

import psycopg

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "src"))
from autopilot.engine.pg import LOCK_KEY, SCHEMA  # noqa: E402

# source table -> target table, and the columns copied (every column both have; written_at/updated_at included).
TABLES = {
    "clearance_sessions": ("gateway_sessions", ("id", "record", "expires_at", "closed", "updated_at")),
    "clearance_artifacts": ("gateway_artifacts", ("task_id", "name", "content", "bytes", "expires_at", "written_at")),
    "clearance_human_requests": ("gateway_human_requests", ("id", "session", "status", "expires_at", "record", "updated_at")),
    "clearance_events": ("gateway_events", ("id", "received_at", "source", "app", "routing_key", "record", "written_at")),
    "clearance_holds": ("gateway_holds", ("agent", "until", "record", "updated_at")),
    "clearance_results": ("gateway_results", ("session", "task_id", "record", "expires_at", "written_at")),
    "clearance_definitions": ("gateway_definitions", ("name", "state", "record", "updated_at")),
}


class CopyError(RuntimeError):
    pass


def _exists(cur, table: str) -> bool:
    cur.execute("SELECT to_regclass(%s) IS NOT NULL", (table,))
    return bool(cur.fetchone()[0])


def copy(src_dsn: str, dst_dsn: str, *, dry_run: bool = False, replace: bool = False, out=print) -> dict[str, int]:
    """Copy every runtime table; returns {target table: rows copied}. Raises CopyError and copies nothing on any
    problem."""
    counts: dict[str, int] = {}
    with psycopg.connect(src_dsn) as src, psycopg.connect(dst_dsn) as dst:
        src.execute("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY")
        with dst.cursor() as d:
            d.execute("SELECT pg_try_advisory_xact_lock(%s)", (LOCK_KEY,))
            if not d.fetchone()[0]:
                raise CopyError("a gateway holds the target database's writer lock: stop it first")
            d.execute(SCHEMA)
            for s_table, (t_table, cols) in TABLES.items():
                with src.cursor() as s:
                    if not _exists(s, s_table):
                        out(f"{s_table}: not in the source, skipped")
                        counts[t_table] = 0
                        continue
                    s.execute(f"SELECT count(*) FROM {s_table}")  # noqa: S608 - names are this module's constants
                    n = s.fetchone()[0]
                d.execute(f"SELECT count(*) FROM {t_table}")  # noqa: S608
                have = d.fetchone()[0]
                if have and not replace:
                    raise CopyError(f"{t_table} already has {have} rows (pass --replace to overwrite them)")
                if have:
                    d.execute(f"DELETE FROM {t_table}")  # noqa: S608
                collist = ", ".join(cols)
                with src.cursor() as s, s.copy(f"COPY (SELECT {collist} FROM {s_table}) TO STDOUT (FORMAT binary)") as cin, \
                        d.copy(f"COPY {t_table} ({collist}) FROM STDIN (FORMAT binary)") as cout:
                    for chunk in cin:
                        cout.write(chunk)
                d.execute(f"SELECT count(*) FROM {t_table}")  # noqa: S608
                got = d.fetchone()[0]
                if got != n:
                    raise CopyError(f"{t_table}: copied {got} rows of {n}")
                counts[t_table] = n
                out(f"{s_table} -> {t_table}: {n} rows")
            if _exists(d, "gateway_outbox"):
                d.execute("SELECT count(*) FROM gateway_outbox")
                if d.fetchone()[0] and not replace:
                    raise CopyError("gateway_outbox is not empty: this database has served a gateway already")
        if dry_run:
            dst.rollback()
            out("dry run: rolled back, nothing written")
        else:
            dst.commit()
            out("committed")
        src.rollback()
    return counts


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--from", dest="src", required=True, help="clearance-pg's DSN (read only)")
    p.add_argument("--to", dest="dst", required=True, help="the gateway database's DSN (GATEWAY_DSN)")
    p.add_argument("--dry-run", action="store_true", help="copy and check, then roll back")
    p.add_argument("--replace", action="store_true", help="overwrite target tables that already have rows")
    a = p.parse_args(argv)
    try:
        copy(a.src, a.dst, dry_run=a.dry_run, replace=a.replace)
    except (CopyError, psycopg.Error) as e:
        print(f"copy failed, nothing written: {type(e).__name__}: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

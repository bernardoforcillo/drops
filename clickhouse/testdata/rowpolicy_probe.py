#!/usr/bin/env python3
"""Re-verify what rowpolicy.go claims, against a real ClickHouse.

Nothing in the Go test suite runs this. It exists so the next person to
touch clickhouse/rowpolicy.go can re-check the facts its doc comments
rest on instead of trusting a comment, and so a claim that turns out to
be version-dependent is caught by running it against a newer engine
rather than by it biting somebody.

No ClickHouse server was reachable from the environment this was
written in: packages.clickhouse.com and the Docker blob CDN are both
blocked at the proxy. What IS reachable is chdb on PyPI, which embeds a
real ClickHouse engine in-process — chdb 4.3.0 carries ClickHouse
26.7.2.1 — driven in clickhouse-local mode.

    pip3 install chdb
    python3 rowpolicy_probe.py                # both checks
    go run ./... | python3 rowpolicy_probe.py --parse   # parse stdin

Two things it checks, and one it cannot.

1. PARSE. Every statement drops emits is fed to EXPLAIN AST, which
   parses and stops. A pass proves the grammar accepts the string and
   nothing more. This is what pins the clause order: the PostgreSQL
   order drops/pg emits (TO before USING) is a syntax error here, and
   FOR INSERT is a syntax error too — the row policy grammar admits
   only ALL and SELECT.

2. ENFORCE. Whether a row policy filters reads and leaves writes
   alone. It has to go through the users.xml <filter> form of the same
   row-policy machinery, because clickhouse-local exposes no writeable
   access storage and answers a SQL CREATE ROW POLICY with code 514,
   ACCESS_STORAGE_FOR_INSERTION_NOT_FOUND. The filter reaches the same
   enforcement path — it shows up in system.row_policies like any
   other — so what it settles is the semantics, not the DDL.

What it cannot check, and what therefore rests on ClickHouse's own
documentation wherever rowpolicy.go cites it: the two
access_control_improvements defaults (clickhouse-local has one user, so
there is no unmatched principal to observe), custom-settings-based
predicates (clickhouse-local ignores custom_settings_prefixes from a
config file), how permissive and restrictive policies combine, and
anything distributed.
"""

import os
import shutil
import sys
import tempfile

TENANT_FILTER_USERS_XML = """<clickhouse>
  <profiles><default></default></profiles>
  <users>
    <default>
      <password></password>
      <networks><ip>::/0</ip></networks>
      <profile>default</profile>
      <quota>default</quota>
      <!-- only so the probe may read system.row_policies back -->
      <access_management>1</access_management>
      <databases>
        <t><docs><filter>tenant = 'acme'</filter></docs></t>
      </databases>
    </default>
  </users>
  <quotas><default></default></quotas>
</clickhouse>
"""

CONFIG_XML = """<clickhouse>
  <user_directories><users_xml><path>{users}</path></users_xml></user_directories>
</clickhouse>
"""

# The shapes rowpolicy.go emits, plus the two it must never emit.
PARSE_CASES = [
    (True, """CREATE ROW POLICY "p" ON "db"."docs" FOR SELECT USING ("t" = 'acme') TO "app" """),
    (True, """CREATE ROW POLICY "p" ON "db"."docs" FOR SELECT USING (1) AS RESTRICTIVE TO "a", "b" """),
    (True, """CREATE ROW POLICY "p" ON "db"."docs" FOR SELECT USING (1) TO ALL"""),
    (True, """CREATE ROW POLICY "p" ON "db"."docs" FOR SELECT USING (1) TO ALL EXCEPT "admin" """),
    (True, """CREATE ROW POLICY IF NOT EXISTS "p" ON "db"."docs" FOR SELECT USING (1) TO ALL"""),
    (True, """CREATE ROW POLICY OR REPLACE "p" ON "db"."docs" FOR SELECT USING (1) TO ALL"""),
    (True, """CREATE ROW POLICY "p" ON CLUSTER "c" ON "db"."docs" FOR SELECT USING (1) TO ALL"""),
    (True, """CREATE ROW POLICY "p" ON "db".* FOR SELECT USING (1) TO ALL"""),
    (True, """DROP ROW POLICY IF EXISTS "p" ON "db"."docs" ON CLUSTER "c" """),
    (True, """DROP ROW POLICY "p" ON "db".* """),
    # The PostgreSQL clause order drops/pg emits.
    (False, """CREATE ROW POLICY "p" ON "db"."docs" AS RESTRICTIVE FOR SELECT TO "a" USING 1"""),
    # The write half PostgreSQL has and ClickHouse does not.
    (False, """CREATE ROW POLICY "p" ON "db"."docs" FOR INSERT USING 1 TO ALL"""),
]


def parses(sess, sql):
    try:
        sess.query("EXPLAIN AST " + sql, "CSV")
        return True, ""
    except Exception as exc:  # noqa: BLE001 — the message is the result
        return False, str(exc).strip().split("\n")[0]


def check_parse(statements=None):
    from chdb import session

    path = tempfile.mkdtemp(prefix="chprobe-parse-")
    sess = session.Session(path)
    print("engine:", sess.query("SELECT version()", "CSV").__str__().strip())
    failures = 0
    cases = [(True, s) for s in statements] if statements else PARSE_CASES
    for want_ok, sql in cases:
        got_ok, err = parses(sess, sql)
        mark = "ok  " if got_ok == want_ok else "FAIL"
        if got_ok != want_ok:
            failures += 1
        print(f"  {mark} parses={got_ok} want={want_ok}  {sql.strip()[:88]}")
        if err and not want_ok:
            print(f"       {err[:150]}")
    sess.close()
    shutil.rmtree(path, ignore_errors=True)
    return failures


def check_enforce():
    """A row policy filters SELECT. It does not constrain INSERT."""
    from chdb import session

    cfg = tempfile.mkdtemp(prefix="chprobe-cfg-")
    users = os.path.join(cfg, "users.xml")
    config = os.path.join(cfg, "config.xml")
    with open(users, "w") as fh:
        fh.write(TENANT_FILTER_USERS_XML)
    with open(config, "w") as fh:
        fh.write(CONFIG_XML.format(users=users))

    path = tempfile.mkdtemp(prefix="chprobe-enf-")
    sess = session.Session(f"{path}?config-file={config}")

    def q(sql):
        return str(sess.query(sql, "CSV")).strip()

    failures = 0
    q("CREATE DATABASE IF NOT EXISTS t")
    q("CREATE TABLE t.docs (id UInt32, tenant String, body String) "
      "ENGINE=MergeTree ORDER BY id")
    q("INSERT INTO t.docs VALUES (1,'acme','a1'),(2,'acme','a2'),(3,'globex','g1')")

    print("  policy in system.row_policies:", q(
        "SELECT short_name, database, table, select_filter FROM system.row_policies"))

    visible = q("SELECT count() FROM t.docs")
    if visible != "2":
        failures += 1
    print(f"  {'ok  ' if visible == '2' else 'FAIL'} SELECT sees {visible} of 3 rows")

    # The write half. Under a policy of tenant = 'acme', writing another
    # tenant's row is not refused: there is no WITH CHECK to refuse it.
    try:
        q("INSERT INTO t.docs VALUES (4,'globex','g2')")
        wrote = True
        err = ""
    except Exception as exc:  # noqa: BLE001
        wrote = False
        err = str(exc).strip().split("\n")[0]
    if not wrote:
        failures += 1
    print(f"  {'ok  ' if wrote else 'FAIL'} INSERT of another tenant's row succeeded={wrote} {err[:120]}")

    physical = q("SELECT sum(rows) FROM system.parts "
                 "WHERE database='t' AND table='docs' AND active")
    still = q("SELECT count() FROM t.docs")
    agree = physical == "4" and still == "2"
    if not agree:
        failures += 1
    print(f"  {'ok  ' if agree else 'FAIL'} {physical} rows on disk, {still} visible "
          "— the write landed and the read boundary hid it")

    sess.close()
    shutil.rmtree(path, ignore_errors=True)
    shutil.rmtree(cfg, ignore_errors=True)
    return failures


def main():
    if "--parse" in sys.argv and not sys.stdin.isatty():
        stmts = [line.strip() for line in sys.stdin if line.strip()]
        sys.exit(1 if check_parse(stmts) else 0)
    print("PARSE")
    failures = check_parse()
    print("ENFORCE")
    failures += check_enforce()
    print("failures:", failures)
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()

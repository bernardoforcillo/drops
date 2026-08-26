#!/usr/bin/env python3
"""Re-verify what rowpolicy.go claims, against a real ClickHouse engine.

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
    python3 rowpolicy_probe.py                # every check
    go run ./... | python3 rowpolicy_probe.py --parse   # parse stdin

Three things it checks, and the ones it cannot.

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

3. FAILOPEN. What a principal NO policy applies to reads. users.xml
   takes as many users as it is given, so the unmatched principal this
   was once thought unobservable on clickhouse-local is observable
   after all: declare a filter for one account and query as the other.
   The engine synthesises a policy carrying the filter 1 for the
   unmatched account and it reads the whole table. That is the sharpest
   edge in the mechanism and it is measured here, not quoted.

   The REMEDY is the half that stays unmeasured. Setting
   users_without_row_policies_can_read_rows to false changes nothing on
   this engine, under <access_control_improvements> or at the config
   root, in the server config or the users config — all four are tried
   below and all four are expected to make no difference, which is why
   the check reports rather than fails. clickhouse-local ignores the
   section, of a piece with it ignoring every user_directories entry
   (system.user_directories lists users_xml and nothing else however
   the config is written, which is also why a SQL CREATE ROW POLICY
   cannot be stored here at all).

What it cannot check, and what therefore rests on ClickHouse's own
documentation wherever rowpolicy.go cites it: whether turning either
access_control_improvements setting off does what it says,
custom-settings-based predicates (clickhouse-local ignores
custom_settings_prefixes from a config file), how permissive and
restrictive policies combine, what a policy with no TO clause applies
to, and anything distributed.
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

# Two accounts, a filter on one of them. `default` is the account chdb
# connects as and the one no policy names.
FAILOPEN_USERS_XML = """<clickhouse>{extra}
  <profiles><default></default></profiles>
  <users>
    <default>
      <password></password>
      <networks><ip>::/0</ip></networks>
      <profile>default</profile>
      <quota>default</quota>
      <access_management>1</access_management>
    </default>
    <tenantreader>
      <password></password>
      <networks><ip>::/0</ip></networks>
      <profile>default</profile>
      <quota>default</quota>
      <databases>
        <t><docs><filter>tenant = 'acme'</filter></docs></t>
      </databases>
    </tenantreader>
  </users>
  <quotas><default></default></quotas>
</clickhouse>
"""

FAILOPEN_CONFIG_XML = """<clickhouse>{extra}
  <user_directories><users_xml><path>{users}</path></users_xml></user_directories>
</clickhouse>
"""

# The setting that is supposed to close the hole, written the four ways
# it could plausibly be read: nested and bare, in each of the two files.
ACI_OFF = ("\n  <access_control_improvements>"
           "\n    <users_without_row_policies_can_read_rows>false"
           "</users_without_row_policies_can_read_rows>"
           "\n  </access_control_improvements>")
BARE_OFF = ("\n  <users_without_row_policies_can_read_rows>false"
            "</users_without_row_policies_can_read_rows>")

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


def failopen_once(users_extra, config_extra, label):
    """Read t.docs as the account no policy names. Returns (rows, filters)."""
    from chdb import session

    cfg = tempfile.mkdtemp(prefix="chprobe-fo-cfg-")
    users = os.path.join(cfg, "users.xml")
    config = os.path.join(cfg, "config.xml")
    with open(users, "w") as fh:
        fh.write(FAILOPEN_USERS_XML.format(extra=users_extra))
    with open(config, "w") as fh:
        fh.write(FAILOPEN_CONFIG_XML.format(extra=config_extra, users=users))

    path = tempfile.mkdtemp(prefix="chprobe-fo-")
    sess = session.Session(f"{path}?config-file={config}")

    def q(sql):
        return str(sess.query(sql, "CSV")).strip()

    q("CREATE DATABASE IF NOT EXISTS t")
    q("CREATE TABLE t.docs (id UInt32, tenant String, body String) "
      "ENGINE=MergeTree ORDER BY id")
    q("INSERT INTO t.docs VALUES (1,'acme','a1'),(2,'acme','a2'),(3,'globex','g1')")
    filters = q("SELECT name, select_filter FROM system.row_policies ORDER BY name")
    rows = q("SELECT count() FROM t.docs")
    dirs = q("SELECT name FROM system.user_directories")
    sess.close()
    shutil.rmtree(path, ignore_errors=True)
    shutil.rmtree(cfg, ignore_errors=True)
    print(f"  {label}: reads {rows} of 3 rows; user_directories={dirs}")
    print(f"       policies: {filters}")
    return rows, filters


def check_failopen():
    """A principal no policy applies to reads every row, silently."""
    failures = 0
    rows, filters = failopen_once("", "", "default (nothing set)")
    if rows != "3":
        failures += 1
        print("  FAIL the unmatched account did NOT read every row — rowpolicy.go's "
              "fail-open paragraph needs rewriting against this engine")
    elif "1" not in filters:
        failures += 1
        print("  FAIL every row was read but no synthesised filter-1 policy is "
              "visible; the mechanism is not what rowpolicy.go describes")
    else:
        print("  ok   the unmatched account read every row, through a policy the "
              "server wrote for it")

    # The remedy, four ways. None of these is expected to change the
    # answer on clickhouse-local; a run where one DOES is a finding, and
    # rowpolicy.go should stop calling the fix unmeasured.
    for label, users_extra, config_extra in [
        ("config <access_control_improvements>", "", ACI_OFF),
        ("config root", "", BARE_OFF),
        ("users <access_control_improvements>", ACI_OFF, ""),
        ("users root", BARE_OFF, ""),
    ]:
        rows, _ = failopen_once(users_extra, config_extra, f"off via {label}")
        if rows != "3":
            print(f"  NOTE  {label} CLOSED the hole on this engine. rowpolicy.go "
                  "records the fix as documentation only; make it a measurement.")
    return failures


def main():
    if "--parse" in sys.argv and not sys.stdin.isatty():
        stmts = [line.strip() for line in sys.stdin if line.strip()]
        sys.exit(1 if check_parse(stmts) else 0)
    print("PARSE")
    failures = check_parse()
    print("ENFORCE")
    failures += check_enforce()
    print("FAILOPEN")
    failures += check_failopen()
    print("failures:", failures)
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()

"""Run the same R1 constraint checks on a disposable SQLite file or PostgreSQL cluster."""
import argparse
from contextlib import closing, contextmanager
import os
from pathlib import Path
import re
import sqlite3
import subprocess
from tempfile import TemporaryDirectory

ROOT = Path(__file__).resolve().parent


@contextmanager
def database(backend, directory):
    if backend == "sqlite":
        def sql(script, *, reject=False, query=False):
            with closing(sqlite3.connect(directory / "engine.db")) as conn:
                conn.execute("PRAGMA foreign_keys = ON")
                conn.execute("PRAGMA recursive_triggers = ON")
                try:
                    if query:
                        return str(conn.execute(script).fetchone()[0])
                    conn.executescript(script)
                except sqlite3.IntegrityError:
                    if reject:
                        return
                    raise
                if reject:
                    raise AssertionError(f"SQLite accepted: {script}")
        yield sql, str(directory / "engine.db")
        return

    bindir = Path(subprocess.check_output(["pg_config", "--bindir"], text=True).strip())
    data = directory / "data"
    subprocess.run([bindir / "initdb", "-D", data, "-U", "tester", "--no-locale",
                    "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject"],
                   check=True, stdout=subprocess.DEVNULL)
    ctl = [bindir / "pg_ctl", "-D", data, "-w"]
    subprocess.run([*ctl, "-l", directory / "server.log", "-o",
                    f"-c listen_addresses='' -k '{directory}' -p 5432", "start"], check=True)
    try:
        def sql(script, *, reject=False, query=False):
            result = subprocess.run(
                [bindir / "psql", "-XqAt", "-h", directory, "-p", "5432", "-U", "tester",
                 "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-v", "VERBOSITY=verbose"],
                input=script, text=True, capture_output=True)
            if reject and result.returncode and re.search(r"ERROR:\s+23\w{3}:", result.stderr):
                return
            if result.returncode:
                raise RuntimeError(result.stderr)
            if reject:
                raise AssertionError(f"PostgreSQL accepted: {script}")
            if query:
                return result.stdout.strip()
        yield sql, f"host={directory} port=5432 user=tester dbname=postgres sslmode=disable"
    finally:
        subprocess.run([*ctl, "-m", "fast", "stop"], check=True)


def check(backend, sql, connection):
    def migrate():
        subprocess.run(["go", "run", "./cmd/workflow-migrate", "-driver", backend],
                       cwd=ROOT.parent, check=True,
                       env={**os.environ, "WORKFLOW_DATABASE_CONNECTION": connection})

    migrate()
    assert sql("SELECT count(*) FROM schema_migrations", query=True) == "4"

    sql("""INSERT INTO definitions VALUES ('p',1,'r1','[]','{}',0);
           INSERT INTO definition_heads VALUES ('p',1);
           INSERT INTO instances(instance_id,process_key,version,variables,created_at)
               VALUES ('a','p',1,'{}',0), ('b','p',1,'{}',0);
           INSERT INTO transitions VALUES ('a',1,'STARTED',NULL,0);
           INSERT INTO operations(operation_id,idempotency_key,kind,target,payload)
               VALUES ('op','k','Start','{}','{"processKey":"p"}');
           INSERT INTO work_items(work_id,kind,operation_id,ready_at)
               VALUES ('cmd','COMMAND','op',0);""")
    sql("UPDATE definition_heads SET version=2 WHERE process_key='p'", reject=True)
    sql("UPDATE definitions SET plan='{}' WHERE process_key='p'", reject=True)
    sql("UPDATE instances SET variables='[]' WHERE instance_id='a'", reject=True)
    sql("INSERT INTO transitions VALUES ('a',1,'STEP','node',1)", reject=True)
    sql("""INSERT INTO operations(operation_id,idempotency_key,kind,target,payload)
           VALUES ('op2','k','Deploy','{}','{}')""", reject=True)
    sql("""INSERT INTO work_items(work_id,kind,operation_id,ready_at)
           VALUES ('cmd2','COMMAND','op',0)""", reject=True)
    sql("UPDATE operations SET status='SUCCEEDED' WHERE operation_id='op'", reject=True)
    sql("UPDATE operations SET status='FAILED',error='{}' WHERE operation_id='op'")
    sql("UPDATE operations SET status='PENDING',error=NULL WHERE operation_id='op'", reject=True)
    sql("UPDATE work_items SET state='LEASED' WHERE work_id='cmd'", reject=True)

    # The failed successor insert must also roll back the tentative instance write.
    sql("""BEGIN;
           UPDATE instances SET revision=2 WHERE instance_id='a';
           INSERT INTO work_items(work_id,kind,operation_id,ready_at) VALUES ('dup','COMMAND','op',0);
           COMMIT;""", reject=True)
    assert sql("SELECT revision FROM instances WHERE instance_id='a'", query=True) == "1"

    sql("""INSERT INTO activations(activation_id,instance_id,token_id,element_id,state)
               VALUES ('s','a','parent','split','CONSUMED'), ('u','a','left','form','WAITING'),
                      ('j','a','right','join','WAITING'), ('x','b','right','join','WAITING');
           INSERT INTO user_tasks(task_id,activation_id,form_id,components,created_at)
               VALUES ('t','u','form1','[]',0);
           INSERT INTO boundary_timers(timer_id,activation_id,element_id,due_at)
               VALUES ('timer','u','boundary',60000);
           INSERT INTO work_items(work_id,kind,instance_id,activation_id,ready_at)
               VALUES ('step','STEP','a','u',0), ('alarm','TIMER','a','u',60000);
           INSERT INTO forks(fork_id,instance_id,split_activation_id,join_element_id)
               VALUES ('f','a','s','join');
           INSERT INTO branch_activations VALUES ('u','a','f','left'), ('j','a','f','right');
           INSERT INTO join_arrivals VALUES ('f','incoming-right','right','j');""")
    sql("""INSERT INTO user_tasks(task_id,activation_id,form_id,components,created_at)
           VALUES ('t2','u','form2','[]',0)""", reject=True)
    sql("""INSERT INTO boundary_timers(timer_id,activation_id,element_id,due_at)
           VALUES ('timer2','j','boundary',60000)""", reject=True)
    sql("""INSERT INTO work_items(work_id,kind,instance_id,activation_id,ready_at)
           VALUES ('orphan-alarm','TIMER','a','j',60000)""", reject=True)
    sql("UPDATE user_tasks SET status='CANCELLED',closed_at=60000 WHERE task_id='t'", reject=True)
    sql("INSERT INTO branch_activations VALUES ('x','b','f','right')", reject=True)
    sql("INSERT INTO join_arrivals VALUES ('f','incoming-right','left','u')", reject=True)
    sql("INSERT INTO join_arrivals VALUES ('f','incoming-left','left','j')", reject=True)
    sql("""INSERT INTO work_items(work_id,kind,instance_id,activation_id,ready_at)
           VALUES ('wrong','STEP','a','x',0)""", reject=True)
    sql("""INSERT INTO transitions VALUES ('a',2,'INCIDENT_RAISED','form',2);
           INSERT INTO incidents(instance_id,revision,work_id,element_id,code,message,retry_input)
               VALUES ('a',2,'step','form','EVALUATION_ERROR','bad input','{}');""")
    migrate()
    assert sql("SELECT status FROM operations WHERE operation_id='op'", query=True) == "FAILED"


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("backend", choices=["sqlite", "postgres"])
    backend = parser.parse_args().backend
    with TemporaryDirectory(prefix="workflow-schema-") as temp:
        with database(backend, Path(temp)) as (execute, connection):
            check(backend, execute, connection)
    print(f"{backend}: R1 schema checks passed")

"""Run the native R1 e2e matrix without using any configured service or database."""
import os
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory

from test_schema import ROOT, check, database


def main():
    with TemporaryDirectory(prefix="workflow-e2e-") as temp:
        directory = Path(temp)
        with database("sqlite", directory) as (sql, connection):
            check("sqlite", sql, connection)
        with database("postgres", directory) as (sql, connection):
            check("postgres", sql, connection)
            print("Schema checks passed. Running native e2e and integration tests on SQLite and PostgreSQL.", flush=True)
            subprocess.run(
                ["go", "test", "-tags=e2e", "-race", "-count=1", "./..."],
                cwd=ROOT.parent, check=True,
                env={**os.environ, "WORKFLOW_TEST_POSTGRES_DSN": connection},
            )


if __name__ == "__main__":
    main()

// Command workflow-migrate upgrades the selected store before runners start.
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/ssge/workflow/db"
)

func main() {
	driver := flag.String("driver", "", "sqlite or postgres; set WORKFLOW_DATABASE_CONNECTION to the file path or DSN")
	flag.Parse()
	store, err := db.Open(*driver, os.Getenv("WORKFLOW_DATABASE_CONNECTION"))
	if err != nil {
		log.Fatal(err)
	}
	if err := db.Migrate(context.Background(), store); err != nil {
		log.Fatal(err)
	}
}

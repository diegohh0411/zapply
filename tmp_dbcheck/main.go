package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil { panic(err) }
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `select id, created_at, version, start_date, end_date from evidence
		where form in ('output','recognition') and end_date is not null order by created_at`)
	if err != nil { panic(err) }
	defer rows.Close()
	for rows.Next() {
		var id fmt.Stringer; var ca, s, e any; var v int
		var idb [16]byte
		if err := rows.Scan(&idb, &ca, &v, &s, &e); err != nil { panic(err) }
		_ = id
		fmt.Printf("%x created=%v v=%d start=%v end=%v\n", idb, ca, v, s, e)
	}
}

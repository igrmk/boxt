package main

import "database/sql"

func singleInt(row *sql.Row) (result int) {
	err := row.Scan(&result)
	checkErr(err)
	return result
}

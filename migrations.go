package main

import "database/sql"

var migrations = []func(w *worker){
	func(w *worker) {
		w.mustExec(`
			create table if not exists feedback (
				chat_id integer,
				text text);`)
		w.mustExec(`
			create table if not exists users (
				chat_id integer primary key,
				external_id text not null default '');`)
		w.mustExec(`
			create table if not exists addresses (
				chat_id integer,
				username text not null default '',
				muted integer not null default 0);`)
		w.mustExec(`
			create table if not exists delivered_ids (
				chat_id integer,
				message_id text not null default '')`)
	},
	func(w *worker) {
		w.mustExec("alter table addresses add next_delivery integer not null default 0")
	},
}

func (w *worker) applyMigrations() {
	row := w.db.QueryRow("select version from schema_version")
	var version int
	err := row.Scan(&version)
	if err == sql.ErrNoRows {
		version = -1
		w.mustExec("insert into schema_version(version) values (0)")
	} else {
		checkErr(err)
	}
	for i, m := range migrations[version+1:] {
		n := i + version + 1
		linf("applying migration %d", n)
		m(w)
		w.mustExec("update schema_version set version=?", n)
	}
}

func (w *worker) createDatabase() {
	linf("creating database if needed...")
	w.mustExec(`create table if not exists schema_version (version integer);`)
	w.applyMigrations()
}
